//go:build e2esmoke

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

// Package smoke_test is the k8s-monitoring-helm integration smoke test. It
// installs the k8s-monitoring Helm chart (with Beyla and the injection
// controller sub-charts enabled) against a locally-built controller image,
// then asserts that the full SDK auto-instrumentation pipeline produces traces
// and metrics in the bundled otel-lgtm stack. The suite owns a dedicated Kind
// cluster so it runs independently of the main e2e suite.
//
// By default the chart is pulled from the Grafana GHCR OCI registry at the
// pinned version [pinnedChartVersion]. To use a local checkout instead (useful
// when iterating on k8s-monitoring-helm changes), set:
//
//	K8S_MONITORING_HELM_CHART_DIR=/path/to/k8s-monitoring-helm/charts/k8s-monitoring
//
// Optional:
//
//	KIND_KEEP_CLUSTER=true  — keep the Kind cluster after the suite finishes
package smoke_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/moby/go-archive"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/support/kind"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"helm.sh/helm/v3/pkg/registry"

	"github.com/grafana/beyla-k8s-injector/test/utils"
)

const (
	// smokeClusterName is the Kind cluster created by this suite.
	smokeClusterName = "k8s-injection-controller-smoke"

	// smokeReleaseName and smokeNamespace are the Helm release identity. Both
	// Beyla (from autoInstrumentation) and the injection controller (from
	// telemetryServices.sdkInjector) land in smokeNamespace so they share a
	// namespace and the controller's ConfigMap watcher sees Beyla's writes.
	smokeReleaseName = "k8smon"
	smokeNamespace   = "monitoring"

	// Resource names derived deterministically from release name + chart templates.
	smokeBeylaDS        = smokeReleaseName + "-beyla"
	smokeInjectorDeploy = smokeReleaseName + "-k8s-injection-controller"
	smokeMutatingWH     = smokeInjectorDeploy + "-mutating-webhook"
	smokeWebhookSvc     = smokeInjectorDeploy + "-webhook"

	// sdkSmokeTestNS is the namespace where the SDK test apps run.
	// Must match the namespace in test/smoke/manifests/instrumentation-apps.yaml.
	sdkSmokeTestNS = "sdk-test"
	// lgtmNS is where grafana/otel-lgtm is deployed (reuses the main suite's manifest).
	lgtmNS = "e2e-lgtm"

	managerImage    = "beyla-k8s-injector:dev"
	sdkImageVersion = "0.0.13"

	injectAnno   = "beyla.grafana.com/inject"
	tempoBaseURL = "http://127.0.0.1:30320"
	promBaseURL  = "http://127.0.0.1:30090"

	// pinnedChartVersion is the k8s-monitoring chart version pulled from GHCR
	// when K8S_MONITORING_HELM_CHART_DIR is not set. Bump intentionally when
	// validating against a newer chart release.
	pinnedChartVersion = "4.3.0"
	// pinnedChartOCI is the full OCI reference for the pinned chart.
	pinnedChartOCI = "ghcr.io/grafana/helm-charts/k8s-monitoring:" + pinnedChartVersion

	// chartDirEnvVar overrides the OCI download with a local chart directory,
	// useful when iterating on k8s-monitoring-helm changes locally.
	chartDirEnvVar = "K8S_MONITORING_HELM_CHART_DIR"

	// SDK app image tags — match test/e2e image tags so images built by
	// either suite can be shared if both run in the same session.
	sdkNodejsImage     = "sdk-nodejs-app:dev"
	sdkPythonImage     = "sdk-python-app:dev"
	sdkPythonMuslImage = "sdk-python-musl-app:dev"
	sdkJavaImage       = "sdk-java-app:dev"
	sdkDotnetImage     = "sdk-dotnet-app:dev"
	sdkDotnetMuslImage = "sdk-dotnet-musl-app:dev"
)

var (
	projectDir       string
	manifestsDir     string // test/e2e/manifests — shared lgtm manifest
	smokeManifestsDir string // test/smoke/manifests — smoke-only manifests

	testCluster *kind.Cluster
	k8sClient   klient.Client
	clientset   *kubernetes.Clientset
	suiteCtx    = context.Background()
)

func TestSmoke(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "e2e smoke suite")
}

