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
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
)

const (
	safetyTestNS                = "safety-test"
	safetyPythonOldApp          = "safety-python-old"
	safetyPythonConflictApp     = "safety-python-conflict"
	safetyNodejsOldApp          = "safety-nodejs-old"
	safetyNodejsSdkInstalledApp = "safety-nodejs-sdk-installed"
	safetyNodejsSdkRequiredApp  = "safety-nodejs-sdk-required"
	safetyJavaGraalvmApp        = "safety-java-graalvm"
	safetyDotnetAOTApp          = "safety-dotnet-aot"
	safetyNodejsESMApp          = "safety-nodejs-esm"

	// Expected substrings in pod logs confirming which safety gate fired. These
	// are external contracts emitted by the inject-sdk-image runtime scripts
	// shipped with Beyla; if upstream rewords any of them, these tests will
	// fail and the strings here need updating. Source locations in
	// grafana/beyla:
	//   pkg/webhook/image/python/sitecustomize.py
	//     - check_python_version → "Python version 3.9 or higher required"
	//     - verify_and_load      → "Not importing OpenTelemetry Python auto-instrumentation"
	//   pkg/webhook/image/node.js/register.js
	//     - version range check      → "does not support Node.js version"
	//     - isOtelSdkInstalled       → "already instrumented with OpenTelemetry"
	//     - isOtelSdkRequiredViaArgs → "already using OpenTelemetry auto-instrumentation"
	logPythonVersionCheck = "Python version 3.9 or higher required"
	logPythonConflict     = "Not importing OpenTelemetry Python auto-instrumentation"
	logNodejsVersionCheck = "does not support Node.js version"
	logNodejsSdkInstalled = "already instrumented with OpenTelemetry"
	logNodejsSdkRequired  = "already using OpenTelemetry auto-instrumentation"
)

var (
	safetyPythonOldImage          = "safety-python-old-app:dev"
	safetyPythonConflictImage     = "safety-python-conflict-app:dev"
	safetyNodejsOldImage          = "safety-nodejs-old-app:dev"
	safetyNodejsSdkInstalledImage = "safety-nodejs-sdk-installed-app:dev"
	safetyNodejsSdkRequiredImage  = "safety-nodejs-sdk-required-app:dev"
	safetyJavaGraalvmImage        = "safety-java-graalvm-app:dev"
	safetyDotnetAOTImage          = "safety-dotnet-aot-app:dev"
	safetyNodejsESMImage          = "safety-nodejs-esm-app:dev"
)

// safetyAppDirs returns each safety test app's source directory and image tag.
// Called from BeforeSuite to build and load all images.
func safetyAppDirs(projectDir string) []struct{ dir, tag string } {
	base := filepath.Join(projectDir, "test", "e2e", "apps")
	return []struct{ dir, tag string }{
		{filepath.Join(base, "safety-python-old"), safetyPythonOldImage},
		{filepath.Join(base, "safety-python-conflict"), safetyPythonConflictImage},
		{filepath.Join(base, "safety-nodejs-old"), safetyNodejsOldImage},
		{filepath.Join(base, "safety-nodejs-sdk-installed"), safetyNodejsSdkInstalledImage},
		{filepath.Join(base, "safety-nodejs-sdk-required"), safetyNodejsSdkRequiredImage},
		{filepath.Join(base, "safety-java-graalvm"), safetyJavaGraalvmImage},
		{filepath.Join(base, "safety-dotnet-aot"), safetyDotnetAOTImage},
		{filepath.Join(base, "safety-nodejs-esm"), safetyNodejsESMImage},
	}
}

// buildSafetyAppImages builds all safety test app Docker images.
func buildSafetyAppImages(ctx context.Context, projectDir string) error {
	for _, app := range safetyAppDirs(projectDir) {
		if err := buildImage(ctx, app.dir, app.tag); err != nil {
			return fmt.Errorf("building %s: %w", app.tag, err)
		}
	}
	return nil
}

