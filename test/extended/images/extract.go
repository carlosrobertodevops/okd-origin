package images

import (
	"context"
	"fmt"
	"strings"

	"github.com/MakeNowJust/heredoc"
	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"

	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8simage "k8s.io/kubernetes/test/utils/image"

	imageapi "github.com/openshift/api/image/v1"
	imageclientset "github.com/openshift/client-go/image/clientset/versioned"
	exutil "github.com/openshift/origin/test/extended/util"
)

var _ = g.Describe("[sig-imageregistry][Feature:ImageExtract] Image extract", func() {
	defer g.GinkgoRecover()

	var oc *exutil.CLI
	var ns string

	g.AfterEach(func() {
		if g.CurrentGinkgoTestDescription().Failed && len(ns) > 0 {
			exutil.DumpPodLogsStartingWithInNamespace("", ns, oc)
		}
	})

	oc = exutil.NewCLI("image-extract")

	g.It("should extract content from an image", func() {
		is, err := oc.ImageClient().ImageV1().ImageStreams("openshift").Get(context.Background(), "tools", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(is.Status.DockerImageRepository).NotTo(o.BeEmpty(), "registry not yet configured?")
		registry := strings.Split(is.Status.DockerImageRepository, "/")[0]

		ns = oc.Namespace()
		cli := oc.KubeFramework().PodClient()
		client := imageclientset.NewForConfigOrDie(oc.UserConfig()).ImageV1()

		// import tools:latest into this namespace - working around a pull through bug with referenced docker images
		// https://bugzilla.redhat.com/show_bug.cgi?id=1843253
		_, err = client.ImageStreamTags(ns).Create(context.Background(), &imageapi.ImageStreamTag{
			ObjectMeta: metav1.ObjectMeta{Name: "1:tools"},
			Tag: &imageapi.TagReference{
				From: &kapi.ObjectReference{Kind: "ImageStreamTag", Namespace: "openshift", Name: "tools:latest"},
			},
		}, metav1.CreateOptions{})
		o.Expect(err).ToNot(o.HaveOccurred())

		isi, err := client.ImageStreamImports(ns).Create(context.Background(), &imageapi.ImageStreamImport{
			ObjectMeta: metav1.ObjectMeta{
				Name: "1",
			},
			Spec: imageapi.ImageStreamImportSpec{
				Import: true,
				Images: []imageapi.ImageImportSpec{
					{
						From: kapi.ObjectReference{Kind: "DockerImage", Name: k8simage.GetE2EImage(k8simage.Agnhost)},
						To:   &kapi.LocalObjectReference{Name: "mysql"},
					},
				},
			},
		}, metav1.CreateOptions{})
		o.Expect(err).ToNot(o.HaveOccurred())

		for i, img := range isi.Status.Images {
			o.Expect(img.Status.Status).To(o.Equal("Success"), fmt.Sprintf("imagestreamimport status for spec.image[%d] (message: %s)", i, img.Status.Message))
		}

		// toolsLayers := isi.Status.Images[0].Image.DockerImageLayers
		// toolsLen := len(toolsLayers)
		// mysqlLayers := isi.Status.Images[1].Image.DockerImageLayers
		// mysqlLen := len(mysqlLayers)

		pod := cli.Create(cliPodWithPullSecret(oc, heredoc.Docf(`
			set -x

			# command exits if directory doesn't exist
			! oc image extract --insecure %[2]s/%[1]s/1:tools --path=/:/tmp/doesnotexist
			# command exits if directory isn't empty
			! oc image extract --insecure %[2]s/%[1]s/1:tools --path=/:/

			# extract a directory to a directory, verify the contents
			mkdir -p /tmp/test
			oc image extract --insecure %[2]s/%[1]s/1:tools --path=/etc/cron.d/:/tmp/test/
			[ -f /tmp/test/0hourly ] && grep root /tmp/test/0hourly

			# extract multiple individual files
			mkdir -p /tmp/test2
			oc image extract --insecure %[2]s/%[1]s/1:tools --path=/etc/shadow:/tmp/test2 --path=/etc/system-release:/tmp/test2
			[ -f /tmp/test2/shadow ] && [ -L /tmp/test2/system-release ]

			# extract a single file to the current directory
			mkdir -p /tmp/test3
			cd /tmp/test3
			oc image extract --insecure %[2]s/%[1]s/1:tools --file=/etc/shadow
			[ -f /tmp/test3/shadow ]
		`, ns, registry)))
		cli.WaitForSuccess(pod.Name, podStartupTimeout)
	})
})