var _ = BeforeSuite(func() {
	var err error

	projectDir, err = utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred(), "Failed to resolve project directory")
	manifestsDir = filepath.Join(projectDir, "test", "e2e", "manifests")
	smokeManifestsDir = filepath.Join(projectDir, "test", "smoke", "manifests")
	kindConfig := filepath.Join(projectDir, "test", "e2e", "kind-config.yaml")

	By("building the manager image")
	Expect(buildImage(suiteCtx, projectDir, managerImage, ".git", "bin", "dist")).
		To(Succeed(), "Failed to build the manager image")

	By("building SDK test app images")
	for _, app := range smokeAppDirs(projectDir) {
		Expect(buildImage(suiteCtx, app.dir, app.tag)).
			To(Succeed(), "Failed to build %s", app.tag)
	}

	// Destroy any stale cluster from a previous interrupted run before
	// creating a fresh one. Port conflicts and container-name collisions
	// from a leftover cluster can cause the new node container to crash
	// during initialization.
	By("destroying any stale smoke cluster from a previous run")
	stale := kind.NewCluster(smokeClusterName)
	_ = stale.Destroy(suiteCtx)

	By("creating the Kind cluster")
	testCluster = kind.NewCluster(smokeClusterName)
	_, err = testCluster.CreateWithConfig(suiteCtx, kindConfig)
	Expect(err).NotTo(HaveOccurred(), "Failed to create Kind cluster")

	By("loading images into Kind")
	Expect(testCluster.LoadImage(suiteCtx, managerImage)).To(Succeed())
	for _, app := range smokeAppDirs(projectDir) {
		Expect(testCluster.LoadImage(suiteCtx, app.tag)).
			To(Succeed(), "Failed to load %s", app.tag)
	}

	By("pre-pulling and loading grafana/otel-lgtm")
	lgtmImage, err := lgtmImageRef(filepath.Join(manifestsDir, "instrumentation-lgtm.yaml"))
	Expect(err).NotTo(HaveOccurred(), "Failed to resolve lgtm image ref")
	pullCtx, pullCancel := context.WithTimeout(suiteCtx, 10*time.Minute)
	defer pullCancel()
	Expect(pullAndLoadImage(pullCtx, smokeClusterName, lgtmImage)).To(Succeed())

	By("building k8s clients")
	k8sClient, err = klient.NewWithKubeConfigFile(testCluster.GetKubeconfig())
	Expect(err).NotTo(HaveOccurred(), "Failed to build klient client")
	clientset, err = kubernetes.NewForConfig(k8sClient.RESTConfig())
	Expect(err).NotTo(HaveOccurred(), "Failed to build clientset")

	By("deploying grafana/otel-lgtm")
	Expect(applyManifestFile(filepath.Join(manifestsDir, "instrumentation-lgtm.yaml"))).To(Succeed())

	// Pre-create the release namespace. The chart renders a Namespace object
	// when namespace.create=true, but krusty/Helm does not guarantee
	// Namespace-first ordering. Pre-creating avoids a race where namespaced
	// resources in the same apply land before their namespace is created.
	By("ensuring the release namespace exists")
	Expect(ensureNamespace(smokeNamespace)).To(Succeed())

	By("querying cluster version for Helm capabilities")
	serverVersion, err := clientset.Discovery().ServerVersion()
	Expect(err).NotTo(HaveOccurred(), "Failed to get server version")
	// Pass the real KubeVersion so chart version guards (e.g. the >=1.28
	// matchConditions check) evaluate against the actual cluster, not Helm's
	// baked-in default of v1.20.0.
	caps := &chartutil.Capabilities{
		KubeVersion: chartutil.KubeVersion{
			Version: serverVersion.GitVersion,
			Major:   serverVersion.Major,
			Minor:   serverVersion.Minor,
		},
		APIVersions: chartutil.DefaultVersionSet,
	}

	By("loading k8s-monitoring-helm chart")
	ch, err := loadChart()
	Expect(err).NotTo(HaveOccurred(), "Failed to load k8s-monitoring chart")

	By("deploying k8s-monitoring-helm chart (Beyla + injection controller)")
	Expect(renderAndApplyHelmChart(ch, smokeReleaseName, smokeNamespace, smokeChartValues(), caps)).
		To(Succeed(), "Failed to deploy k8s-monitoring chart")

	By("waiting for the injection controller rollout")
	waitDeploymentReady(smokeInjectorDeploy, smokeNamespace, 3*time.Minute)

	By("waiting for the webhook to be reachable")
	waitWebhookReachable()

	By("waiting for grafana/otel-lgtm to be ready")
	waitDeploymentReady("lgtm", lgtmNS, 5*time.Minute)

	By("waiting for the Beyla DaemonSet to be ready")
	waitBeylaReady()
})

