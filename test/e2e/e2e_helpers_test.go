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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	// Telemetry backend.
	lgtmNS = "e2e-lgtm"

	// The webhook stamps this annotation on instrumented pods.
	injectAnno = "beyla.grafana.com/inject"

	// sdkImageVersion is the inject-sdk-image version the controller injects
	// (config/test/sdk-inject.yaml). Used when crafting manual injection
	// ConfigMaps in the injection lifecycle and safety tests.
	sdkImageVersion = "0.0.13"

	// LGTM's Tempo as seen from the host (kind extraPortMapping). Use
	// 127.0.0.1, not "localhost": kind/Docker binds the host port on IPv4 only,
	// while "localhost" may resolve to IPv6 ::1 (nothing listens there) and the
	// query fails with "connection refused".
	tempoBaseURL = "http://127.0.0.1:30320"
)

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
	// Force init_container injection mode (rather than the ImageVolumeSource the
	// config/test overlay would pick via "auto"): the e2e cluster has no
	// ImageVolume feature gate, and init_container works on any node. This is
	// scoped to the suite — config/test (used by `make demo-deploy`) is untouched.
	objs, err := decoder.DecodeAll(suiteCtx, bytes.NewReader(rendered),
		decoder.MutateOption(forceInitContainerMode))
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

// waitNamespaceDeleted blocks until the named namespace no longer exists (HTTP
// 404) or the timeout fires. It specifically requires a not-found response so
// transient errors such as connection refused do not prematurely unblock callers
// and mask cluster instability.
func waitNamespaceDeleted(name string, timeout time.Duration) {
	Eventually(func(g Gomega) {
		var ns corev1.Namespace
		err := k8sClient.Resources().Get(suiteCtx, name, "", &ns)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"namespace %s still exists or unreachable: %v", name, err)
	}, timeout, 5*time.Second).Should(Succeed())
}

// ensureNamespace creates the namespace if it does not already exist.
func ensureNamespace(name string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	return decoder.CreateIgnoreAlreadyExists(k8sClient.Resources())(suiteCtx, ns)
}

// waitBeylaReady blocks until the beyla DaemonSet is scheduled and all pods
// are Ready. grafana/beyla:main uses imagePullPolicy: Always, so allow time.
func waitBeylaReady() {
	Eventually(func(g Gomega) {
		var ds appsv1.DaemonSet
		g.Expect(k8sClient.Resources().Get(suiteCtx, "beyla", ctrlNamespace, &ds)).To(Succeed())
		g.Expect(ds.Status.DesiredNumberScheduled).To(BeNumerically(">", 0), "DaemonSet not scheduled yet")
		g.Expect(ds.Status.NumberReady).To(Equal(ds.Status.DesiredNumberScheduled), "Beyla pods not all ready")
	}, 5*time.Minute, 5*time.Second).Should(Succeed())
}

// deployBeyla applies the shared Beyla manifest (SA, RBAC, DaemonSet) and
// creates the per-suite beyla-config ConfigMap targeting the given namespace.
// The DaemonSet picks up whatever beyla-config exists at pod start, so callers
// must invoke this once per suite (after a tearDownBeyla) rather than mutating
// the ConfigMap of a running DaemonSet.
func deployBeyla(targetNamespace string) error {
	manifestsDir := filepath.Join(projectDir, "test", "e2e", "manifests")
	if err := applyManifestFile(filepath.Join(manifestsDir, "beyla.yaml")); err != nil {
		return fmt.Errorf("apply beyla.yaml: %w", err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "beyla-config", Namespace: ctrlNamespace},
		Data:       map[string]string{"beyla-config.yaml": beylaConfigYAML(targetNamespace)},
	}
	return decoder.CreateIgnoreAlreadyExists(k8sClient.Resources())(suiteCtx, cm)
}

// beylaConfigYAML renders Beyla's config with the target instrumentation
// namespace and the controller's SDK image version. image_version must match
// the controller's own SDKInject config (config/test/sdk-inject.yaml).
func beylaConfigYAML(targetNamespace string) string {
	return fmt.Sprintf(`log_level: debug
attributes:
  kubernetes:
    enable: true
# OTLP export destinations. The injected SDK's OTLP endpoint is taken from
# the traces export (one endpoint carries both signals into the per-node
# ConfigMap that the controller reads). otel_metrics_export is set to the
# same otel-lgtm address to match the e2e's metrics intent.
otel_traces_export:
  endpoint: http://lgtm.e2e-lgtm.svc.cluster.local:4318
  protocol: http/protobuf
otel_metrics_export:
  endpoint: http://lgtm.e2e-lgtm.svc.cluster.local:4318
  protocol: http/protobuf
injector:
  # Workloads in these namespaces are recorded in the per-node ConfigMap as
  # eligible_for_restart and sent to the external controller.
  instrument:
    - k8s_namespace: %s
  # Delegate mutation to the external controller-manager Deployment. When
  # set, Beyla skips its own TLS webhook server and only publishes the
  # per-node ConfigMap.
  webhook:
    external_deployment_name: beyla-k8s-injector/beyla-k8s-injector-controller-manager
  image_version: %s
`, targetNamespace, sdkImageVersion)
}

// tearDownBeyla removes the per-suite Beyla state so a subsequent suite starts
// from a clean slate: the DaemonSet (so the next deployBeyla picks up a fresh
// ConfigMap), the beyla-config ConfigMap itself, and all per-node injection
// ConfigMaps Beyla published (which the controller has already drained from its
// in-memory registry via the delete event — see configmap_controller.go).
// Errors are intentionally ignored: AfterAll best-effort.
func tearDownBeyla() {
	_ = k8sClient.Resources().Delete(suiteCtx,
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "beyla", Namespace: ctrlNamespace}})
	_ = k8sClient.Resources().Delete(suiteCtx,
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "beyla-config", Namespace: ctrlNamespace}})
	var cms corev1.ConfigMapList
	if err := k8sClient.Resources(ctrlNamespace).List(suiteCtx, &cms,
		resources.WithLabelSelector("beyla.grafana.com/node")); err == nil {
		for i := range cms.Items {
			_ = k8sClient.Resources().Delete(suiteCtx, &cms.Items[i])
		}
	}
}

// overrideManagerImage rewrites the manager container's image on the deployed
// controller to the locally-built image loaded into kind, and waits for the Get
// to succeed (the Deployment exists right after applyKustomization).
func overrideManagerImage() {
	var dep appsv1.Deployment
	Expect(k8sClient.Resources().Get(suiteCtx, ctrlDeployment, ctrlNamespace, &dep)).
		To(Succeed(), "controller Deployment should exist after applying config/test")
	for i := range dep.Spec.Template.Spec.Containers {
		if dep.Spec.Template.Spec.Containers[i].Name == "manager" {
			dep.Spec.Template.Spec.Containers[i].Image = managerImage
		}
	}
	Expect(k8sClient.Resources().Update(suiteCtx, &dep)).To(Succeed(), "failed to override manager image")
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
