//go:build e2e
// +build e2e

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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/grafana/beyla-k8s-injector/test/utils"
)

// These names track the kustomize namePrefix (config/default/kustomization.yaml)
// and namespace (config/default/kustomization.yaml). If you change either,
// update these to match.
const namespace = "beyla-k8s-injector"
const serviceAccountName = "beyla-k8s-injector-controller-manager"
const metricsServiceName = "beyla-k8s-injector-controller-manager-metrics-service"
const metricsRoleBindingName = "beyla-k8s-injector-metrics-binding"
const metricsReaderRoleName = "beyla-k8s-injector-metrics-reader"
const webhookServiceName = "beyla-k8s-injector-webhook-service"
const mutatingWebhookConfigName = "beyla-k8s-injector-mutating-webhook-configuration"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
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
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				By("validating the pod's status")
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole="+metricsReaderRoleName,
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("ensuring the controller pod is ready")
			verifyControllerPodReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", controllerPodName, "-n", namespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "Controller pod not ready")
			}
			Eventually(verifyControllerPodReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted, 3*time.Minute, time.Second).Should(Succeed())

			By("waiting for the webhook service endpoints to be ready")
			verifyWebhookEndpointsReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "endpointslices.discovery.k8s.io", "-n", namespace,
					"-l", "kubernetes.io/service-name="+webhookServiceName,
					"-o", "jsonpath={range .items[*]}{range .endpoints[*]}{.addresses[*]}{end}{end}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Webhook endpoints should exist")
				g.Expect(output).ShouldNot(BeEmpty(), "Webhook endpoints not yet ready")
			}
			Eventually(verifyWebhookEndpointsReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying the mutating webhook server is ready")
			verifyMutatingWebhookReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "mutatingwebhookconfigurations.admissionregistration.k8s.io",
					"beyla-k8s-injector-mutating-webhook-configuration",
					"-o", "jsonpath={.webhooks[0].clientConfig.caBundle}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "MutatingWebhookConfiguration should exist")
				g.Expect(output).ShouldNot(BeEmpty(), "Mutating webhook CA bundle not yet injected")
			}
			Eventually(verifyMutatingWebhookReady, 3*time.Minute, time.Second).Should(Succeed())

			By("waiting additional time for webhook server to stabilize")
			time.Sleep(5 * time.Second)

			// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": [
								"for i in $(seq 1 30); do curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics && exit 0 || sleep 2; done; exit 1"
							],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccountName": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
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

		It("should provisioned cert-manager", func() {
			By("validating that cert-manager has the certificate Secret")
			verifyCertManager := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "secrets", "webhook-server-cert", "-n", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}
			Eventually(verifyCertManager).Should(Succeed())
		})

		It("should have CA injection for mutating webhooks", func() {
			By("checking CA injection for mutating webhooks")
			verifyCAInjection := func(g Gomega) {
				cmd := exec.Command("kubectl", "get",
					"mutatingwebhookconfigurations.admissionregistration.k8s.io",
					"beyla-k8s-injector-mutating-webhook-configuration",
					"-o", "go-template={{ range .webhooks }}{{ .clientConfig.caBundle }}{{ end }}")
				mwhOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(mwhOutput)).To(BeNumerically(">", 10))
			}
			Eventually(verifyCAInjection).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks

		// TODO: Customize the e2e test suite with scenarios specific to your project.
		// Consider applying sample/CR(s) and check their status and/or verifying
		// the reconciliation by using the metrics, i.e.:
		// metricsOutput, err := getMetricsOutput()
		// Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
		// Expect(metricsOutput).To(ContainSubstring(
		//    fmt.Sprintf(`controller_runtime_reconcile_total{controller="%s",result="success"} 1`,
		//    strings.ToLower(<Kind>),
		// ))
	})

	// Exercises the full instrument → re-configure → uninstrument lifecycle
	// against the deployed controller + webhook, the way Beyla drives it via the
	// per-node ConfigMap. Runs after the readiness checks above so the webhook is
	// known to be serving.
	Context("Injection lifecycle", func() {
		const (
			workloadNS = "beyla-inject-e2e"
			workload   = "sample-app"
			cmName     = "beyla-node-state"
			injectAnno = "beyla.grafana.com/inject"
			// A valid OCI reference the apiserver accepts as an ImageVolumeSource.
			// These assertions are spec-level (the inject annotation), so the pod
			// does not need to actually pull/run this image.
			sdkImage = "ghcr.io/grafana/beyla/inject-sdk-image:0.0.9"
		)

		It("instruments a matching workload and uninstruments it once the config excludes it", func() {
			By("creating an isolated namespace for the sample workload")
			_, err := utils.Run(exec.Command("kubectl", "create", "ns", workloadNS))
			Expect(err).NotTo(HaveOccurred(), "Failed to create workload namespace")
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", workloadNS,
					"--ignore-not-found", "--wait=false"))
			})

			By("deploying the sample workload")
			Expect(kubectlApply(sampleDeployment(workloadNS, workload))).To(Succeed())

			// ---- Step 1: Beyla sends a ConfigMap that instruments the workload ----
			By("applying the Beyla ConfigMap whose criteria select the workload namespace")
			Expect(kubectlApply(selectorConfigMap(cmName, workloadNS, workload, workloadNS, sdkImage))).
				To(Succeed())

			By("waiting until the workload pod is instrumented by the webhook")
			// The controller loads the ConfigMap into its in-memory registry
			// asynchronously, so a pod admitted before that comes up clean. Deleting
			// such a pod lets the ReplicaSet recreate it; the loop converges once the
			// registry is populated (or the controller's own rollout sweep fires).
			Eventually(func(g Gomega) {
				pods := podsWithLabel(g, workloadNS, "app="+workload)
				g.Expect(pods).NotTo(BeEmpty(), "no workload pods yet")
				p := pods[0]
				if p.Metadata.Annotations[injectAnno] == "" {
					_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", p.Metadata.Name,
						"-n", workloadNS, "--ignore-not-found"))
				}
				g.Expect(p.Metadata.Annotations).To(HaveKey(injectAnno),
					"workload pod was not instrumented")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			// The controller may instrument pre-existing pods by rolling the
			// Deployment, so capture the current rollout marker to tell that roll
			// apart from the uninstrument roll triggered in step 3.
			revBefore, _ := restartedAt(workloadNS, workload)

			// ---- Step 2: Beyla updates the config to exclude the workload ----
			By("updating the ConfigMap so the criteria no longer match the workload")
			// The workload stays listed in eligible_for_restart so the controller
			// re-evaluates it and notices it is instrumented-but-unmatched.
			Expect(kubectlApply(selectorConfigMap(cmName, workloadNS, workload, "somewhere-else", sdkImage))).
				To(Succeed())

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
					g.Expect(p.Metadata.Annotations).NotTo(HaveKey(injectAnno),
						"pod %s is still instrumented after the config excluded it", p.Metadata.Name)
				}
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})
	})
})

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	By("creating temporary file to store the token request")
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		By("executing kubectl command to create the token")
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		By("parsing the JSON output to extract the token")
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	return utils.Run(cmd)
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}