var _ = AfterSuite(func() {
	if testCluster == nil {
		return
	}
	utils.ExportClusterLogs(context.Background(), testCluster, "e2e-smoke")
	if os.Getenv("KIND_KEEP_CLUSTER") == "true" {
		_, _ = fmt.Fprintf(GinkgoWriter, "Keeping Kind cluster (KIND_KEEP_CLUSTER=true)\n")
		return
	}
	By("destroying the Kind cluster")
	// Beyla runs with hostPID=true and privileged=true; Docker Desktop
	// occasionally cannot kill such containers cleanly. Treat destroy
	// failure as a warning rather than a suite failure — the container
	// will be cleaned up by `docker system prune` or Docker restart.
	if err := testCluster.Destroy(context.Background()); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Warning: failed to destroy Kind cluster (may need manual cleanup): %v\n", err)
	}
})

// smokeChartValues returns the Helm values map. Release-derived names
// (Beyla SA, controller Deployment) are computed here so allowedConfigMapWriters
// and external_deployment_name stay consistent with the release identity.
func smokeChartValues() map[string]interface{} {
	beylaSAName := smokeReleaseName + "-beyla"
	allowedWriter := fmt.Sprintf("system:serviceaccount:%s:%s", smokeNamespace, beylaSAName)
	externalDeployment := fmt.Sprintf("%s/%s", smokeNamespace, smokeInjectorDeploy)

	return map[string]interface{}{
		"cluster": map[string]interface{}{
			"name": smokeClusterName,
		},
		// A minimal Prometheus destination is required by the chart's validation:
		// autoInstrumentation needs at least one metrics-capable destination.
		// Since Alloy isn't running (alloy-operator disabled), this destination
		// is never actually used — it only satisfies the schema check.
		"destinations": map[string]interface{}{
			"lgtm-prometheus": map[string]interface{}{
				"type": "prometheus",
				"url":  "http://lgtm." + lgtmNS + ".svc.cluster.local:9090/api/v1/write",
			},
		},
		// Disable the alloy-operator: its CRD (kind: Alloy) is not installed in the
		// test cluster. The chart still renders Alloy CR objects for any defined
		// collector; renderAndApplyHelmChart filters those out before apply.
		"alloy-operator": map[string]interface{}{
			"deploy": false,
		},
		// Minimal collector satisfies the chart's "at least one collector enabled"
		// validation. The Alloy CR it produces is filtered out; no Alloy runs.
		"collectors": map[string]interface{}{
			"alloy": map[string]interface{}{
				"extraConfig": "// smoke-test placeholder",
			},
		},
		"autoInstrumentation": map[string]interface{}{
			"enabled": true,
			"beyla": map[string]interface{}{
				// Creates the Role+RoleBinding that lets the Beyla SA write per-node
				// injection ConfigMaps into the shared release namespace.
				"injector": map[string]interface{}{
					"enabled": true,
				},
				"config": map[string]interface{}{
					"data": map[string]interface{}{
						"log_level": "debug",
						"otel_traces_export": map[string]interface{}{
							"endpoint": "http://lgtm." + lgtmNS + ".svc.cluster.local:4318",
							"protocol": "http/protobuf",
						},
						"otel_metrics_export": map[string]interface{}{
							"endpoint": "http://lgtm." + lgtmNS + ".svc.cluster.local:4318",
							"protocol": "http/protobuf",
						},
						// Not overridden by _beyla-config.tpl; passes through as-is.
						"injector": map[string]interface{}{
							"instrument": []interface{}{
								map[string]interface{}{"k8s_namespace": sdkSmokeTestNS},
							},
							"webhook": map[string]interface{}{
								// Gates Beyla's ConfigMap writes on webhook readiness.
								"external_deployment_name": externalDeployment,
							},
							"image_version": sdkImageVersion,
						},
					},
				},
			},
		},
		"telemetryServices": map[string]interface{}{
			"sdkInjector": map[string]interface{}{
				"deploy": true,
				// Same namespace as Beyla — watcher and writer share it without cross-namespace RBAC.
				"namespace": map[string]interface{}{
					"name":   smokeNamespace,
					"create": false, // pre-created in BeforeSuite to avoid ordering races
				},
				"allowedConfigMapWriters": allowedWriter,
				"image": map[string]interface{}{
					"repository": "beyla-k8s-injector",
					"tag":        "dev",
					"pullPolicy": "Never",
				},
				"sdkConfig": map[string]interface{}{
					"injectionMode": "init_container",
				},
			},
		},
	}
}

