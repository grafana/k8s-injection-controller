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

	lgtmNS     = "e2e-lgtm"
	injectAnno = "beyla.grafana.com/inject"

	// sdkImageVersion must match config/test/sdk-inject.yaml. Used when
	// crafting manual injection ConfigMaps in the lifecycle and safety tests.
	sdkImageVersion = "0.0.13"

	// Use 127.0.0.1, not localhost: kind binds the host port on IPv4 only.
	tempoBaseURL = "http://127.0.0.1:30320"
)

// applyManifestFile applies every document in a manifest file, creating each
// object (ignoring ones that already exist).
func applyManifestFile(path string) error {
	return decoder.DecodeEachFile(suiteCtx, os.DirFS(filepath.Dir(path)), filepath.Base(path),
		decoder.CreateIgnoreAlreadyExists(k8sClient.Resources()))
}

// applyKustomization renders dir with the kustomize Go API and creates the
// objects. Admission webhook configurations are created last so the validating
// webhook isn't registered while the beyla-sdk-config ConfigMap is still being
// applied (failurePolicy=Fail would reject the ConfigMap before the server is up).
func applyKustomization(dir string) error {
	resMap, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(filesys.MakeFsOnDisk(), dir)
	if err != nil {
		return fmt.Errorf("kustomize build %s: %w", dir, err)
	}
	rendered, err := resMap.AsYaml()
	if err != nil {
		return fmt.Errorf("rendering kustomize output: %w", err)
	}
	// Force init_container mode: the e2e cluster has no ImageVolume feature gate.
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

// injectionModeLine matches the top-level `injection_mode:` key (column 0,
// so commented mentions are ignored).
var injectionModeLine = regexp.MustCompile(`(?m)^injection_mode:.*$`)

// forceInitContainerMode rewrites the SDK config ConfigMap's injection_mode to
// init_container. We change only the data so kustomize's content hash on the
// ConfigMap name still matches the manager Deployment's mount.
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
// WebhookConfiguration.
func isAdmissionWebhookConfig(obj k8s.Object) bool {
	switch obj.(type) {
	case *admissionregistrationv1.ValidatingWebhookConfiguration,
		*admissionregistrationv1.MutatingWebhookConfiguration:
		return true
	}
	return false
}

// waitNamespaceDeleted blocks until the namespace returns 404. Other errors
// (e.g. connection refused) do not unblock the wait.
func waitNamespaceDeleted(name string) {
	Eventually(func(g Gomega) {
		var ns corev1.Namespace
		err := k8sClient.Resources().Get(suiteCtx, name, "", &ns)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"namespace %s still exists or unreachable: %v", name, err)
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
}

// ensureNamespace creates the namespace if it does not already exist.
func ensureNamespace(name string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	return decoder.CreateIgnoreAlreadyExists(k8sClient.Resources())(suiteCtx, ns)
}

// waitBeylaReady blocks until the beyla DaemonSet is scheduled and all pods
// Ready. First-run pull of grafana/beyla can take time.
func waitBeylaReady() {
	Eventually(func(g Gomega) {
		var ds appsv1.DaemonSet
		g.Expect(k8sClient.Resources().Get(suiteCtx, "beyla", ctrlNamespace, &ds)).To(Succeed())
		g.Expect(ds.Status.DesiredNumberScheduled).To(BeNumerically(">", 0), "DaemonSet not scheduled yet")
		g.Expect(ds.Status.NumberReady).To(Equal(ds.Status.DesiredNumberScheduled), "Beyla pods not all ready")
	}, 5*time.Minute, 5*time.Second).Should(Succeed())
}

// deployBeyla applies the shared Beyla manifest and creates the per-suite
// beyla-config ConfigMap targeting the given namespace. Beyla pods read the
// ConfigMap only at start, so callers must tearDownBeyla first.
func deployBeyla(targetNamespace string) error {
	if err := applyManifestFile(filepath.Join(manifestsDir, "beyla.yaml")); err != nil {
		return fmt.Errorf("apply beyla.yaml: %w", err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "beyla-config", Namespace: ctrlNamespace},
		Data:       map[string]string{"beyla-config.yaml": beylaConfigYAML(targetNamespace)},
	}
	return decoder.CreateIgnoreAlreadyExists(k8sClient.Resources())(suiteCtx, cm)
}

// beylaConfigYAML renders Beyla's config targeting the given namespace.
// image_version must match config/test/sdk-inject.yaml.
func beylaConfigYAML(targetNamespace string) string {
	return fmt.Sprintf(`log_level: debug
attributes:
  kubernetes:
    enable: true
otel_traces_export:
  endpoint: http://lgtm.e2e-lgtm.svc.cluster.local:4318
  protocol: http/protobuf
otel_metrics_export:
  endpoint: http://lgtm.e2e-lgtm.svc.cluster.local:4318
  protocol: http/protobuf
injector:
  instrument:
    - k8s_namespace: %s
  webhook:
    external_deployment_name: beyla-k8s-injector/beyla-k8s-injector-controller-manager
  image_version: %s
`, targetNamespace, sdkImageVersion)
}

// tearDownBeyla removes the per-suite Beyla state and waits for the DaemonSet
// and its pods to be fully gone. Without the wait, the next deployBeyla can
// silently no-op (CreateIgnoreAlreadyExists) while the previous DaemonSet is
// still tombstoning, leaving stale pods running with the old beyla-config.
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
	Eventually(func(g Gomega) {
		var ds appsv1.DaemonSet
		err := k8sClient.Resources().Get(suiteCtx, "beyla", ctrlNamespace, &ds)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Beyla DaemonSet still exists")
		var pods corev1.PodList
		g.Expect(k8sClient.Resources(ctrlNamespace).List(suiteCtx, &pods,
			resources.WithLabelSelector("app.kubernetes.io/name=beyla"))).To(Succeed())
		g.Expect(pods.Items).To(BeEmpty(), "Beyla pods still terminating")
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
}

// overrideManagerImage swaps the manager container's image to the locally-built
// one loaded into kind. Triggers the rollout.
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
	}, timeout, 2*time.Second).Should(Succeed())
}