var _ = Describe("SDK auto-instrumentation safety", Ordered, func() {
	// This suite verifies two classes of safety behaviour:
	//
	// Class 1 — SDK skip: the runtime safety scripts (sitecustomize.py for
	// Python, register.js for Node.js) skip initialisation for incompatible
	// apps without crashing. Beyla adds these workloads to eligible_for_restart
	// (it scans and instruments them), but the SDK's own checks refuse to load.
	//
	// Class 2 — Injection resilience: the injector (libotelinject.so via
	// LD_PRELOAD) does not crash native runtimes that cannot load the SDK. For
	// GraalVM native image and .NET Native AOT there is no JVM/CLR to process
	// JAVA_TOOL_OPTIONS or DOTNET_STARTUP_HOOKS, so those env vars are silently
	// ignored; for ESM Node.js, --require register.js loads in CJS context
	// before the ESM module system initialises.
	//
	// Both classes follow the same three-act skeleton:
	//
	//   1. Class-1 apps running, Beyla absent: Tempo is empty.
	//   2. Beyla deploys and writes its per-node injection ConfigMap. Act 2
	//      gates on Beyla listing every Class-1 app in eligible_for_restart,
	//      then deploys Class-2 apps (admitted immediately by the active webhook).
	//   3. Each app: pod is injected (annotation + Running); Class-1 apps have
	//      no spans and their logs show the specific skip message; Class-2 apps
	//      have no spans and the pod is still Running.
	//
	// Note: check_otlp_proto (sitecustomize.py gate 2) is omitted because Beyla
	// always writes OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf into the rule env,
	// so that gate cannot be reached via the normal injection pipeline.

	BeforeAll(func() {
		manifestsDir := filepath.Join(projectDir, "test", "e2e", "manifests")

		// Within this binary, Ginkgo runs Describes in file load order
		// (alphabetical): e2e_beyla_startup_filter_test.go,
		// e2e_injection_test.go, this file, then e2e_instrumentation_test.go.
		// The startup-filter and injection suites both leave the cluster in a
		// clean state (no lgtm, no Beyla, no safety-test namespace), so on a
		// fresh run these wait-for-deletion calls return immediately. They are
		// load-bearing only on KIND_KEEP_CLUSTER=true re-runs, where this
		// suite's own AfterAll may still be tearing those namespaces down.
		By("waiting for previous lgtm namespace to finish terminating")
		waitNamespaceDeleted(lgtmNS, 2*time.Minute)

		By("waiting for previous safety-test namespace to finish terminating")
		waitNamespaceDeleted(safetyTestNS, 2*time.Minute)

		By("deploying grafana/otel-lgtm")
		Expect(applyManifestFile(filepath.Join(manifestsDir, "instrumentation-lgtm.yaml"))).To(Succeed())

		By("waiting for otel-lgtm to be ready")
		waitDeploymentReady("lgtm", lgtmNS, 5*time.Minute)

		By("ensuring the safety-test namespace exists for canary pods in act 2")
		// Class-1 apps are deployed in act 2, after the webhook is confirmed
		// active, so they are instrumented at admission rather than via
		// eligible_for_restart. The namespace must pre-exist for the canary pod.
		Expect(ensureNamespace(safetyTestNS)).To(Succeed())
	})

	AfterAll(func() {
		By("tearing down Beyla")
		tearDownBeyla()

		By("tearing down otel-lgtm")
		_ = k8sClient.Resources().Delete(suiteCtx,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: lgtmNS}})

		By("tearing down all safety test apps")
		_ = k8sClient.Resources().Delete(suiteCtx,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: safetyTestNS}})
	})

	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		dumpPodLogs("controller-manager", ctrlNamespace, "control-plane=controller-manager")
		dumpPodLogs("beyla", ctrlNamespace, "app.kubernetes.io/name=beyla")
		dumpPodLogs("safety-python-old", safetyTestNS, "app="+safetyPythonOldApp)
		dumpPodLogs("safety-python-conflict", safetyTestNS, "app="+safetyPythonConflictApp)
		dumpPodLogs("safety-nodejs-old", safetyTestNS, "app="+safetyNodejsOldApp)
		dumpPodLogs("safety-nodejs-sdk-installed", safetyTestNS, "app="+safetyNodejsSdkInstalledApp)
		dumpPodLogs("safety-nodejs-sdk-required", safetyTestNS, "app="+safetyNodejsSdkRequiredApp)
		dumpPodLogs("safety-java-graalvm", safetyTestNS, "app="+safetyJavaGraalvmApp)
		dumpPodLogs("safety-dotnet-aot", safetyTestNS, "app="+safetyDotnetAOTApp)
		dumpPodLogs("safety-nodejs-esm", safetyTestNS, "app="+safetyNodejsESMApp)
		dumpInjectionConfigMaps()
	})

	SetDefaultEventuallyTimeout(5 * time.Minute)
	SetDefaultEventuallyPollingInterval(10 * time.Second)

	// ---- Act 1: Class-1 apps running, uninstrumented ----

	It("Tempo is empty before any safety apps are deployed", func() {
		By("asserting Tempo has no spans for any safety app service name")
		// No apps are running yet — this confirms the telemetry backend is
		// reachable and clean before the injection pipeline is exercised.
		for _, svcName := range []string{
			safetyPythonOldApp,
			safetyPythonConflictApp,
			safetyNodejsOldApp,
			safetyNodejsSdkInstalledApp,
			safetyNodejsSdkRequiredApp,
			safetyJavaGraalvmApp,
			safetyDotnetAOTApp,
			safetyNodejsESMApp,
		} {
			_, n, err := tempoHasTraces(tempoBaseURL,
				fmt.Sprintf(`{ resource.service.name = "%s" }`, svcName),
			)
			Expect(err).NotTo(HaveOccurred(), "Tempo query failed for %s", svcName)
			Expect(n).To(Equal(0), "unexpected spans for %s before any apps are deployed", svcName)
		}
	})

	// ---- Act 2: Beyla deploys, Class-1 injection triggered, Class-2 apps deploy ----

	It("Beyla deploys, writes an injection ConfigMap, and Class-1 apps are instrumented at admission", func() {
		manifestsDir := filepath.Join(projectDir, "test", "e2e", "manifests")

		By("deploying the real Beyla DaemonSet wired to the controller")
		Expect(deployBeyla(safetyTestNS)).To(Succeed())

		By("waiting for the Beyla DaemonSet to be ready")
		waitBeylaReady()

		By("waiting until Beyla writes a per-node injection ConfigMap")
		Eventually(func(g Gomega) {
			var cms corev1.ConfigMapList
			g.Expect(k8sClient.Resources(ctrlNamespace).List(suiteCtx, &cms,
				resources.WithLabelSelector("beyla.grafana.com/node"))).To(Succeed())
			g.Expect(cms.Items).NotTo(BeEmpty(), "Beyla has not published a per-node ConfigMap yet")
		}, 5*time.Minute, 5*time.Second).Should(Succeed())

		By("confirming the injection registry is active for safety-test via canary pods")
		// The controller loads Beyla's ConfigMap into its registry asynchronously.
		// We poll by creating successive pause pods until one is annotated by the
		// webhook, confirming the namespace rule is live. Class-1 apps deployed
		// immediately after are then instrumented at admission — no dependency on
		// eligible_for_restart, which Beyla may not write for all runtimes (e.g.
		// Python 3.8 is intentionally excluded by Beyla's startup compatibility
		// filter; the webhook still instruments such pods when admitted directly).
		// Each failed Eventually iteration creates a new pod (canary-N) since the
		// admission verdict is fixed at create time — we can't re-check an existing
		// pod. DeferCleanup sweeps any orphans at the end of the spec.
		var canaryAttempt int
		DeferCleanup(func() {
			var pods corev1.PodList
			if err := k8sClient.Resources(safetyTestNS).List(suiteCtx, &pods); err != nil {
				return
			}
			for i := range pods.Items {
				if strings.HasPrefix(pods.Items[i].Name, "canary-") {
					_ = k8sClient.Resources().Delete(suiteCtx, &pods.Items[i])
				}
			}
		})
		Eventually(func(g Gomega) {
			canaryAttempt++
			canary := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("canary-%d", canaryAttempt),
					Namespace: safetyTestNS,
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:  "pause",
						Image: "registry.k8s.io/pause:3.10",
					}},
				},
			}
			g.Expect(k8sClient.Resources().Create(suiteCtx, canary)).To(Succeed())
			var admitted corev1.Pod
			g.Expect(k8sClient.Resources().Get(suiteCtx, canary.Name, safetyTestNS, &admitted)).To(Succeed())
			g.Expect(admitted.Annotations).To(HaveKey(injectAnno),
				"canary pod %s not yet annotated; registry may not be populated", canary.Name)
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("deploying Class-1 safety apps — their pods are instrumented at webhook admission")
		Expect(applyManifestFile(filepath.Join(manifestsDir, "instrumentation-safety-apps.yaml"))).To(Succeed())

		By("waiting for Class-1 app Deployments to roll out")
		for _, app := range []string{
			safetyPythonOldApp,
			safetyPythonConflictApp,
			safetyNodejsOldApp,
			safetyNodejsSdkInstalledApp,
			safetyNodejsSdkRequiredApp,
		} {
			waitDeploymentReady(app, safetyTestNS, 5*time.Minute)
		}
	})

	It("deploys Class-2 resilience apps after the injection ConfigMap is active", func() {
		manifestsDir := filepath.Join(projectDir, "test", "e2e", "manifests")

		By("deploying GraalVM native image, .NET Native AOT, and ESM Node.js apps")
		// Class-2 apps are deployed here — after the controller's registry is
		// populated — so that the webhook instruments their pods on admission.
		// Beyla's eBPF scanner may not recognise native binaries as Java or .NET
		// processes and therefore may not add them to eligible_for_restart; the
		// webhook-driven path works regardless.
		Expect(applyManifestFile(filepath.Join(manifestsDir, "instrumentation-safety-native-apps.yaml"))).To(Succeed())

		By("waiting for Class-2 app Deployments to roll out")
		for _, app := range []string{safetyJavaGraalvmApp, safetyDotnetAOTApp, safetyNodejsESMApp} {
			waitDeploymentReady(app, safetyTestNS, 5*time.Minute)
		}
	})

	// ---- Act 3: safety gates fire (Class-1) and resilience holds (Class-2) ----

	Context("Python", func() {
		Context("Python < 3.9", func() {
			It("pod stays Running and check_python_version skips instrumentation", func() {
				By("waiting for an instrumented, ready Python 3.8 pod")
				waitInstrumentedReadyPod(safetyTestNS, "app="+safetyPythonOldApp)
				assertSafetySkip(safetyPythonOldApp, logPythonVersionCheck)
			})
		})

		Context("conflicting opentelemetry-api version", func() {
			It("pod stays Running and verify_and_load skips instrumentation", func() {
				By("waiting for an instrumented, ready Python conflict pod")
				waitInstrumentedReadyPod(safetyTestNS, "app="+safetyPythonConflictApp)
				assertSafetySkip(safetyPythonConflictApp, logPythonConflict)
			})
		})
	})

	Context("Node.js", func() {
		Context("unsupported runtime version (Node.js 16)", func() {
			It("pod stays Running and version check skips instrumentation", func() {
				By("waiting for an instrumented, ready Node.js 16 pod")
				waitInstrumentedReadyPod(safetyTestNS, "app="+safetyNodejsOldApp)
				assertSafetySkip(safetyNodejsOldApp, logNodejsVersionCheck)
			})
		})

		Context("@opentelemetry/sdk-node present in node_modules", func() {
			It("pod stays Running and isOtelSdkInstalled skips instrumentation", func() {
				By("waiting for an instrumented, ready pod with pre-installed OTel SDK")
				waitInstrumentedReadyPod(safetyTestNS, "app="+safetyNodejsSdkInstalledApp)
				assertSafetySkip(safetyNodejsSdkInstalledApp, logNodejsSdkInstalled)
			})
		})

		Context("OTel SDK required via CMD --require", func() {
			It("pod stays Running and isOtelSdkRequiredViaArgs skips instrumentation", func() {
				By("waiting for an instrumented, ready pod with OTel required via args")
				waitInstrumentedReadyPod(safetyTestNS, "app="+safetyNodejsSdkRequiredApp)
				assertSafetySkip(safetyNodejsSdkRequiredApp, logNodejsSdkRequired)
			})
		})

		Context("ES module app (type: module)", func() {
			It("pod stays Running when register.js is loaded into an ESM app", func() {
				// register.js is loaded via --require in CJS context before the ESM
				// module system initialises. Whether OTel successfully instruments the
				// ESM app depends on Node.js internals; the test only asserts the app
				// does not crash (no CrashLoopBackOff). Tempo is not checked because
				// spans may legitimately appear if instrumentation succeeds.
				By("waiting for an instrumented, ready ESM Node.js pod")
				waitInstrumentedReadyPod(safetyTestNS, "app="+safetyNodejsESMApp)
				assertSafetyResilience(safetyNodejsESMApp)
			})
		})
	})

	Context("Java GraalVM native image", func() {
		It("pod stays Running after injection into a native binary", func() {
			// libotelinject.so is preloaded into the native binary via LD_PRELOAD and
			// sets JAVA_TOOL_OPTIONS=-javaagent:... A GraalVM native image has no JVM,
			// so the env var is ignored and no Java agent runs. The test verifies the
			// binary starts and stays Running — i.e. LD_PRELOAD of libotelinject.so
			// does not crash a native process. This is injection resilience: the
			// agent never loads, not because a script skipped it but because there
			// is no runtime to interpret the hook.
			By("waiting for an instrumented, ready GraalVM native pod")
			waitInstrumentedReadyPod(safetyTestNS, "app="+safetyJavaGraalvmApp)
			assertSafetyResilience(safetyJavaGraalvmApp)
		})
	})

	Context(".NET Native AOT", func() {
		It("pod stays Running after injection into a native AOT binary", func() {
			// libotelinject.so sets DOTNET_STARTUP_HOOKS pointing to the OTel .NET
			// hook DLL. A Native AOT binary has no CLR to process startup hooks, so
			// the env var is ignored and no .NET SDK is loaded. The test verifies
			// the binary starts and stays Running — same resilience class as GraalVM.
			By("waiting for an instrumented, ready .NET Native AOT pod")
			waitInstrumentedReadyPod(safetyTestNS, "app="+safetyDotnetAOTApp)
			assertSafetyResilience(safetyDotnetAOTApp)
		})
	})
})