// loadChart loads from K8S_MONITORING_HELM_CHART_DIR if set, otherwise pulls
// the pinned version from GHCR (public; no credentials required).
func loadChart() (*chart.Chart, error) {
	if dir := os.Getenv(chartDirEnvVar); dir != "" {
		_, _ = fmt.Fprintf(GinkgoWriter, "Loading chart from local directory: %s\n", dir)
		return loader.Load(dir)
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "Pulling chart from OCI: %s\n", pinnedChartOCI)
	client, err := registry.NewClient(registry.ClientOptWriter(GinkgoWriter))
	if err != nil {
		return nil, fmt.Errorf("creating registry client: %w", err)
	}
	res, err := client.Pull(pinnedChartOCI)
	if err != nil {
		return nil, fmt.Errorf("pulling %s: %w", pinnedChartOCI, err)
	}
	return loader.LoadArchive(bytes.NewReader(res.Chart.Data))
}

// renderAndApplyHelmChart renders ch, filters Alloy CRs, and applies objects
// with webhook configurations deferred last.
func renderAndApplyHelmChart(ch *chart.Chart, releaseName, namespace string, vals map[string]interface{}, caps *chartutil.Capabilities) error {
	// ProcessDependenciesWithMerge must run on the raw user values BEFORE
	// ToRenderValues. Called after, it receives the full render context rather
	// than .Values and breaks condition evaluation, leaving disabled sub-charts
	// enabled and their `fail` guards firing (e.g. k8s-manifest-tail).
	if err := chartutil.ProcessDependenciesWithMerge(ch, vals); err != nil {
		return fmt.Errorf("processing chart dependencies: %w", err)
	}

	coalesced, err := chartutil.ToRenderValues(ch, vals, chartutil.ReleaseOptions{
		Name:      releaseName,
		Namespace: namespace,
		IsInstall: true,
	}, caps)
	if err != nil {
		return fmt.Errorf("coalescing chart values: %w", err)
	}

	rendered, err := engine.Render(ch, coalesced)
	if err != nil {
		return fmt.Errorf("rendering chart: %w", err)
	}

	// Prepend "---\n" so templates starting directly with apiVersion: (no
	// leading document marker) are treated as separate YAML documents. Without
	// this, consecutive outputs merge and duplicate-key resolution silently
	// drops earlier objects.
	var allYAML strings.Builder
	for name, content := range rendered {
		if strings.HasSuffix(name, "NOTES.txt") || strings.TrimSpace(content) == "" {
			continue
		}
		allYAML.WriteString("---\n")
		allYAML.WriteString(content)
		allYAML.WriteString("\n")
	}

	// Filter Alloy CRs — alloy-operator disabled but placeholder collector still renders them.
	filtered := filterAlloyCRs(allYAML.String())

	objs, err := decoder.DecodeAll(suiteCtx, bytes.NewReader(filtered))
	if err != nil {
		return fmt.Errorf("decoding chart objects: %w", err)
	}

	create := decoder.CreateIgnoreAlreadyExists(k8sClient.Resources())
	var deferred []k8s.Object
	for _, obj := range objs {
		if isAdmissionWebhookConfig(obj) {
			deferred = append(deferred, obj)
			continue
		}
		if err := create(suiteCtx, obj); err != nil {
			return fmt.Errorf("creating %s %s: %w",
				obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
		}
	}
	for _, obj := range deferred {
		if err := create(suiteCtx, obj); err != nil {
			return fmt.Errorf("creating webhook config %s: %w", obj.GetName(), err)
		}
	}
	return nil
}

// filterAlloyCRs removes kind: Alloy documents from a YAML stream.
// Splits on --- at line start; safe because Helm always indents embedded --- in string values.
func filterAlloyCRs(data string) []byte {
	var result strings.Builder
	var docBuf strings.Builder

	flush := func() {
		doc := docBuf.String()
		docBuf.Reset()
		if strings.TrimSpace(doc) == "" {
			return
		}
		if strings.Contains(doc, "kind: Alloy") && strings.Contains(doc, "collectors.grafana.com") {
			return
		}
		result.WriteString("---\n")
		result.WriteString(doc)
	}

	for _, line := range strings.Split(data, "\n") {
		if line == "---" {
			flush()
		} else {
			docBuf.WriteString(line)
			docBuf.WriteString("\n")
		}
	}
	flush()
	return []byte(result.String())
}

func isAdmissionWebhookConfig(obj k8s.Object) bool {
	switch obj.(type) {
	case *admissionregistrationv1.MutatingWebhookConfiguration,
		*admissionregistrationv1.ValidatingWebhookConfiguration:
		return true
	}
	return false
}

func waitDeploymentReady(name, ns string, timeout time.Duration) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		var dep appsv1.Deployment
		g.Expect(k8sClient.Resources().Get(suiteCtx, name, ns, &dep)).To(Succeed())
		g.Expect(dep.Spec.Replicas).NotTo(BeNil())
		g.Expect(dep.Status.ObservedGeneration).To(BeNumerically(">=", dep.Generation))
		g.Expect(dep.Status.UpdatedReplicas).To(Equal(*dep.Spec.Replicas))
		g.Expect(dep.Status.ReadyReplicas).To(Equal(*dep.Spec.Replicas))
	}, timeout, 2*time.Second).Should(Succeed())
}

