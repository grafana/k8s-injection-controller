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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

const (
	// Controller install names track config/default's namePrefix + namespace.
	ctrlNamespace  = "beyla-k8s-injector"
	ctrlDeployment = "beyla-k8s-injector-controller-manager"
	webhookService = "beyla-k8s-injector-webhook-service"
	mutatingWHName = "beyla-k8s-injector-mutating-webhook-configuration"

	// Telemetry backend + workload.
	lgtmNS  = "e2e-lgtm"
	demoNS  = "demo"
	demoApp = "hello-node"

	// The webhook stamps this annotation on instrumented pods.
	injectAnno = "beyla.grafana.com/inject"

	// LGTM's Prometheus as seen from the host (kind extraPortMapping). Use
	// 127.0.0.1, not "localhost": kind/Docker binds the host port on IPv4 only,
	// while "localhost" may resolve to IPv6 ::1 (nothing listens there) and the
	// query fails with "connection refused".
	promBaseURL = "http://127.0.0.1:30090"

	// LGTM's Tempo as seen from the host (kind extraPortMapping). Same 127.0.0.1
	// rationale as promBaseURL. The suite hits Tempo's trace search API here.
	tempoBaseURL = "http://127.0.0.1:30320"
)

var _ = Describe("Telemetry pipeline", Ordered, func() {
	// otel-lgtm (Prometheus + Tempo), the controller, the real Beyla DaemonSet and
	// the demo app are all deployed once in BeforeSuite (see
	// e2e_metrics_suite_test.go); these specs run against that shared deployment.

	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		dumpPodLogs("controller-manager", ctrlNamespace, "control-plane=controller-manager")
		dumpPodLogs("beyla", ctrlNamespace, "app.kubernetes.io/name=beyla")
		dumpPodLogs("demo app", demoNS, "app="+demoApp)
		dumpInjectionConfigMaps()
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	// ---- Stand up Beyla and instrument the demo app (shared prerequisites) ----
	// These ordered specs bring the full pipeline up: backend ready, Beyla
	// publishing its per-node ConfigMap, and the demo app actually instrumented.
	// The per-signal assertions below (metrics, traces) all depend on this.

	It("brings up otel-lgtm", func() {
		By("waiting for the LGTM deployment to become ready")
		waitDeploymentReady("lgtm", lgtmNS, 5*time.Minute)
	})

	It("runs a real Beyla that publishes a per-node injection ConfigMap", func() {
		By("waiting for the Beyla DaemonSet to be ready")
		// grafana/beyla:main is pulled fresh (imagePullPolicy: Always), so allow time.
		Eventually(func(g Gomega) {
			var ds appsv1.DaemonSet
			g.Expect(k8sClient.Resources().Get(suiteCtx, "beyla", ctrlNamespace, &ds)).To(Succeed())
			g.Expect(ds.Status.DesiredNumberScheduled).To(BeNumerically(">", 0), "DaemonSet not scheduled yet")
			g.Expect(ds.Status.NumberReady).To(Equal(ds.Status.DesiredNumberScheduled), "Beyla pods not all ready")
		}, 5*time.Minute, 5*time.Second).Should(Succeed())

		By("waiting until Beyla writes a ConfigMap marked for the injection controller")
		// Beyla stamps the per-node state ConfigMap with the beyla.grafana.com/node
		// label (the same key it uses as the selector annotation), so a
		// label-existence selector reliably finds it.
		Eventually(func(g Gomega) {
			var cms corev1.ConfigMapList
			g.Expect(k8sClient.Resources(ctrlNamespace).List(suiteCtx, &cms,
				resources.WithLabelSelector("beyla.grafana.com/node"))).To(Succeed())
			g.Expect(cms.Items).NotTo(BeEmpty(),
				"Beyla has not published a per-node injection ConfigMap yet")
		}, 5*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("instruments the demo app via the injection controller", func() {
		By("waiting until a demo app pod is instrumented and Ready")
		// A pod that carries the inject annotation AND is Ready proves the SDK
		// image actually pulled, mounted and started — i.e. the full
		// Beyla -> ConfigMap -> controller -> webhook path worked end to end.
		waitInstrumentedReadyPod(demoNS, "app="+demoApp)
	})

	// ---- The instrumented app's metrics reach Prometheus ----
	Context("metrics", func() {
		It("exports the demo app's HTTP metrics to LGTM (queryable via PromQL)", func() {
			By("querying LGTM's Prometheus until the demo app's HTTP server metrics appear")
			// The Node.js SDK (stable HTTP semconv, OTEL_SEMCONV_STABILITY_OPT_IN=http)
			// emits http.server.request.duration, which otel-lgtm ingests as
			// http_server_request_duration_seconds_*. Try a few candidate names so the
			// assertion does not hinge on otel-lgtm's exact OTLP->Prometheus naming.
			Eventually(func(g Gomega) {
				matched, n, err := promHasSeries(promBaseURL,
					`http_server_request_duration_seconds_count`,
					`http_server_request_duration_seconds_bucket`,
					`{__name__=~"http_server_request_duration.*"}`,
					`{__name__=~"http_server_.*"}`,
				)
				g.Expect(err).NotTo(HaveOccurred(), "Prometheus query failed")
				g.Expect(n).To(BeNumerically(">", 0),
					"no HTTP server metrics in LGTM yet (last query tried: %s)", matched)
				_, _ = fmt.Fprintf(GinkgoWriter, "matched %d series with query %q\n", n, matched)
			}, 5*time.Minute, 10*time.Second).Should(Succeed())
		})
	})

	// ---- The instrumented app's traces reach Tempo ----
	Context("traces", func() {
		It("exports the demo app's HTTP traces to Tempo (queryable via TraceQL)", func() {
			By("querying LGTM's Tempo until the demo app's HTTP server spans appear")
			// The injected SDK exports spans over the same OTLP endpoint as metrics
			// (see beyla-config: otel_traces_export), so otel-lgtm's Tempo ingests
			// them. Try a few TraceQL queries so the assertion does not hinge on the
			// exact resource.service.name the injector stamps: prefer the demo app's
			// name, then fall back to any HTTP server span.
			Eventually(func(g Gomega) {
				matched, n, err := tempoHasTraces(tempoBaseURL,
					`{ resource.service.name = "`+demoApp+`" }`,
					`{ resource.service.name =~ "`+demoApp+`.*" }`,
					`{ span.http.request.method != "" }`,
					`{ name =~ "GET.*" }`,
				)
				g.Expect(err).NotTo(HaveOccurred(), "Tempo query failed")
				g.Expect(n).To(BeNumerically(">", 0),
					"no HTTP server traces in Tempo yet (last query tried: %s)", matched)
				_, _ = fmt.Fprintf(GinkgoWriter, "matched %d traces with query %q\n", n, matched)
			}, 5*time.Minute, 10*time.Second).Should(Succeed())
		})
	})
})

// applyManifestFile applies every document in a manifest file, creating each
// object (ignoring ones that already exist).
func applyManifestFile(path string) error {
	return decoder.DecodeEachFile(suiteCtx, os.DirFS(filepath.Dir(path)), filepath.Base(path),
		decoder.CreateIgnoreAlreadyExists(k8sClient.Resources()))
}

// applyKustomization renders a kustomization directory with the kustomize Go API
// (no `kustomize`/`kubectl` binary) and creates the resulting objects.
//
// Admission webhook configurations are created last, mirroring kubectl's apply
// ordering: otherwise the ValidatingWebhookConfiguration can be registered before
// the beyla-sdk-config ConfigMap (same namespace) is created, and that ConfigMap
// write is then rejected (failurePolicy=Fail) because the webhook server is not
// up yet ("connection refused"). Nothing else writes ConfigMaps in that
// namespace until the manager is confirmed ready, so deferring the webhook
// configs is safe.
func applyKustomization(dir string) error {
	resMap, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(filesys.MakeFsOnDisk(), dir)
	if err != nil {
		return fmt.Errorf("kustomize build %s: %w", dir, err)
	}
	rendered, err := resMap.AsYaml()
	if err != nil {
		return fmt.Errorf("rendering kustomize output: %w", err)
	}
	// Two mutations applied to the rendered objects before they are created:
	//   - forceInitContainerMode: the e2e cluster has no ImageVolume feature gate,
	//     so override the SDK config's "auto" injection mode to init_container.
	//   - forceManagerImage: point the manager container at the image this suite
	//     built and loaded into kind. Doing it here (pre-create) rather than via a
	//     post-apply Update means the first and only manager pod already has the
	//     correct image — no ErrImagePull window against config/manager's
	//     committed placeholder tag, and no extra rollout to wait out.
	objs, err := decoder.DecodeAll(suiteCtx, bytes.NewReader(rendered),
		decoder.MutateOption(forceInitContainerMode),
		decoder.MutateOption(forceManagerImage))
	if err != nil {
		return fmt.Errorf("decoding kustomize output: %w", err)
	}

	create := decoder.CreateIgnoreAlreadyExists(k8sClient.Resources())
	var deferred []k8s.Object
	for _, obj := range objs {
		if isAdmissionWebhookConfig(obj) {
			deferred = append(deferred, obj)
			continue
		}
		if err := create(suiteCtx, obj); err != nil {
			return err
		}
	}
	for _, obj := range deferred {
		if err := create(suiteCtx, obj); err != nil {
			return err
		}
	}
	return nil
}

// injectionModeLine matches the top-level `injection_mode:` key in the SDK
// config YAML (anchored at column 0, so commented mentions are ignored).
var injectionModeLine = regexp.MustCompile(`(?m)^injection_mode:.*$`)

// forceInitContainerMode rewrites the controller's SDK config ConfigMap so the
// injector uses init_container mode instead of the ImageVolumeSource. It is a
// decoder MutateFunc applied while rendering config/test; the ConfigMap name
// keeps kustomize's content hash (we only change the data), so the manager
// Deployment still mounts it.
func forceInitContainerMode(obj k8s.Object) error {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok || !strings.Contains(cm.Name, "beyla-sdk-config") {
		return nil
	}
	body, ok := cm.Data["sdk-inject.yaml"]
	if !ok {
		return fmt.Errorf("SDK config ConfigMap %s has no sdk-inject.yaml key", cm.Name)
	}
	if injectionModeLine.MatchString(body) {
		body = injectionModeLine.ReplaceAllString(body, "injection_mode: init_container")
	} else {
		body += "\ninjection_mode: init_container\n"
	}
	cm.Data["sdk-inject.yaml"] = body
	return nil
}

// isAdmissionWebhookConfig reports whether obj is a (Validating|Mutating)
// WebhookConfiguration — the admission registrations that activate the webhook.
func isAdmissionWebhookConfig(obj k8s.Object) bool {
	switch obj.(type) {
	case *admissionregistrationv1.ValidatingWebhookConfiguration,
		*admissionregistrationv1.MutatingWebhookConfiguration:
		return true
	}
	return false
}

// ensureNamespace creates the namespace if it does not already exist.
func ensureNamespace(name string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	return decoder.CreateIgnoreAlreadyExists(k8sClient.Resources())(suiteCtx, ns)
}

// forceManagerImage rewrites the manager container's image to the one this suite
// built and loaded into kind. It is a decoder MutateFunc applied while rendering
// the overlay, so the Deployment is created with the right image up front (the
// committed config/manager tag is a placeholder that is not present in the
// cluster). Setting it pre-create avoids an ErrImagePull window and the extra
// rollout that a post-apply image override would incur.
func forceManagerImage(obj k8s.Object) error {
	dep, ok := obj.(*appsv1.Deployment)
	if !ok || dep.Name != ctrlDeployment {
		return nil
	}
	for i := range dep.Spec.Template.Spec.Containers {
		if dep.Spec.Template.Spec.Containers[i].Name == "manager" {
			dep.Spec.Template.Spec.Containers[i].Image = managerImage
		}
	}
	return nil
}

// waitDeploymentReady blocks until the named Deployment has fully rolled out (all
// replicas updated and ready for the observed generation).
func waitDeploymentReady(name, ns string, timeout time.Duration) {
	Eventually(func(g Gomega) {
		var dep appsv1.Deployment
		g.Expect(k8sClient.Resources().Get(suiteCtx, name, ns, &dep)).To(Succeed())
		g.Expect(dep.Spec.Replicas).NotTo(BeNil())
		g.Expect(dep.Status.ObservedGeneration).To(BeNumerically(">=", dep.Generation), "rollout not observed yet")
		g.Expect(dep.Status.UpdatedReplicas).To(Equal(*dep.Spec.Replicas), "rollout in progress")
		g.Expect(dep.Status.ReadyReplicas).To(Equal(*dep.Spec.Replicas), "not all replicas ready")
	}, timeout, 5*time.Second).Should(Succeed())
}

// waitWebhookReachable blocks until the controller's mutating webhook has its CA
// bundle injected by cert-manager and its serving endpoints are programmed.
func waitWebhookReachable() {
	Eventually(func(g Gomega) {
		var mwc admissionregistrationv1.MutatingWebhookConfiguration
		g.Expect(k8sClient.Resources().Get(suiteCtx, mutatingWHName, "", &mwc)).To(Succeed())
		g.Expect(mwc.Webhooks).NotTo(BeEmpty())
		g.Expect(mwc.Webhooks[0].ClientConfig.CABundle).NotTo(BeEmpty(), "CA bundle not yet injected")
	}, 3*time.Minute, 5*time.Second).Should(Succeed())

	Eventually(func(g Gomega) {
		var slices discoveryv1.EndpointSliceList
		g.Expect(k8sClient.Resources(ctrlNamespace).List(suiteCtx, &slices,
			resources.WithLabelSelector("kubernetes.io/service-name="+webhookService))).To(Succeed())
		addrs := 0
		for _, s := range slices.Items {
			for _, ep := range s.Endpoints {
				addrs += len(ep.Addresses)
			}
		}
		g.Expect(addrs).To(BeNumerically(">", 0), "webhook endpoints not yet ready")
	}, 3*time.Minute, 5*time.Second).Should(Succeed())

	// Give the webhook server a moment to start serving after the endpoint is
	// programmed, so the first ConfigMap apply doesn't race it.
	time.Sleep(5 * time.Second)
}

// waitInstrumentedReadyPod blocks until a pod matching selector in ns is both
// instrumented (carries injectAnno) and Ready, and returns it.
func waitInstrumentedReadyPod(ns, selector string) corev1.Pod {
	var found corev1.Pod
	Eventually(func(g Gomega) {
		var pods corev1.PodList
		g.Expect(k8sClient.Resources(ns).List(suiteCtx, &pods,
			resources.WithLabelSelector(selector))).To(Succeed())
		found = corev1.Pod{}
		for i := range pods.Items {
			p := pods.Items[i]
			if p.Annotations[injectAnno] != "" && podReady(&p) {
				found = p
				break
			}
		}
		g.Expect(found.Name).NotTo(BeEmpty(),
			"no instrumented, ready pod for selector %q in %s yet", selector, ns)
	}, 5*time.Minute, 5*time.Second).Should(Succeed())
	return found
}

// podReady reports whether the pod's Ready condition is True.
func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// promQueryResult is the slice of the Prometheus instant-query API response the
// suite asserts on: just the result vector.
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
// assertion resilient to how otel-lgtm names OTLP-derived series.
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

// tempoSearchResult is the slice of Tempo's /api/search response the suite
// asserts on: just the list of matching traces.
type tempoSearchResult struct {
	Traces []struct {
		TraceID string `json:"traceID"`
	} `json:"traces"`
}

// tempoHasTraces runs each TraceQL query in order against Tempo's instant search
// API and returns the first query that yields at least one trace, plus the
// number of traces it returned. Trying several candidate queries keeps the
// assertion resilient to the exact resource.service.name the injector stamps.
func tempoHasTraces(baseURL string, queries ...string) (string, int, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	last := ""
	for _, q := range queries {
		last = q
		u := baseURL + "/api/search?q=" + url.QueryEscape(q) + "&limit=20"
		resp, err := client.Get(u) //nolint:gosec // test-only request to a local Tempo
		if err != nil {
			return q, 0, err
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return q, 0, err
		}
		if resp.StatusCode != http.StatusOK {
			return q, 0, fmt.Errorf("tempo query %q returned HTTP %d: %s", q, resp.StatusCode, string(body))
		}
		var sr tempoSearchResult
		if err := json.Unmarshal(body, &sr); err != nil {
			return q, 0, fmt.Errorf("decoding tempo response for %q: %w", q, err)
		}
		if len(sr.Traces) > 0 {
			return q, len(sr.Traces), nil
		}
	}
	return last, 0, nil
}

// dumpPodLogs writes the tail of logs for a labeled workload to the Ginkgo
// writer, for failure diagnostics.
func dumpPodLogs(what, ns, selector string) {
	By(fmt.Sprintf("dumping %s diagnostics", what))
	var pods corev1.PodList
	if err := k8sClient.Resources(ns).List(suiteCtx, &pods,
		resources.WithLabelSelector(selector)); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "failed to list %s pods: %s\n", what, err)
		return
	}
	for i := range pods.Items {
		name := pods.Items[i].Name
		stream, err := clientset.CoreV1().Pods(ns).
			GetLogs(name, &corev1.PodLogOptions{TailLines: ptr.To(int64(100))}).Stream(suiteCtx)
		if err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "logs %s/%s: %s\n", ns, name, err)
			continue
		}
		data, _ := io.ReadAll(stream)
		_ = stream.Close()
		_, _ = fmt.Fprintf(GinkgoWriter, "=== %s logs (%s/%s) ===\n%s\n", what, ns, name, data)
	}
}

// dumpInjectionConfigMaps writes the per-node injection ConfigMaps Beyla
// published, for failure diagnostics.
func dumpInjectionConfigMaps() {
	By("dumping per-node injection ConfigMaps")
	var cms corev1.ConfigMapList
	if err := k8sClient.Resources(ctrlNamespace).List(suiteCtx, &cms,
		resources.WithLabelSelector("beyla.grafana.com/node")); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "failed to list injection ConfigMaps: %s\n", err)
		return
	}
	for i := range cms.Items {
		cm := cms.Items[i]
		_, _ = fmt.Fprintf(GinkgoWriter, "=== ConfigMap %s/%s ===\n%v\n", cm.Namespace, cm.Name, cm.Data)
	}
}
