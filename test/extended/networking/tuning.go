package networking

import (
	"context"
	"fmt"
	g "github.com/onsi/ginkgo"
	t "github.com/onsi/ginkgo/extensions/table"
	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	kapiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	frameworkpod "k8s.io/kubernetes/test/e2e/framework/pod"
	"strings"
	"time"
)

const (
	tuningNADName = "tuningnad"
	baseNAD       = "basenad"
	podName       = "testpod"

	sysctlKey  = "net.ipv4.conf.IFNAME.send_redirects"
	sysctlPath = "/proc/sys/net/ipv4/conf/%s/send_redirects"
)

type SysctlVariant struct {
	Sysctl string
	Value  string
	Path   string
}

var whitelistedSysctls = []SysctlVariant{
	{Sysctl: "net.ipv4.conf.IFNAME.send_redirects", Value: "1", Path: "/proc/sys/net/ipv4/conf/net1/send_redirects"},
	{Sysctl: "net.ipv4.conf.IFNAME.accept_source_route", Value: "1", Path: "/proc/sys/net/ipv4/conf/net1/accept_source_route"},
	{Sysctl: "net.ipv6.conf.IFNAME.accept_source_route", Value: "1", Path: "/proc/sys/net/ipv6/conf/net1/accept_source_route"},
	{Sysctl: "net.ipv4.conf.IFNAME.accept_redirects", Value: "1", Path: "/proc/sys/net/ipv4/conf/net1/accept_redirects"},
	{Sysctl: "net.ipv6.conf.IFNAME.accept_redirects", Value: "1", Path: "/proc/sys/net/ipv6/conf/net1/accept_redirects"},
	{Sysctl: "net.ipv4.conf.IFNAME.secure_redirects", Value: "1", Path: "/proc/sys/net/ipv4/conf/net1/secure_redirects"},
	// Bumping the net.ipv6 values for Multiarch until https://bugzilla.redhat.com/show_bug.cgi?id=2082239 is fixed in RHEL
	// uncomment the following two lines once the bug is fixed.
	// {Sysctl: "net.ipv6.neigh.IFNAME.base_reachable_time_ms", Value: "30010", Path: "/proc/sys/net/ipv6/neigh/net1/base_reachable_time_ms"},
	// {Sysctl: "net.ipv6.neigh.IFNAME.retrans_time_ms", Value: "1010", Path: "/proc/sys/net/ipv6/neigh/net1/retrans_time_ms"},
}

// getPodNodeName returns the name of the node the pod is scheduled on
func getPodNodeName(client clientset.Interface, namespace, name string) string {
	pod, err := client.CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	o.Expect(err).NotTo(o.HaveOccurred(), "unable get running pod")
	o.Expect(pod.Spec.NodeName).NotTo(o.BeEmpty(), "expected scheduled pod but found empty Spec.NodeName")
	return pod.Spec.NodeName
}

