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

	// Substrings emitted by the inject-sdk-image runtime scripts in
	// grafana/beyla. If upstream rewords any, update here.
	//   pkg/webhook/image/python/sitecustomize.py: check_python_version, verify_and_load
	//   pkg/webhook/image/node.js/register.js:     version range, isOtelSdkInstalled, isOtelSdkRequiredViaArgs
	logPythonVersionCheck = "Python version 3.9 or higher required"
	logPythonConflict     = "Not importing OpenTelemetry Python auto-instrumentation"
	logNodejsVersionCheck = "does not support Node.js version"
	logNodejsSdkInstalled = "already instrumented with OpenTelemetry"
	logNodejsSdkRequired  = "already using OpenTelemetry auto-instrumentation"

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

var _ = Describe("SDK auto-instrumentation safety", Ordered, func() {
	// Two classes of safety behaviour:
	//   Class 1 — SDK skip: sitecustomize.py / register.js detect incompatible
	//     apps and skip initialisation without crashing.
	//   Class 2 — Injection resilience: libotelinject.so (LD_PRELOAD) doesn't
	//     crash native runtimes (GraalVM, .NET AOT) or ESM Node.js that can't
	//     usefully load the SDK.
	//
	// Three acts: (1) Class-1 apps running, Beyla absent → Tempo empty;
	// (2) Beyla deploys, Class-1 apps deploy and are instrumented at admission,
	// Class-2 apps follow; (3) per-app assertions on the specific safety gate.
	//
	// check_otlp_proto is omitted: Beyla always writes
	// OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf, so that gate is unreachable
	// via the normal pipeline.

	BeforeAll(func() {
		// The wait-for-deletion calls are load-bearing only on
		// KIND_KEEP_CLUSTER=true re-runs.
		By("waiting for previous lgtm namespace to finish terminating")
		waitNamespaceDeleted(lgtmNS)

		By("waiting for previous safety-test namespace to finish terminating")
		waitNamespaceDeleted(safetyTestNS)

		By("deploying grafana/otel-lgtm")
		Expect(applyManifestFile(filepath.Join(manifestsDir, "instrumentation-lgtm.yaml"))).To(Succeed())

		By("waiting for otel-lgtm to be ready")
		waitDeploymentReady("lgtm", lgtmNS, 5*time.Minute)

		By("ensuring the safety-test namespace exists for canary pods in act 2")
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
			_, n, err := tempoHasTraces(
				fmt.Sprintf(`{ resource.service.name = "%s" }`, svcName),
			)
			Expect(err).NotTo(HaveOccurred(), "Tempo query failed for %s", svcName)
			Expect(n).To(Equal(0), "unexpected spans for %s before any apps are deployed", svcName)
		}
	})

	// ---- Act 2: Beyla deploys, Class-1 injection triggered, Class-2 apps deploy ----

	It("Beyla deploys, writes an injection ConfigMap, and Class-1 apps are instrumented at admission", func() {
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
		// Admission verdict is fixed at create time, so each iteration creates
		// a fresh canary-N pod until one is annotated. DeferCleanup sweeps them.
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
		Expect(applyManifestFile(filepath.Join(manifestsDir, "instrumentation-safety-class1-apps.yaml"))).To(Succeed())

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
		By("deploying GraalVM native image, .NET Native AOT, and ESM Node.js apps")
		// Beyla's eBPF scanner may not recognise native binaries, so these
		// rely on the webhook-driven path rather than eligible_for_restart.
		Expect(applyManifestFile(filepath.Join(manifestsDir, "instrumentation-safety-class2-apps.yaml"))).To(Succeed())

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
				// register.js loads via --require in CJS context before the ESM
				// loader runs. The test only asserts the app doesn't crash.
				By("waiting for an instrumented, ready ESM Node.js pod")
				waitInstrumentedReadyPod(safetyTestNS, "app="+safetyNodejsESMApp)
				assertSafetyResilience(safetyNodejsESMApp)
			})
		})
	})

	Context("Java GraalVM native image", func() {
		It("pod stays Running after injection into a native binary", func() {
			// No JVM to process JAVA_TOOL_OPTIONS, so LD_PRELOAD of
			// libotelinject.so is a no-op — just verify it doesn't crash.
			By("waiting for an instrumented, ready GraalVM native pod")
			waitInstrumentedReadyPod(safetyTestNS, "app="+safetyJavaGraalvmApp)
			assertSafetySkip(safetyJavaGraalvmApp, "")
		})
	})

	Context(".NET Native AOT", func() {
		It("pod stays Running after injection into a native AOT binary", func() {
			// No CLR to process DOTNET_STARTUP_HOOKS — same resilience class
			// as GraalVM.
			By("waiting for an instrumented, ready .NET Native AOT pod")
			waitInstrumentedReadyPod(safetyTestNS, "app="+safetyDotnetAOTApp)
			assertSafetySkip(safetyDotnetAOTApp, "")
		})
	})
})

// assertSafetySkip verifies Class-1: no spans reach Tempo and the expected
// skip message appears in pod logs. The 30s sleep gives OTLP retry/backoff
// headroom before we assert absence.
func assertSafetySkip(appName, expectedLogMessage string) {
	By(fmt.Sprintf("waiting 30s before asserting no spans for service %q", appName))
	time.Sleep(30 * time.Second)

	By(fmt.Sprintf("asserting Tempo has no spans for service %q", appName))
	_, n, err := tempoHasTraces(
		fmt.Sprintf(`{ resource.service.name = "%s" }`, appName),
	)
	Expect(err).NotTo(HaveOccurred(), "Tempo query failed for %s", appName)
	Expect(n).To(Equal(0),
		"unexpected spans for %s: safety mechanism should have prevented SDK initialisation", appName)

	By(fmt.Sprintf("asserting pod logs contain skip message %q", expectedLogMessage))
	assertPodLogsContain(safetyTestNS, "app="+appName, expectedLogMessage)
}

// assertSafetyResilience verifies Class-2: the pod stays Running and Ready
// despite the injection. Spans may or may not appear; not part of the contract.
func assertSafetyResilience(appName string) {
	By(fmt.Sprintf("waiting 30s before asserting pod %q is still Running", appName))
	time.Sleep(30 * time.Second)

	By("asserting the pod is still Running and Ready (injection did not crash the app)")
	assertPodRunning(safetyTestNS, "app="+appName)
}

// assertPodLogsContain asserts the first matching pod's logs contain expected.
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

// assertPodRunning asserts the first matching pod is Running and Ready.
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