func waitBeylaReady() {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		var ds appsv1.DaemonSet
		g.Expect(k8sClient.Resources().Get(suiteCtx, smokeBeylaDS, smokeNamespace, &ds)).To(Succeed())
		g.Expect(ds.Status.DesiredNumberScheduled).To(BeNumerically(">", 0), "DaemonSet not yet scheduled")
		g.Expect(ds.Status.NumberReady).To(Equal(ds.Status.DesiredNumberScheduled), "Beyla pods not all ready")
	}, 5*time.Minute, 5*time.Second).Should(Succeed())
}

// waitWebhookReachable blocks until the webhook is fully operational.
func waitWebhookReachable() {
	GinkgoHelper()

	// 1. CA bundle injected.
	Eventually(func(g Gomega) {
		var mwc admissionregistrationv1.MutatingWebhookConfiguration
		g.Expect(k8sClient.Resources().Get(suiteCtx, smokeMutatingWH, "", &mwc)).To(Succeed())
		g.Expect(mwc.Webhooks).NotTo(BeEmpty())
		g.Expect(mwc.Webhooks[0].ClientConfig.CABundle).NotTo(BeEmpty(), "CA bundle not yet injected")
	}, 3*time.Minute, 5*time.Second).Should(Succeed())

	// 2. Endpoint programmed.
	Eventually(func(g Gomega) {
		var slices discoveryv1.EndpointSliceList
		g.Expect(k8sClient.Resources(smokeNamespace).List(suiteCtx, &slices,
			resources.WithLabelSelector("kubernetes.io/service-name="+smokeWebhookSvc))).To(Succeed())
		addrs := 0
		for _, s := range slices.Items {
			for _, ep := range s.Endpoints {
				addrs += len(ep.Addresses)
			}
		}
		g.Expect(addrs).To(BeNumerically(">", 0), "webhook endpoint not yet ready")
	}, 3*time.Minute, 5*time.Second).Should(Succeed())

	// 3. Dry-run a ConfigMap in the watchNamespace; only "failed calling webhook"
	//    (network/TLS failure) keeps retrying.
	Eventually(func(g Gomega) {
		probe := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "webhook-readiness-probe",
				Namespace:   smokeNamespace,
				Annotations: map[string]string{"beyla.grafana.com/node": ""},
			},
		}
		_, err := clientset.CoreV1().ConfigMaps(smokeNamespace).Create(
			suiteCtx, probe, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		if err == nil {
			return
		}
		g.Expect(err.Error()).NotTo(ContainSubstring("failed calling webhook"),
			"webhook server not yet reachable: %v", err)
	}, 1*time.Minute, 2*time.Second).Should(Succeed())
}

func waitInstrumentedReadyPod(ns, selector string) {
	GinkgoHelper()
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
			"no instrumented, ready pod for %q in %s yet", selector, ns)
	}, 5*time.Minute, 2*time.Second).Should(Succeed())
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func ensureNamespace(name string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	return decoder.CreateIgnoreAlreadyExists(k8sClient.Resources())(suiteCtx, ns)
}