var _ = g.Describe("[sig-network][Feature:tuning]", func() {
	oc := exutil.NewCLI("tuning")
	f := oc.KubeFramework()

	g.It("pod should start with all sysctl on whitelist", func() {
		namespace := f.Namespace.Name
		sysctls := map[string]string{}
		for _, sysctl := range whitelistedSysctls {
			sysctls[sysctl.Sysctl] = sysctl.Value
		}
		err := createTuningNAD(oc.AdminConfig(), namespace, tuningNADName, sysctls)
		o.Expect(err).NotTo(o.HaveOccurred(), "unable to create network-attachment-definition")

		exutil.CreateExecPodOrFail(f.ClientSet, namespace, podName, func(pod *kapiv1.Pod) {
			pod.ObjectMeta.Annotations = map[string]string{"k8s.v1.cni.cncf.io/networks": fmt.Sprintf("%s/%s", namespace, tuningNADName)}
		})
		for _, sysctl := range whitelistedSysctls {
			result, err := oc.AsAdmin().Run("exec").Args(podName, "-n", namespace, "--", "cat", sysctl.Path).Output()
			o.Expect(err).NotTo(o.HaveOccurred(), "error checking pod sysctl")
			o.Expect(result).To(o.Equal(sysctl.Value), "incorrect sysctl value")
		}
	})
	t.DescribeTable("pod should not start for sysctls not on whitelist", func(sysctl, value string) {
		namespace := f.Namespace.Name
		tuningNADName := "tuningnadwithdisallowedsysctls"
		err := createTuningNAD(oc.AdminConfig(), namespace, tuningNADName, map[string]string{sysctl: value})
		o.Expect(err).NotTo(o.HaveOccurred(), "unable to create network-attachment-definition")

		podDefinition := frameworkpod.NewAgnhostPod(namespace, podName, nil, nil, nil)
		podDefinition.ObjectMeta.Annotations = map[string]string{"k8s.v1.cni.cncf.io/networks": fmt.Sprintf("%s/%s", namespace, tuningNADName)}
		pod := f.PodClient().Create(podDefinition)
		err = frameworkpod.WaitForPodCondition(f.ClientSet, namespace, pod.Name, "Failed", 30*time.Second, func(pod *kapiv1.Pod) (bool, error) {
			if pod.Status.Phase == kapiv1.PodPending {
				return true, nil
			}
			return false, nil
		})
		o.Expect(err).NotTo(o.HaveOccurred(), "incorrect pod status")
		o.Consistently(func() kapiv1.PodPhase {
			pod, err := f.ClientSet.CoreV1().Pods(namespace).Get(context.Background(), podName, metav1.GetOptions{})
			o.Expect(err).ToNot(o.HaveOccurred())
			return pod.Status.Phase
		}, 15*time.Second, 3*time.Second).Should(o.Equal(kapiv1.PodPending))
	},
		t.Entry("net.ipv4.conf.all.send_redirects", "net.ipv4.conf.all.send_redirects", "1"),
		t.Entry("net.ipv4.conf.IFNAME.arp_filter", "net.ipv4.conf.IFNAME.arp_filter", "1"),
	)

	g.It("pod sysctls should not affect node", func() {
		namespace := f.Namespace.Name
		g.By("creating a preexisting pod to check host sysctl")
		nodePod := frameworkpod.CreateExecPodOrFail(f.ClientSet, f.Namespace.Name, "nodeaccess-pod-", func(pod *kapiv1.Pod) {
			pod.Spec.Volumes = []kapiv1.Volume{
				{Name: "sysvolume", VolumeSource: kapiv1.VolumeSource{HostPath: &kapiv1.HostPathVolumeSource{Path: "/sys/class/net"}}},
				{Name: "procvolume", VolumeSource: kapiv1.VolumeSource{HostPath: &kapiv1.HostPathVolumeSource{Path: "/proc"}}},
			}
			pod.Spec.Containers[0].VolumeMounts = []kapiv1.VolumeMount{{Name: "sysvolume", MountPath: "/host/net"}, {Name: "procvolume", MountPath: "/host/proc"}}
			pod.Spec.HostNetwork = true
		})
		testNodeName := getPodNodeName(f.ClientSet, nodePod.Namespace, nodePod.Name)

		const baseNADName string = "basenad"
		const basePodName string = "basepod"
		hostIfName := strings.ReplaceAll(string(uuid.NewUUID()), "-", "")[:14]

		g.By("creating a first network-attachment-definition with a unique host interface name")
		err := createTuningNADWithBridgeName(oc.AdminConfig(), namespace, baseNADName, hostIfName, nil)
		o.Expect(err).NotTo(o.HaveOccurred(), "unable to create first network-attachment-definition")

		g.By("creating a pod using the first network-attachment-definition to ensure the host interface exists")
		exutil.CreateExecPodOrFail(f.ClientSet, namespace, basePodName, func(pod *kapiv1.Pod) {
			pod.ObjectMeta.Annotations = map[string]string{"k8s.v1.cni.cncf.io/networks": fmt.Sprintf("%s/%s", namespace, baseNADName)}
			pod.Spec.NodeName = testNodeName
		})

		g.By("getting the value of the host interface sysctl")
		hostSysctlValue, err := oc.AsAdmin().Run("exec").Args(nodePod.Name, "--", "cat", "/host"+fmt.Sprintf(sysctlPath, hostIfName)).Output()
		o.Expect(err).NotTo(o.HaveOccurred(), "unable to check sysctl")
		sysctlValue := "1"
		if sysctlValue == hostSysctlValue {
			sysctlValue = "0"
		}

		g.By("creating a second network-attachment-definition with sysctl of value other than the host sysctl value")
		testIfName := strings.ReplaceAll(string(uuid.NewUUID()), "-", "")[:14]
		err = createTuningNADWithBridgeName(oc.AdminConfig(), namespace, tuningNADName, testIfName, map[string]string{sysctlKey: sysctlValue})
		o.Expect(err).NotTo(o.HaveOccurred(), "unable to create second network-attachment-definition")

		g.By("creating a pod with the same interface name as the host with a sysctl set")
		exutil.CreateExecPodOrFail(f.ClientSet, namespace, podName, func(pod *kapiv1.Pod) {
			pod.ObjectMeta.Annotations = map[string]string{"k8s.v1.cni.cncf.io/networks": fmt.Sprintf("%s/%s@%s", namespace, tuningNADName, hostIfName)}
			pod.Spec.NodeName = testNodeName
		})

		g.By("checking the value of the sysctl on the pod is that specified in the network-attachment-defintion")
		result, err := oc.AsAdmin().Run("exec").Args(podName, "-n", namespace, "--", "cat", fmt.Sprintf(sysctlPath, hostIfName)).Output()
		o.Expect(err).NotTo(o.HaveOccurred(), "error checking pod sysctls")
		o.Expect(result).To(o.Equal(sysctlValue), "incorrect sysctl value")

		g.By("checking that the value of the node sysctl did not change")
		hostSysctlValue2, err := oc.AsAdmin().Run("exec").Args(nodePod.Name, "--", "cat", "/host"+fmt.Sprintf(sysctlPath, hostIfName)).Output()
		o.Expect(err).NotTo(o.HaveOccurred(), "error checking pod sysctls")
		o.Expect(hostSysctlValue).Should(o.Equal(hostSysctlValue2))
	})

	g.It("pod sysctl should not affect existing pods", func() {
		namespace := f.Namespace.Name
		path := fmt.Sprintf(sysctlPath, "net1")
		err := createNAD(oc.AdminConfig(), namespace, baseNAD)
		o.Expect(err).NotTo(o.HaveOccurred(), "unable to create network-attachment-definition")

		previousPod := frameworkpod.CreateExecPodOrFail(f.ClientSet, f.Namespace.Name, "previous-pod-", func(pod *kapiv1.Pod) {
			pod.ObjectMeta.Annotations = map[string]string{"k8s.v1.cni.cncf.io/networks": fmt.Sprintf("%s/%s", namespace, baseNAD)}
		})
		testNodeName := getPodNodeName(f.ClientSet, previousPod.Namespace, previousPod.Name)

		podOutputBeforeSysctlAplied, err := oc.AsAdmin().Run("exec").Args(previousPod.Name, "--", "cat", path).Output()
		o.Expect(err).NotTo(o.HaveOccurred(), "unable to check sysctl value")
		sysctlValue := "1"
		if sysctlValue == podOutputBeforeSysctlAplied {
			sysctlValue = "0"
		}
		err = createTuningNAD(oc.AdminConfig(), namespace, tuningNADName, map[string]string{sysctlKey: sysctlValue})
		o.Expect(err).NotTo(o.HaveOccurred(), "unable to create network-attachment-definition")

		exutil.CreateExecPodOrFail(f.ClientSet, namespace, podName, func(pod *kapiv1.Pod) {
			pod.ObjectMeta.Annotations = map[string]string{"k8s.v1.cni.cncf.io/networks": fmt.Sprintf("%s/%s", namespace, tuningNADName)}
			pod.Spec.NodeName = testNodeName
		})
		podOutputAfterSysctlAplied, err := oc.AsAdmin().Run("exec").Args(previousPod.Name, "--", "cat", path).Output()
		o.Expect(err).NotTo(o.HaveOccurred(), "unable to check sysctl value")
		o.Expect(podOutputBeforeSysctlAplied).Should(o.Equal(podOutputAfterSysctlAplied))
	})

	g.It("pod sysctl should not affect newly created pods", func() {
		namespace := f.Namespace.Name
		path := fmt.Sprintf(sysctlPath, "net1")

		err := createNAD(oc.AdminConfig(), namespace, baseNAD)
		o.Expect(err).NotTo(o.HaveOccurred(), "unable to create network-attachment-definition")

		previousPod := frameworkpod.CreateExecPodOrFail(f.ClientSet, f.Namespace.Name, "sysctl-pod-", func(pod *kapiv1.Pod) {
			pod.ObjectMeta.Annotations = map[string]string{"k8s.v1.cni.cncf.io/networks": fmt.Sprintf("%s/%s", namespace, baseNAD)}
		})
		testNodeName := getPodNodeName(f.ClientSet, previousPod.Namespace, previousPod.Name)

		podOutputBeforeSysctlAplied, err := oc.AsAdmin().Run("exec").Args(previousPod.Name, "--", "cat", path).Output()
		o.Expect(err).NotTo(o.HaveOccurred(), "unable to check sysctl value")
		sysctlValue := "1"
		if sysctlValue == podOutputBeforeSysctlAplied {
			sysctlValue = "0"
		}
		err = createTuningNAD(oc.AdminConfig(), namespace, tuningNADName, map[string]string{sysctlKey: sysctlValue})
		o.Expect(err).NotTo(o.HaveOccurred(), "unable to create network-attachment-definition")

		exutil.CreateExecPodOrFail(f.ClientSet, namespace, podName, func(pod *kapiv1.Pod) {
			pod.ObjectMeta.Annotations = map[string]string{"k8s.v1.cni.cncf.io/networks": fmt.Sprintf("%s/%s", namespace, tuningNADName)}
			pod.Spec.NodeName = testNodeName

		})
		podOutputAfterSysctlAplied, err := oc.AsAdmin().Run("exec").Args(previousPod.Name, "--", "cat", path).Output()
		o.Expect(err).NotTo(o.HaveOccurred(), "unable to check sysctl value")
		o.Expect(podOutputBeforeSysctlAplied).Should(o.Equal(podOutputAfterSysctlAplied))

		nextPod := frameworkpod.CreateExecPodOrFail(f.ClientSet, f.Namespace.Name, "sysctl-pod-", func(pod *kapiv1.Pod) {
			pod.ObjectMeta.Annotations = map[string]string{"k8s.v1.cni.cncf.io/networks": fmt.Sprintf("%s/%s", namespace, baseNAD)}
			pod.Spec.NodeName = testNodeName
		})
		podOutput, err := oc.AsAdmin().Run("exec").Args(nextPod.Name, "--", "cat", path).Output()
		o.Expect(err).NotTo(o.HaveOccurred(), "unable to check sysctl value")
		o.Expect(podOutput).Should(o.Equal(podOutputBeforeSysctlAplied))
	})

})