// assertSafetySkip verifies Class-1 safety: the inject-sdk-image runtime script
// (sitecustomize.py / register.js) detected an incompatible app and skipped SDK
// initialisation while leaving the app running. It asserts both signals: no
// spans reach Tempo, and the expected skip message appears in the pod logs.
//
// The 30-second sleep absorbs the worst-case time between pod Ready and the
// first failed export attempt: register.js / sitecustomize.py run before the
// app's main entrypoint, so any span would be produced within seconds of Ready;
// 30s leaves comfortable headroom for OTLP retry/backoff before we assert
// absence. Tempo's search API is "instant" (no ingest delay matters for
// "zero traces"), so the sleep does not need to cover ingest.
func assertSafetySkip(appName, expectedLogMessage string) {
	By(fmt.Sprintf("waiting 30s before asserting no spans for service %q", appName))
	time.Sleep(30 * time.Second)

	By(fmt.Sprintf("asserting Tempo has no spans for service %q", appName))
	_, n, err := tempoHasTraces(tempoBaseURL,
		fmt.Sprintf(`{ resource.service.name = "%s" }`, appName),
	)
	Expect(err).NotTo(HaveOccurred(), "Tempo query failed for %s", appName)
	Expect(n).To(Equal(0),
		"unexpected spans for %s: safety mechanism should have prevented SDK initialisation", appName)

	By(fmt.Sprintf("asserting pod logs contain skip message %q", expectedLogMessage))
	assertPodLogsContain(safetyTestNS, "app="+appName, expectedLogMessage)
}

