//go:build e2e

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"fmt"
	"io"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
)

const (
	serviceAccountName = "beyla-k8s-injector-controller-manager"
	metricsServiceName = "beyla-k8s-injector-controller-manager-metrics-service"
)

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			if controllerPodName != "" {
				By("Fetching controller manager pod logs")
				if controllerLogs, err := podLogs(ctrlNamespace, controllerPodName); err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
				} else {
					_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
				}
			}

			By("Fetching Kubernetes events")
			if eventsOutput, err := events(ctrlNamespace); err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			if metricsOutput, err := podLogs(ctrlNamespace, "curl-metrics"); err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			if controllerPodName != "" {
				By("Fetching controller manager pod status")
				var pod corev1.Pod
				if err := k8sClient.Resources().Get(suiteCtx, controllerPodName, ctrlNamespace, &pod); err == nil {
					fmt.Printf("Pod status:\n phase=%s\n conditions=%+v\n containerStatuses=%+v\n",
						pod.Status.Phase, pod.Status.Conditions, pod.Status.ContainerStatuses)
				} else {
					fmt.Println("Failed to fetch controller pod status")
				}
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				By("getting the name of the controller-manager pod")
				var pods corev1.PodList
				g.Expect(k8sClient.Resources(ctrlNamespace).List(suiteCtx, &pods,
					resources.WithLabelSelector("control-plane=controller-manager"))).
					To(Succeed(), "Failed to retrieve controller-manager pod information")

				var names []string
				for i := range pods.Items {
					if pods.Items[i].DeletionTimestamp == nil {
						names = append(names, pods.Items[i].Name)
					}
				}
				g.Expect(names).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = names[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				By("validating the pod's status")
				var pod corev1.Pod
				g.Expect(k8sClient.Resources().Get(suiteCtx, controllerPodName, ctrlNamespace, &pod)).To(Succeed())
				g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("validating that the metrics service is available")
			// config/test runs --metrics-secure=false, so the endpoint is plain HTTP
			// on :8443 with no authn/authz filter — no bearer token needed.
			var metricsSvc corev1.Service
			Expect(k8sClient.Resources().Get(suiteCtx, metricsServiceName, ctrlNamespace, &metricsSvc)).
				To(Succeed(), "Metrics service should exist")

			By("ensuring the controller pod is ready")
			verifyControllerPodReady := func(g Gomega) {
				var pod corev1.Pod
				g.Expect(k8sClient.Resources().Get(suiteCtx, controllerPodName, ctrlNamespace, &pod)).To(Succeed())
				g.Expect(podReady(&pod)).To(BeTrue(), "Controller pod not ready")
			}
			Eventually(verifyControllerPodReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				output, err := podLogs(ctrlNamespace, controllerPodName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted, 3*time.Minute, time.Second).Should(Succeed())

			By("waiting for the webhook service endpoints to be ready")
			verifyWebhookEndpointsReady := func(g Gomega) {
				var slices discoveryv1.EndpointSliceList
				g.Expect(k8sClient.Resources(ctrlNamespace).List(suiteCtx, &slices,
					resources.WithLabelSelector("kubernetes.io/service-name="+webhookService))).
					To(Succeed(), "Webhook endpoints should exist")
				var addresses []string
				for _, slice := range slices.Items {
					for _, ep := range slice.Endpoints {
						addresses = append(addresses, ep.Addresses...)
					}
				}
				g.Expect(addresses).ShouldNot(BeEmpty(), "Webhook endpoints not yet ready")
			}
			Eventually(verifyWebhookEndpointsReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying the mutating webhook server is ready")
			verifyMutatingWebhookReady := func(g Gomega) {
				caBundle, err := mutatingWebhookCABundle()
				g.Expect(err).NotTo(HaveOccurred(), "MutatingWebhookConfiguration should exist")
				g.Expect(caBundle).ShouldNot(BeEmpty(), "Mutating webhook CA bundle not yet injected")
			}
			Eventually(verifyMutatingWebhookReady, 3*time.Minute, time.Second).Should(Succeed())

			// BeforeSuite already ran waitWebhookReachable with a real probe,
			// so no extra stabilize sleep is needed here.

			// +kubebuilder:scaffold:e2e-webhooks-readiness

			By("creating the curl-metrics pod to access the metrics endpoint")
			// Pod retries to ride out the metrics server warming up.
			curlScript := fmt.Sprintf(
				"for i in $(seq 1 30); do curl -v "+
					"http://%s.%s.svc.cluster.local:8443/metrics && exit 0 || sleep 2; done; exit 1",
				metricsServiceName, ctrlNamespace)
			curlPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "curl-metrics", Namespace: ctrlNamespace},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: serviceAccountName,
					Containers: []corev1.Container{{
						Name:    "curl",
						Image:   "curlimages/curl:8.11.0",
						Command: []string{"/bin/sh", "-c"},
						Args:    []string{curlScript},
						SecurityContext: &corev1.SecurityContext{
							ReadOnlyRootFilesystem:   ptr.To(true),
							AllowPrivilegeEscalation: ptr.To(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							RunAsNonRoot:             ptr.To(true),
							RunAsUser:                ptr.To(int64(1000)),
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
					}},
				},
			}
			Expect(k8sClient.Resources().Create(suiteCtx, curlPod)).To(Succeed(), "Failed to create curl-metrics pod")
			DeferCleanup(func() {
				_ = k8sClient.Resources().Delete(suiteCtx,
					&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "curl-metrics", Namespace: ctrlNamespace}})
			})

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				var pod corev1.Pod
				g.Expect(k8sClient.Resources().Get(suiteCtx, "curl-metrics", ctrlNamespace, &pod)).To(Succeed())
				g.Expect(pod.Status.Phase).To(Equal(corev1.PodSucceeded), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			verifyMetricsAvailable := func(g Gomega) {
				metricsOutput, err := getMetricsOutput()
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
				g.Expect(metricsOutput).NotTo(BeEmpty())
				g.Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
			}
			Eventually(verifyMetricsAvailable, 2*time.Minute).Should(Succeed())
		})

		It("provisions a populated webhook serving certificate", func() {
			// Mode-agnostic: cert-manager issues the serving cert into its Secret,
			// while the self-signed rotator generates one and writes it into the
			// pre-created Secret. Either way the Secret must exist and carry a
			// tls.crt. The Secret name differs per mode (servingCertSecret).
			By("validating the serving-cert Secret exists and carries a cert")
			verifyServingCert := func(g Gomega) {
				var secret corev1.Secret
				g.Expect(k8sClient.Resources().Get(suiteCtx, servingCertSecret, ctrlNamespace, &secret)).To(Succeed())
				g.Expect(secret.Data).To(HaveKey("tls.crt"))
				g.Expect(secret.Data["tls.crt"]).NotTo(BeEmpty())
			}
			Eventually(verifyServingCert).Should(Succeed())
		})

		It("should have CA injection for mutating webhooks", func() {
			By("checking CA injection for mutating webhooks")
			verifyCAInjection := func(g Gomega) {
				caBundle, err := mutatingWebhookCABundle()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(caBundle)).To(BeNumerically(">", 10))
			}
			Eventually(verifyCAInjection).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks
	})

	// Exercises the full instrument → re-configure → uninstrument lifecycle the
	// way Beyla drives it via the per-node ConfigMap.
	Context("Injection lifecycle", func() {
		const (
			workloadNS = "beyla-inject-e2e"
			workload   = "sample-app"
			cmName     = "beyla-node-state"
		)

		It("instruments a matching workload and uninstruments it once the config excludes it", func() {
			By("creating an isolated namespace for the sample workload")
			Expect(k8sClient.Resources().Create(suiteCtx,
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: workloadNS}})).
				To(Succeed(), "Failed to create workload namespace")
			DeferCleanup(func() {
				_ = k8sClient.Resources().Delete(suiteCtx,
					&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: workloadNS}})
			})

			By("deploying the sample workload")
			Expect(k8sClient.Resources().Create(suiteCtx, sampleDeployment(workloadNS, workload))).To(Succeed())

			By("granting Beyla's ServiceAccount RBAC to write injection ConfigMaps")
			Expect(k8sClient.Resources().Create(suiteCtx, beylaWriterRole(ctrlNamespace))).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Resources().Delete(suiteCtx, beylaWriterRole(ctrlNamespace)) })
			Expect(k8sClient.Resources().Create(suiteCtx, beylaWriterRoleBinding(ctrlNamespace, allowedConfigMapWriter))).
				To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Resources().Delete(suiteCtx, beylaWriterRoleBinding(ctrlNamespace, allowedConfigMapWriter))
			})

			// ---- Step 1: Beyla sends a ConfigMap that instruments the workload ----
			By("asserting an unauthorized identity cannot write an injection ConfigMap")
			// kind-admin is neither on ALLOWED_CONFIGMAP_WRITERS nor in system:masters
			// here, so the validating webhook rejects it. Retry through the
			// CA-injection window — until the apiserver can reach the webhook, the
			// error is "webhook unreachable" rather than "not authorized".
			Eventually(func(g Gomega) {
				err := k8sClient.Resources().Create(suiteCtx,
					selectorConfigMap(cmName, ctrlNamespace, workloadNS, workload, workloadNS, sdkImageVersion))
				g.Expect(err).To(HaveOccurred(), "expected the validating webhook to deny the unauthorized write")
				g.Expect(err.Error()).To(ContainSubstring("not authorized"))
			}, time.Minute, 5*time.Second).Should(Succeed())

			By("applying the Beyla ConfigMap whose criteria select the workload namespace")
			// The ConfigMap lives in the controller's own namespace; criteria
			// target workloadNS. applyConfigMap uses Beyla's impersonated SA so
			// the validating webhook approves via the allowlist.
			Eventually(func(g Gomega) {
				g.Expect(applyConfigMap(
					selectorConfigMap(cmName, ctrlNamespace, workloadNS, workload, workloadNS, sdkImageVersion))).
					To(Succeed())
			}, time.Minute, 5*time.Second).Should(Succeed())

			By("waiting until the workload pod is instrumented by the webhook")
			// Controller loads the ConfigMap asynchronously, so an early-admitted
			// pod comes up clean. Deleting it lets the ReplicaSet recreate it
			// once the registry is populated.
			Eventually(func(g Gomega) {
				pods := podsWithLabel(g, workloadNS, "app="+workload)
				g.Expect(pods).NotTo(BeEmpty(), "no workload pods yet")
				p := pods[0]
				if p.Annotations[injectAnno] == "" {
					_ = k8sClient.Resources().Delete(suiteCtx, &p)
				}
				g.Expect(p.Annotations).To(HaveKey(injectAnno),
					"workload pod was not instrumented")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			// Capture the current rollout marker so the uninstrument roll in
			// step 3 is distinguishable from any roll the controller did to
			// instrument pre-existing pods.
			revBefore, _ := restartedAt(workloadNS, workload)

			// ---- Step 2: Beyla updates the config to exclude the workload ----
			By("updating the ConfigMap so the criteria no longer match the workload")
			// Workload stays in eligible_for_restart so the controller re-evaluates
			// it and sees instrumented-but-unmatched.
			Eventually(func(g Gomega) {
				g.Expect(applyConfigMap(
					selectorConfigMap(cmName, ctrlNamespace, workloadNS, workload, "somewhere-else", sdkImageVersion))).
					To(Succeed())
			}, time.Minute, 5*time.Second).Should(Succeed())

			// ---- Step 3: the workload gets uninstrumented ----
			By("asserting the controller rolls the now-unmatched Deployment")
			Eventually(func(g Gomega) {
				rev, err := restartedAt(workloadNS, workload)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(rev).NotTo(BeEmpty(), "Deployment was not rolled for uninstrumentation")
				g.Expect(rev).NotTo(Equal(revBefore), "expected a fresh rollout for uninstrumentation")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("asserting the recreated pods come back without instrumentation")
			Eventually(func(g Gomega) {
				pods := podsWithLabel(g, workloadNS, "app="+workload)
				g.Expect(pods).NotTo(BeEmpty())
				for _, p := range pods {
					g.Expect(p.Annotations).NotTo(HaveKey(injectAnno),
						"pod %s is still instrumented after the config excluded it", p.Name)
				}
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})
	})
})