func createNAD(config *rest.Config, namespace string, nadName string) error {
	return createTuningNAD(config, namespace, nadName, nil)
}

func createTuningNAD(config *rest.Config, namespace, nadName string, sysctls map[string]string) error {
	return createTuningNADWithBridgeName(config, namespace, nadName, "tunbr", sysctls)
}

func createTuningNADWithBridgeName(config *rest.Config, namespace, nadName, bridgeName string, sysctls map[string]string) error {
	sysctlString := ""
	for sysctl, value := range sysctls {
		if len(sysctlString) > 0 {
			sysctlString = sysctlString + ","
		}
		sysctlString = sysctlString + fmt.Sprintf("\"%s\":\"%s\"", sysctl, value)
	}
	if len(sysctlString) > 0 {
		sysctlString = fmt.Sprintf(`,{"type":"tuning","sysctl":{%s}}`, sysctlString)
	}
	nadConfig := fmt.Sprintf(`{"cniVersion":"0.4.0","name":"%s","plugins":[{"type":"bridge","bridge":"%s","ipam":{"type":"static","addresses":[{"address":"10.10.0.1/24"}]}}%s]}`, nadName, bridgeName, sysctlString)
	return createNetworkAttachmentDefinition(config, namespace, nadName, nadConfig)
}
