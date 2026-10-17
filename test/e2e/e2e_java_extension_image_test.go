//go:build e2e

package e2e

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
)

const (
	javaExtensionTestNS   = "java-extension-test"
	javaExtensionApp      = "java-extension-app"
	javaExtensionAppImage = "java-extension-app:dev"
)

var _ = Describe("Java extension image volume", Ordered, func() {
	BeforeAll(func() {
		if !javaExtensionImageMode {
			Skip("set E2E_JAVA_EXTENSION_IMAGE=true to run against a Kubernetes 1.35+ cluster")
		}
		By("waiting for a previous Java extension test namespace to be removed")
		Expect(deleteNamespace(javaExtensionTestNS)).To(Succeed())
		waitNamespaceDeleted(javaExtensionTestNS)
		Expect(ensureNamespace(javaExtensionTestNS)).To(Succeed())

		By("starting Beyla for the Java workload namespace")
		Expect(deployBeyla(javaExtensionTestNS)).To(Succeed())
		waitBeylaReady()
		Eventually(func(g Gomega) {
			var cms corev1.ConfigMapList
			g.Expect(k8sClient.Resources(ctrlNamespace).List(suiteCtx, &cms,
				resources.WithLabelSelector("beyla.grafana.com/node"))).To(Succeed())
			g.Expect(cms.Items).NotTo(BeEmpty(), "Beyla has not published an injection ConfigMap")
		}, 5*time.Minute, 5*time.Second).Should(Succeed())
	})

	AfterAll(func() {
		if !javaExtensionImageMode {
			return
		}
		tearDownBeyla()
		_ = k8sClient.Resources().Delete(suiteCtx,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: javaExtensionTestNS}})
	})

	SetDefaultEventuallyTimeout(5 * time.Minute)
	SetDefaultEventuallyPollingInterval(5 * time.Second)

	It("injects an extension image into a running Java workload", func() {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: javaExtensionApp, Namespace: javaExtensionTestNS},
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers:    []corev1.Container{{Name: "app", Image: javaExtensionAppImage}},
			},
		}
		By("creating the Java workload after Beyla publishes the namespace selector")
		Expect(k8sClient.Resources().Create(suiteCtx, pod)).To(Succeed())

		By("checking admission attached the configured extension image and agent path")
		var admitted corev1.Pod
		Eventually(func() error {
			return k8sClient.Resources().Get(suiteCtx, pod.Name, pod.Namespace, &admitted)
		}).Should(Succeed())
		Expect(admitted.Annotations).To(HaveKey(injectAnno))
		var extensionVolume *corev1.Volume
		for i := range admitted.Spec.Volumes {
			if admitted.Spec.Volumes[i].Name == "otel-java-extension-0" {
				extensionVolume = &admitted.Spec.Volumes[i]
				break
			}
		}
		Expect(extensionVolume).NotTo(BeNil())
		Expect(extensionVolume.Image).NotTo(BeNil())
		Expect(extensionVolume.Image.Reference).To(Equal(javaExtensionImage))

		var extensionsPath string
		for _, env := range admitted.Spec.Containers[0].Env {
			if env.Name == "OTEL_JAVAAGENT_EXTENSIONS" {
				extensionsPath = env.Value
				break
			}
		}
		Expect(extensionsPath).To(ContainSubstring("/__otel_java_extensions__/0/extensions"))

		By("waiting for the Java workload to start")
		Eventually(func(g Gomega) {
			var running corev1.Pod
			g.Expect(k8sClient.Resources().Get(suiteCtx, pod.Name, pod.Namespace, &running)).To(Succeed())
			g.Expect(podReady(&running)).To(BeTrue(), "Java workload is not ready")
			logs, err := podLogs(javaExtensionTestNS, javaExtensionApp)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(logs).To(ContainSubstring("JAVA_EXTENSION_WORKLOAD_READY"))
			g.Expect(logs).To(ContainSubstring(javaExtensionMarker))
		}, 5*time.Minute, 5*time.Second).Should(Succeed())
	})
})