// kubectlApply pipes a manifest to `kubectl apply -f -`.
func kubectlApply(manifest string) error {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)
	return err
}

// kubectlOut runs kubectl and returns stdout only, so JSON/JSONPath output is
// not polluted by warnings the CLI writes to stderr.
func kubectlOut(args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	out, err := cmd.Output()
	return string(out), err
}

// podSummary is the slice of a Pod we assert on: its name and annotations.
type podSummary struct {
	Metadata struct {
		Name        string            `json:"name"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
}

// podsWithLabel lists pods in ns matching the label selector.
func podsWithLabel(g Gomega, ns, selector string) []podSummary {
	out, err := kubectlOut("get", "pods", "-n", ns, "-l", selector, "-o", "json")
	g.Expect(err).NotTo(HaveOccurred(), "failed to list pods")
	var list struct {
		Items []podSummary `json:"items"`
	}
	g.Expect(json.Unmarshal([]byte(out), &list)).To(Succeed(), "failed to parse pod list")
	return list.Items
}

// restartedAt returns the value of the rollout marker the controller stamps on
// a Deployment's pod template when it triggers a (re-)roll. Empty if unset.
func restartedAt(ns, deployment string) (string, error) {
	return kubectlOut("get", "deploy", deployment, "-n", ns,
		"-o", "jsonpath={.spec.template.metadata.annotations.beyla\\.grafana\\.com/restartedAt}")
}

// sampleDeployment renders a minimal workload. The pause image runs without a
// shell and ignores the env/volume the webhook injects, so the pod reaches
// Ready and rolling updates complete regardless of injection state.
func sampleDeployment(ns, name string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[2]s
  namespace: %[1]s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: %[2]s
  template:
    metadata:
      labels:
        app: %[2]s
    spec:
      containers:
        - name: app
          image: registry.k8s.io/pause:3.10
`, ns, name)
}

// selectorConfigMap renders the per-node ConfigMap Beyla writes: the selection
// criteria + SDK image under instrumentation.yaml, and the workload under
// eligible_for_restart.yaml. discoveryNS is the namespace the criterion matches
// (set it to the workload namespace to instrument, elsewhere to exclude); the
// eligible_for_restart entry always names the Deployment so the controller
// re-evaluates it after the criterion stops matching.
func selectorConfigMap(cmName, targetNS, deployment, discoveryNS, image string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %[1]s
  namespace: %[2]s
  annotations:
    beyla.grafana.com/node: ""
data:
  instrumentation.yaml: |
    discovery:
      - k8s_namespace: %[4]s
    image_volume_path: %[5]s
    otel_export:
      endpoint: http://otel-collector:4318
      protocol: http/protobuf
  eligible_for_restart.yaml: |
    - namespace: %[2]s
      kind: Deployment
      name: %[3]s
`, cmName, targetNS, deployment, discoveryNS, image)
}
