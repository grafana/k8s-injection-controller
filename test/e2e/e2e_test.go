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
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/grafana/beyla-k8s-injector/internal/config"
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
		// deploy-test (config/test overlay) passes a --config that enables the SDK
		// languages (enabled_sdks). Without it EnabledSDKs is empty and the injector
		// blanks every language agent path, so injected pods are never actually
		// instrumented and emit no telemetry.
		cmd = exec.Command("make", "deploy-test", fmt.Sprintf("IMG=%s", managerImage))
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

	// Exercises the full lifecycle Beyla drives through the per-node ConfigMap,
	// end to end against the deployed controller + webhook and with a real
	// auto-instrumentable Node.js workload:
	//   1. the workload is instrumented once a matching ConfigMap appears;
	//   2. the injected annotations + environment variables are verified;
	//   3. the metrics the SDK emits are confirmed queryable in a Grafana
	//      otel-lgtm instance;
	//   4. the workload is uninstrumented once the ConfigMap stops matching.
	// Ordered so the steps share one deployed workload + LGTM and run in order;
	// runs after the readiness checks above so the webhook is known to be serving.
	Context("Injection lifecycle", Ordered, func() {
		const (
			lgtmNS     = "beyla-lgtm-e2e"
			appNS      = "beyla-metrics-e2e"
			appName    = "sample-node-app"
			cmName     = "beyla-node-state"
			injectAnno = "beyla.grafana.com/inject"
			sdkImage   = "ghcr.io/grafana/beyla/inject-sdk-image:0.0.11"
			// NodePort for LGTM's Prometheus; must match the extraPortMapping in
			// test/e2e/kind-config.yaml so the host can reach it.
			lgtmPromNodePort = 30090
			// OTLP/HTTP endpoint the injected SDK exports to, via cluster DNS.
			otlpEndpoint = "http://lgtm." + lgtmNS + ".svc.cluster.local:4318"
			// LGTM's Prometheus as seen from the host (kind extraPortMapping).
			// Use 127.0.0.1, not "localhost": kind/Docker binds the host port on
			// IPv4 only, while "localhost" can resolve to IPv6 ::1 (nothing listens
			// there) and the query fails with "connection refused".
			promBaseURL = "http://127.0.0.1:30090"
		)

		BeforeAll(func() {
			By("creating the LGTM namespace and deploying grafana/otel-lgtm")
			_, err := utils.Run(exec.Command("kubectl", "create", "ns", lgtmNS))
			Expect(err).NotTo(HaveOccurred(), "Failed to create LGTM namespace")
			Expect(kubectlApply(lgtmManifests(lgtmNS, lgtmPromNodePort))).To(Succeed())

			By("creating the workload namespace and deploying the Node.js workload")
			_, err = utils.Run(exec.Command("kubectl", "create", "ns", appNS))
			Expect(err).NotTo(HaveOccurred(), "Failed to create workload namespace")
			Expect(kubectlApply(nodeAppManifests(appNS, appName))).To(Succeed())

			By("waiting for LGTM to become ready")
			Eventually(func(g Gomega) {
				out, err := kubectlOut("get", "deploy", "lgtm", "-n", lgtmNS,
					"-o", "jsonpath={.status.readyReplicas}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("1"), "LGTM not ready yet")
			}, 5*time.Minute, 5*time.Second).Should(Succeed())
		})

		AfterAll(func() {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", appNS,
				"--ignore-not-found", "--wait=false"))
			_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", lgtmNS,
				"--ignore-not-found", "--wait=false"))
		})

		// ---- Step 1: Beyla sends a ConfigMap that instruments the workload ----
		It("instruments the running workload", func() {
			By("applying the Beyla ConfigMap whose criteria select the workload namespace")
			Expect(kubectlApply(
				metricsConfigMap(cmName, appNS, appName, appNS, otlpEndpoint, sdkImage))).To(Succeed())

			By("waiting until an instrumented workload pod is running and ready")
			// The controller rolls the Deployment once it loads the ConfigMap into
			// its registry, so the instrumented pod replaces the initial clean one.
			// Waiting for a pod that both carries the inject annotation and is Ready
			// proves the injected container actually starts (the SDK image is
			// multi-arch, so it pulls and mounts on this node).
			waitInstrumentedReadyPod(appNS, "app="+appName, injectAnno)
		})

		// ---- Step 2: verify the injected pod spec ----
		It("injects the expected SDK annotations and environment variables", func() {
			p := waitInstrumentedReadyPod(appNS, "app="+appName, injectAnno)

			By("checking the inject annotation matches the SDK image package version")
			want := (&config.SDKInject{ImageVolumePath: sdkImage}).PackageVersion()
			Expect(p.Metadata.Annotations[injectAnno]).To(Equal(want),
				"inject annotation should be the SHA-224 of the SDK image reference")

			By("checking the SDK image is mounted as an ImageVolume")
			Expect(p.volumeImage("otel-inject-instrumentation")).To(Equal(sdkImage))

			By("checking the injected environment on the app container")
			env := p.containerEnv("app")
			Expect(env).To(HaveKeyWithValue("LD_PRELOAD",
				"/__otel_sdk_auto_instrumentation__/dist/injector/libotelinject.so"))
			Expect(env).To(HaveKeyWithValue("OTEL_INJECTOR_CONFIG_FILE",
				"/__otel_sdk_auto_instrumentation__/dist/injector/otelinject.conf"))
			Expect(env).To(HaveKeyWithValue("OTEL_EXPORTER_OTLP_ENDPOINT", otlpEndpoint))
			Expect(env).To(HaveKeyWithValue("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf"))
			Expect(env).To(HaveKeyWithValue("OTEL_SEMCONV_STABILITY_OPT_IN", "http"))
			// The ConfigMap enabled metrics and disabled traces/logs.
			Expect(env).To(HaveKeyWithValue("OTEL_METRICS_EXPORTER", "otlp"))
			Expect(env).To(HaveKeyWithValue("OTEL_TRACES_EXPORTER", "none"))
			Expect(env).To(HaveKeyWithValue("OTEL_LOGS_EXPORTER", "none"))
			Expect(env).To(HaveKey("BEYLA_INJECTOR_SDK_PKG_VERSION"))
			Expect(env["BEYLA_INJECTOR_SDK_PKG_VERSION"]).NotTo(BeEmpty())
		})

		// ---- Step 3: verify telemetry actually flows to LGTM ----
		It("exports metrics that become queryable in LGTM", func() {
			By("generating HTTP traffic against the workload so the SDK emits metrics")
			// The load generator lives in the (unselected) LGTM namespace so it is
			// not itself instrumented.
			Expect(kubectlApply(loadGeneratorManifests(lgtmNS, "load-gen",
				fmt.Sprintf("http://%s.%s.svc.cluster.local:8080/", appName, appNS)))).To(Succeed())

			By("querying LGTM's Prometheus until the workload's metrics show up")
			// The SDK maps service.name/service.namespace onto Prometheus job/instance
			// and emits a target_info series; try a few candidate selectors so the
			// assertion does not depend on otel-lgtm's exact OTLP→Prometheus naming.
			Eventually(func(g Gomega) {
				matched, n, err := promHasSeries(promBaseURL,
					`target_info{service_name="`+appName+`"}`,
					`{service_name="`+appName+`"}`,
					`{job=~".*`+appName+`.*"}`,
				)
				g.Expect(err).NotTo(HaveOccurred(), "Prometheus query failed")
				g.Expect(n).To(BeNumerically(">", 0),
					"no metrics for %q in LGTM yet (last query tried: %s)", appName, matched)
			}, 5*time.Minute, 10*time.Second).Should(Succeed())
		})

		// ---- Step 4: Beyla updates the config to exclude the workload ----
		It("uninstruments the workload once the ConfigMap no longer matches", func() {
			// Capture the current rollout marker so we can tell the uninstrument
			// roll apart from the instrument roll triggered in step 1.
			revBefore, _ := restartedAt(appNS, appName)

			By("updating the ConfigMap so the criteria no longer match the workload")
			// The workload stays listed in eligible_for_restart so the controller
			// re-evaluates it and notices it is instrumented-but-unmatched.
			Expect(kubectlApply(
				metricsConfigMap(cmName, appNS, appName, "somewhere-else", otlpEndpoint, sdkImage))).To(Succeed())

			By("asserting the controller rolls the now-unmatched Deployment")
			Eventually(func(g Gomega) {
				rev, err := restartedAt(appNS, appName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(rev).NotTo(BeEmpty(), "Deployment was not rolled for uninstrumentation")
				g.Expect(rev).NotTo(Equal(revBefore), "expected a fresh rollout for uninstrumentation")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("asserting the recreated pods come back without instrumentation")
			Eventually(func(g Gomega) {
				pods := podsDetailed(g, appNS, "app="+appName)
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

// podDetail is the slice of a Pod we assert on: identity/annotations, the
// injected volume + container env, and readiness.
type podDetail struct {
	Metadata struct {
		Name        string            `json:"name"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Volumes []struct {
			Name  string `json:"name"`
			Image *struct {
				Reference string `json:"reference"`
			} `json:"image"`
		} `json:"volumes"`
		Containers []struct {
			Name string `json:"name"`
			Env  []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"env"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

// ready reports whether the pod's Ready condition is True.
func (p podDetail) ready() bool {
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

// volumeImage returns the ImageVolume reference for the named volume, or "".
func (p podDetail) volumeImage(name string) string {
	for _, v := range p.Spec.Volumes {
		if v.Name == name && v.Image != nil {
			return v.Image.Reference
		}
	}
	return ""
}

// containerEnv returns the named container's env as a map (plain values only;
// downward-API valueFrom entries surface as empty strings).
func (p podDetail) containerEnv(name string) map[string]string {
	for _, c := range p.Spec.Containers {
		if c.Name == name {
			m := make(map[string]string, len(c.Env))
			for _, e := range c.Env {
				m[e.Name] = e.Value
			}
			return m
		}
	}
	return nil
}

// podsDetailed lists pods in ns matching the label selector.
func podsDetailed(g Gomega, ns, selector string) []podDetail {
	out, err := kubectlOut("get", "pods", "-n", ns, "-l", selector, "-o", "json")
	g.Expect(err).NotTo(HaveOccurred(), "failed to list pods")
	var list struct {
		Items []podDetail `json:"items"`
	}
	g.Expect(json.Unmarshal([]byte(out), &list)).To(Succeed(), "failed to parse pod list")
	return list.Items
}

// waitInstrumentedReadyPod blocks until a pod matching selector in ns is both
// instrumented (carries injectAnno) and Ready, and returns it. Fails the spec
// if none appears in time.
func waitInstrumentedReadyPod(ns, selector, injectAnno string) podDetail {
	var found podDetail
	Eventually(func(g Gomega) {
		found = podDetail{}
		for _, p := range podsDetailed(g, ns, selector) {
			if p.Metadata.Annotations[injectAnno] != "" && p.ready() {
				found = p
				break
			}
		}
		g.Expect(found.Metadata.Name).NotTo(BeEmpty(),
			"no instrumented, ready pod for selector %q in %s yet", selector, ns)
	}, 5*time.Minute, 5*time.Second).Should(Succeed())
	return found
}

// restartedAt returns the value of the rollout marker the controller stamps on
// a Deployment's pod template when it triggers a (re-)roll. Empty if unset.
func restartedAt(ns, deployment string) (string, error) {
	return kubectlOut("get", "deploy", deployment, "-n", ns,
		"-o", "jsonpath={.spec.template.metadata.annotations.beyla\\.grafana\\.com/restartedAt}")
}

// lgtmManifests renders a single-replica grafana/otel-lgtm Deployment plus a
// Service. The Service is NodePort so the host can reach Prometheus (9090) via
// the kind extraPortMapping, while in-cluster workloads reach the OTLP receiver
// (4318) through the same Service's ClusterIP. No persistent storage — the
// instance is disposable.
func lgtmManifests(ns string, promNodePort int) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: lgtm
  namespace: %[1]s
  labels:
    app: lgtm
spec:
  replicas: 1
  selector:
    matchLabels:
      app: lgtm
  template:
    metadata:
      labels:
        app: lgtm
    spec:
      containers:
        - name: lgtm
          image: grafana/otel-lgtm:0.28.0
          ports:
            - containerPort: 4317
            - containerPort: 4318
            - containerPort: 9090
          readinessProbe:
            httpGet:
              path: /-/ready
              port: 9090
            initialDelaySeconds: 15
            periodSeconds: 5
            failureThreshold: 60
---
apiVersion: v1
kind: Service
metadata:
  name: lgtm
  namespace: %[1]s
spec:
  type: NodePort
  selector:
    app: lgtm
  ports:
    - name: otlp-grpc
      port: 4317
      targetPort: 4317
    - name: otlp-http
      port: 4318
      targetPort: 4318
    - name: prometheus
      port: 9090
      targetPort: 9090
      nodePort: %[2]d
`, ns, promNodePort)
}

// nodeAppManifests renders a minimal Node.js HTTP server plus a Service. We use
// the glibc node:20-slim image on purpose: the injector preloads a glibc
// libotelinject.so via LD_PRELOAD, which a musl (alpine) image would silently
// ignore, leaving the app uninstrumented. The SDK's HTTP instrumentation emits
// metrics once the server starts handling requests.
func nodeAppManifests(ns, name string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[2]s
  namespace: %[1]s
  labels:
    app: %[2]s
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
          image: node:20-slim
          # Export metrics often so the assertion does not wait a full default
          # (60s) collection cycle.
          env:
            - name: OTEL_METRIC_EXPORT_INTERVAL
              value: "5000"
          command:
            - node
            - -e
            - |
              const http = require('http');
              http.createServer((req, res) => {
                res.writeHead(200, {'Content-Type': 'text/plain'});
                res.end('hello\n');
              }).listen(8080, () => console.log('listening on 8080'));
          ports:
            - containerPort: 8080
---
apiVersion: v1
kind: Service
metadata:
  name: %[2]s
  namespace: %[1]s
spec:
  selector:
    app: %[2]s
  ports:
    - port: 8080
      targetPort: 8080
`, ns, name)
}

// loadGeneratorManifests renders a Deployment that curls the target URL in a
// loop, driving the traffic the SDK needs to emit HTTP metrics.
func loadGeneratorManifests(ns, name, targetURL string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[2]s
  namespace: %[1]s
  labels:
    app: %[2]s
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
        - name: load
          image: curlimages/curl:8.11.0
          command:
            - /bin/sh
            - -c
            - "while true; do curl -s -o /dev/null %[3]s || true; sleep 1; done"
`, ns, name, targetURL)
}

// metricsConfigMap renders the per-node ConfigMap Beyla writes: the selection
// criteria + SDK image + OTLP destination under instrumentation.yaml (with
// metric export enabled and traces/logs disabled), and the workload under
// eligible_for_restart.yaml. discoveryNS is the namespace the criterion
// matches: set it to the workload namespace to instrument, elsewhere to
// exclude. The eligible_for_restart entry always names the Deployment so the
// controller re-evaluates it after the criterion stops matching.
func metricsConfigMap(cmName, ns, deployment, discoveryNS, otlpEndpoint, image string) string {
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
    image_volume_path: %[6]s
    otel_export:
      endpoint: %[5]s
      protocol: http/protobuf
    otel_exported_signals:
      metrics: true
      traces: false
      logs: false
  eligible_for_restart.yaml: |
    - namespace: %[2]s
      kind: Deployment
      name: %[3]s
`, cmName, ns, deployment, discoveryNS, otlpEndpoint, image)
}

// promQueryResult is the slice of the Prometheus instant-query API response we
// assert on: just the result vector.
type promQueryResult struct {
	Data struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
		} `json:"result"`
	} `json:"data"`
}

// promHasSeries runs each PromQL query in order against the Prometheus instant
// query API and returns the first query that yields at least one series, plus
// the number of series it returned. Trying several candidate queries keeps the
// assertion resilient to how otel-lgtm names OTLP-derived series and labels.
func promHasSeries(baseURL string, queries ...string) (string, int, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	last := ""
	for _, q := range queries {
		last = q
		u := baseURL + "/api/v1/query?query=" + url.QueryEscape(q)
		resp, err := client.Get(u) //nolint:gosec // test-only request to a local Prometheus
		if err != nil {
			return q, 0, err
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return q, 0, err
		}
		if resp.StatusCode != http.StatusOK {
			return q, 0, fmt.Errorf("prometheus query %q returned HTTP %d: %s", q, resp.StatusCode, string(body))
		}
		var pr promQueryResult
		if err := json.Unmarshal(body, &pr); err != nil {
			return q, 0, fmt.Errorf("decoding prometheus response for %q: %w", q, err)
		}
		if len(pr.Data.Result) > 0 {
			return q, len(pr.Data.Result), nil
		}
	}
	return last, 0, nil
}