// getMetricsOutput returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() (string, error) {
	return podLogs(ctrlNamespace, "curl-metrics")
}

// podLogs streams the logs of a pod's first container.
func podLogs(ns, name string) (string, error) {
	stream, err := clientset.CoreV1().Pods(ns).GetLogs(name, &corev1.PodLogOptions{}).Stream(suiteCtx)
	if err != nil {
		return "", err
	}
	defer func() { _ = stream.Close() }()
	data, err := io.ReadAll(stream)
	return string(data), err
}

// events renders the namespace's events, newest involved-object info last, for
// failure diagnostics.
func events(ns string) (string, error) {
	list, err := clientset.CoreV1().Events(ns).List(suiteCtx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, e := range list.Items {
		_, _ = fmt.Fprintf(&b, "%s\t%s\t%s/%s\t%s\n",
			e.LastTimestamp, e.Type, e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Message)
	}
	return b.String(), nil
}

// mutatingWebhookCABundle returns the CA bundle injected into the controller's
// MutatingWebhookConfiguration (empty until cert-manager's ca-injector fills it).
func mutatingWebhookCABundle() ([]byte, error) {
	var mwc admissionregistrationv1.MutatingWebhookConfiguration
	if err := k8sClient.Resources().Get(suiteCtx, mutatingWHName, "", &mwc); err != nil {
		return nil, err
	}
	var bundle []byte
	for _, w := range mwc.Webhooks {
		bundle = append(bundle, w.ClientConfig.CABundle...)
	}
	return bundle, nil
}

// podsWithLabel lists pods in ns matching the label selector.
func podsWithLabel(g Gomega, ns, selector string) []corev1.Pod {
	var list corev1.PodList
	g.Expect(k8sClient.Resources(ns).List(suiteCtx, &list,
		resources.WithLabelSelector(selector))).To(Succeed(), "failed to list pods")
	return list.Items
}

// restartedAt returns the value of the rollout marker the controller stamps on
// a Deployment's pod template when it triggers a (re-)roll. Empty if unset.
func restartedAt(ns, deployment string) (string, error) {
	var d appsv1.Deployment
	if err := k8sClient.Resources().Get(suiteCtx, deployment, ns, &d); err != nil {
		return "", err
	}
	return d.Spec.Template.Annotations["beyla.grafana.com/restartedAt"], nil
}

// applyConfigMap upserts cm as Beyla's impersonated ServiceAccount, so the
// write is admitted by the validating webhook the same way it would be in
// production.
func applyConfigMap(cm *corev1.ConfigMap) error {
	cms := beylaClientset.CoreV1().ConfigMaps(cm.Namespace)
	existing, err := cms.Get(suiteCtx, cm.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = cms.Create(suiteCtx, cm, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	existing.Annotations = cm.Annotations
	existing.Data = cm.Data
	_, err = cms.Update(suiteCtx, existing, metav1.UpdateOptions{})
	return err
}

// sampleDeployment renders a minimal pause-image workload that reaches Ready
// regardless of injection state.
func sampleDeployment(ns, name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "app",
						Image: "registry.k8s.io/pause:3.10",
					}},
				},
			},
		},
	}
}