// waitWebhookReachable blocks until the validating ConfigMap webhook is
// actually serving. It waits for CA injection and endpoint programming, then
// dry-run creates an annotated ConfigMap: either an allow or a webhook-side
// deny means the server responded. Only a network/handshake error
// ("failed calling webhook") keeps the probe retrying.
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

	Eventually(func(g Gomega) {
		probe := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "webhook-readiness-probe",
				Namespace:   ctrlNamespace,
				Annotations: map[string]string{"beyla.grafana.com/node": ""},
			},
		}
		_, err := clientset.CoreV1().ConfigMaps(ctrlNamespace).Create(suiteCtx, probe, metav1.CreateOptions{
			DryRun: []string{metav1.DryRunAll},
		})
		if err == nil {
			return
		}
		g.Expect(err.Error()).NotTo(ContainSubstring("failed calling webhook"),
			"webhook server not yet reachable: %v", err)
	}, 1*time.Minute, 2*time.Second).Should(Succeed())
}

// waitInstrumentedReadyPod blocks until a pod matching selector in ns is both
// instrumented (carries injectAnno) and Ready.
func waitInstrumentedReadyPod(ns, selector string) {
	Eventually(func(g Gomega) {
		var pods corev1.PodList
		g.Expect(k8sClient.Resources(ns).List(suiteCtx, &pods,
			resources.WithLabelSelector(selector))).To(Succeed())
		ready := false
		for i := range pods.Items {
			p := &pods.Items[i]
			if p.Annotations[injectAnno] != "" && podReady(p) {
				ready = true
				break
			}
		}
		g.Expect(ready).To(BeTrue(),
			"no instrumented, ready pod for selector %q in %s yet", selector, ns)
	}, 5*time.Minute, 2*time.Second).Should(Succeed())
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

// tempoSearchResult is the slice of Tempo's /api/search response we read.
type tempoSearchResult struct {
	Traces []struct {
		TraceID string `json:"traceID"`
	} `json:"traces"`
}

// tempoHasTraces tries each TraceQL query in order and returns the first to
// match plus its trace count. Multiple queries keep the assertion resilient
// to the exact resource.service.name the injector stamps.
func tempoHasTraces(queries ...string) (string, int, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	last := ""
	for _, q := range queries {
		last = q
		u := tempoBaseURL + "/api/search?q=" + url.QueryEscape(q) + "&limit=20"
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

// dumpPodLogs writes the tail of logs for matching pods to the Ginkgo writer.
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

// dumpInjectionConfigMaps writes each per-node injection ConfigMap to the
// Ginkgo writer.
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