// assertSafetyResilience verifies Class-2 safety: the injector (libotelinject.so
// via LD_PRELOAD) and the language runtime tolerated the injection attempt
// without crashing the application, even though the SDK cannot meaningfully
// run (native binaries with no JVM/CLR; ESM apps where register.js loads in
// CJS context). Whether spans appear depends on runtime internals and is not
// part of the safety contract here — only that the pod stays Running. The
// 30-second sleep matches assertSafetySkip's rationale: long enough that an
// induced crash would have surfaced before we check.
func assertSafetyResilience(appName string) {
	By(fmt.Sprintf("waiting 30s before asserting pod %q is still Running", appName))
	time.Sleep(30 * time.Second)

	By("asserting the pod is still Running and Ready (injection did not crash the app)")
	assertPodRunning(safetyTestNS, "app="+appName)
}

// assertPodLogsContain asserts that the first pod matching selector in ns has
// logs containing the expected substring.
func assertPodLogsContain(ns, selector, expected string) {
	var pods corev1.PodList
	Expect(k8sClient.Resources(ns).List(suiteCtx, &pods,
		resources.WithLabelSelector(selector))).To(Succeed())
	Expect(pods.Items).NotTo(BeEmpty(), "no pods found for %s in %s", selector, ns)
	logs, err := podLogs(ns, pods.Items[0].Name)
	Expect(err).NotTo(HaveOccurred(), "failed to get logs for pod %s/%s", ns, pods.Items[0].Name)
	Expect(logs).To(ContainSubstring(expected),
		"pod logs do not contain expected skip message")
}

// assertPodRunning asserts that the first pod matching selector in ns is in the
// Running phase and its Ready condition is True.
func assertPodRunning(ns, selector string) {
	var pods corev1.PodList
	Expect(k8sClient.Resources(ns).List(suiteCtx, &pods,
		resources.WithLabelSelector(selector))).To(Succeed())
	Expect(pods.Items).NotTo(BeEmpty(), "no pods found for %s in %s", selector, ns)
	p := &pods.Items[0]
	Expect(p.Status.Phase).To(Equal(corev1.PodRunning),
		"pod %s/%s is not Running after injection", ns, p.Name)
	Expect(podReady(p)).To(BeTrue(),
		"pod %s/%s is not Ready after injection", ns, p.Name)
}