// beylaWriterRole grants the verbs Beyla needs to upsert its per-node state
// ConfigMaps. Production ships this RBAC with Beyla; the e2e recreates it so
// the impersonated writes are authorized.
func beylaWriterRole(ns string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "beyla-configmap-writer", Namespace: ns},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"configmaps"},
			Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
		}},
	}
}

// beylaWriterRoleBinding binds beylaWriterRole to the impersonated Beyla
// identity (a User subject, since the suite impersonates it by username).
func beylaWriterRoleBinding(ns, user string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "beyla-configmap-writer", Namespace: ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "beyla-configmap-writer"},
		Subjects:   []rbacv1.Subject{{Kind: "User", Name: user}},
	}
}

// selectorConfigMap renders the per-node ConfigMap Beyla writes. discoveryNS
// is what the rule's k8s_selector matches (use the workload namespace to
// instrument, anything else to exclude); the eligible_for_restart entry
// always names the Deployment so the controller re-evaluates after a rule
// stops matching.
func selectorConfigMap(cmName, cmNamespace, targetNS, deployment, discoveryNS, imageVersion string) *corev1.ConfigMap {
	instrumentation := fmt.Sprintf(`image_version: %s
rules:
- k8s_selector:
    namespaces:
    - %s
`, imageVersion, discoveryNS)

	eligible := fmt.Sprintf(`- namespace: %s
  kind: Deployment
  name: %s
`, targetNS, deployment)

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        cmName,
			Namespace:   cmNamespace,
			Annotations: map[string]string{"beyla.grafana.com/node": ""},
		},
		Data: map[string]string{
			"instrumentation.yaml":      instrumentation,
			"eligible_for_restart.yaml": eligible,
		},
	}
}