func applyManifestFile(path string) error {
	return decoder.DecodeEachFile(suiteCtx, os.DirFS(filepath.Dir(path)), filepath.Base(path),
		decoder.CreateIgnoreAlreadyExists(k8sClient.Resources()))
}

// smokeAppDirs returns SDK test app dirs/tags. Tags match test/e2e so images
// are interchangeable between suites.
func smokeAppDirs(projectDir string) []struct{ dir, tag string } {
	base := filepath.Join(projectDir, "test", "e2e", "apps")
	return []struct{ dir, tag string }{
		{filepath.Join(base, "instrumentation-nodejs"), sdkNodejsImage},
		{filepath.Join(base, "instrumentation-python-glibc"), sdkPythonImage},
		{filepath.Join(base, "instrumentation-python-musl"), sdkPythonMuslImage},
		{filepath.Join(base, "instrumentation-java"), sdkJavaImage},
		{filepath.Join(base, "instrumentation-dotnet-glibc"), sdkDotnetImage},
		{filepath.Join(base, "instrumentation-dotnet-musl"), sdkDotnetMuslImage},
	}
}

func buildImage(ctx context.Context, dir, tag string, excludes ...string) error {
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("creating docker client: %w", err)
	}
	defer func() { _ = cli.Close() }()

	buildContext, err := archive.TarWithOptions(dir, &archive.TarOptions{ExcludePatterns: excludes})
	if err != nil {
		return fmt.Errorf("creating build context: %w", err)
	}
	defer func() { _ = buildContext.Close() }()

	resp, err := cli.ImageBuild(ctx, buildContext, build.ImageBuildOptions{
		Tags:       []string{tag},
		Dockerfile: "Dockerfile",
		Remove:     true,
	})
	if err != nil {
		return fmt.Errorf("starting image build: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := jsonmessage.DisplayJSONMessagesStream(resp.Body, GinkgoWriter, 0, false, nil); err != nil {
		return fmt.Errorf("image build failed: %w", err)
	}
	return nil
}

// pullAndLoadImage pulls a registry image into the Kind node's containerd via
// ctr images pull. See test/e2e/e2e_suite_test.go for why docker save → kind
// load fails with multi-arch images on Docker Desktop.
func pullAndLoadImage(ctx context.Context, clusterName, ref string) error {
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("creating docker client: %w", err)
	}
	defer func() { _ = cli.Close() }()

	info, err := cli.Info(ctx)
	if err != nil {
		return fmt.Errorf("getting docker info: %w", err)
	}
	platform := "linux/amd64"
	if info.Architecture == "aarch64" || info.Architecture == "arm64" {
		platform = "linux/arm64"
	}

	ctrRef := ref
	if !strings.Contains(strings.SplitN(ref, "/", 2)[0], ".") {
		ctrRef = "docker.io/" + ref
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "Pulling %s (%s) into Kind via ctr\n", ctrRef, platform)

	kindNode := clusterName + "-control-plane"
	execCreate, err := cli.ContainerExecCreate(ctx, kindNode, container.ExecOptions{
		AttachStdout: true,
		AttachStderr: true,
		Cmd: []string{"ctr", "--namespace=k8s.io", "images", "pull", "--platform", platform, ctrRef},
	})
	if err != nil {
		return fmt.Errorf("creating ctr pull exec in %s: %w", kindNode, err)
	}

	conn, err := cli.ContainerExecAttach(ctx, execCreate.ID, container.ExecStartOptions{})
	if err != nil {
		return fmt.Errorf("attaching ctr pull exec: %w", err)
	}
	defer conn.Close()

	var out bytes.Buffer
	_, _ = stdcopy.StdCopy(&out, &out, conn.Reader)

	result, err := cli.ContainerExecInspect(ctx, execCreate.ID)
	if err != nil {
		return fmt.Errorf("inspecting ctr pull result: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("ctr images pull failed (exit %d):\n%s", result.ExitCode, out.String())
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "ctr pull: %s\n", out.String())
	return nil
}

// lgtmImageRef extracts the grafana/otel-lgtm image ref from a manifest file.
func lgtmImageRef(manifestPath string) (string, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if s := strings.TrimSpace(line); strings.HasPrefix(s, "image: grafana/otel-lgtm:") {
			return strings.TrimPrefix(s, "image: "), nil
		}
	}
	return "", fmt.Errorf("grafana/otel-lgtm image not found in %s", manifestPath)
}
