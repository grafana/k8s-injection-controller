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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
)

const (
	sdkTestNS        = "sdk-test"
	sdkNodejsApp     = "sdk-nodejs"
	sdkPythonApp     = "sdk-python"
	sdkPythonMuslApp = "sdk-python-musl"
	sdkJavaApp       = "sdk-java"
	sdkDotnetApp     = "sdk-dotnet"
	sdkDotnetMuslApp = "sdk-dotnet-musl"

	sdkNodejsImage     = "sdk-nodejs-app:dev"
	sdkPythonImage     = "sdk-python-app:dev"
	sdkPythonMuslImage = "sdk-python-musl-app:dev"
	sdkJavaImage       = "sdk-java-app:dev"
	sdkDotnetImage     = "sdk-dotnet-app:dev"
	sdkDotnetMuslImage = "sdk-dotnet-musl-app:dev"
)

// sdkAppDirs returns each SDK test app's source directory and image tag.
func sdkAppDirs(projectDir string) []struct{ dir, tag string } {
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

var sdkApps = []string{
	sdkNodejsApp, sdkPythonApp, sdkPythonMuslApp,
	sdkJavaApp, sdkDotnetApp, sdkDotnetMuslApp,
}

var _ = Describe("SDK auto-instrumentation pipeline", Ordered, func() {
	// Three acts under Ginkgo's Ordered container:
	//   1. Apps running, Beyla absent: Tempo is empty.
	//   2. Beyla deploys and writes its per-node injection ConfigMap.
	//   3. Each language SDK is installed and spans flow to Tempo.

	BeforeAll(func() {
		// The safety suite sorts before this one and deletes e2e-lgtm in its
		// AfterAll. Wait for it to be fully gone before recreating it.
		By("waiting for previous lgtm namespace to finish terminating")
		waitNamespaceDeleted(lgtmNS)

		By("deploying grafana/otel-lgtm")
		Expect(applyManifestFile(filepath.Join(manifestsDir, "instrumentation-lgtm.yaml"))).To(Succeed())

		By("deploying instrumentation test apps and load generators")
		Expect(applyManifestFile(filepath.Join(manifestsDir, "instrumentation-apps.yaml"))).To(Succeed())

		By("waiting for otel-lgtm to be ready")
		waitDeploymentReady("lgtm", lgtmNS, 5*time.Minute)

		By("waiting for SDK app Deployments to roll out")
		for _, app := range sdkApps {
			waitDeploymentReady(app, sdkTestNS, 5*time.Minute)
		}
	})

	AfterAll(func() {
		By("tearing down Beyla")
		tearDownBeyla()

		By("tearing down otel-lgtm")
		_ = k8sClient.Resources().Delete(suiteCtx,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: lgtmNS}})

		By("tearing down SDK test apps")
		_ = k8sClient.Resources().Delete(suiteCtx,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: sdkTestNS}})
	})

	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		dumpPodLogs("controller-manager", ctrlNamespace, "control-plane=controller-manager")
		dumpPodLogs("beyla", ctrlNamespace, "app.kubernetes.io/name=beyla")
		dumpPodLogs("sdk-nodejs", sdkTestNS, "app="+sdkNodejsApp)
		dumpPodLogs("sdk-python", sdkTestNS, "app="+sdkPythonApp)
		dumpPodLogs("sdk-python-musl", sdkTestNS, "app="+sdkPythonMuslApp)
		dumpPodLogs("sdk-java", sdkTestNS, "app="+sdkJavaApp)
		dumpPodLogs("sdk-dotnet", sdkTestNS, "app="+sdkDotnetApp)
		dumpPodLogs("sdk-dotnet-musl", sdkTestNS, "app="+sdkDotnetMuslApp)
		dumpInjectionConfigMaps()
	})

	SetDefaultEventuallyTimeout(5 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	// ---- Act 1: apps are running and receiving traffic, but uninstrumented ----

	It("apps produce no telemetry before Beyla instruments them", func() {
		By("letting load generators drive traffic to the uninstrumented apps")
		// Small buffer in case anything is in flight; apps have no SDK so 0
		// spans is the steady state.
		time.Sleep(5 * time.Second)

		By("asserting Tempo has no spans for any SDK app service")
		for _, svcName := range sdkApps {
			_, n, err := tempoHasTraces(
				fmt.Sprintf(`{ resource.service.name = "%s" }`, svcName),
			)
			Expect(err).NotTo(HaveOccurred(), "Tempo query failed for %s", svcName)
			Expect(n).To(Equal(0), "unexpected spans for %s before Beyla instruments it", svcName)
		}
	})

	// ---- Act 2: Beyla deploys and triggers injection ----

	It("Beyla deploys and writes a per-node injection ConfigMap", func() {
		By("deploying the real Beyla DaemonSet wired to the controller")
		Expect(deployBeyla(sdkTestNS)).To(Succeed())

		By("waiting for the Beyla DaemonSet to be ready")
		waitBeylaReady()

		By("waiting until Beyla writes a per-node injection ConfigMap")
		Eventually(func(g Gomega) {
			var cms corev1.ConfigMapList
			g.Expect(k8sClient.Resources(ctrlNamespace).List(suiteCtx, &cms,
				resources.WithLabelSelector("beyla.grafana.com/node"))).To(Succeed())
			g.Expect(cms.Items).NotTo(BeEmpty(), "Beyla has not published a per-node injection ConfigMap yet")
		}, 5*time.Minute, 5*time.Second).Should(Succeed())

		By("verifying every SDK app pod is instrumented and Ready")
		// The controller rolls every matching Deployment once it processes
		// Beyla's ConfigMap, so all apps in sdkTestNS become instrumented as a
		// group — Act-3 specs can skip the per-app readiness check.
		for _, app := range sdkApps {
			waitInstrumentedReadyPod(sdkTestNS, "app="+app)
		}
	})

	// ---- Act 3: each language SDK is installed and spans reach Tempo ----

	Context("Node.js", func() {
		It("instruments an Express app and emits HTTP server spans", func() {
			assertTempoHasSpansForService(sdkNodejsApp)
		})
	})

	Context("Python", func() {
		Context("glibc (python:3.11-slim)", func() {
			It("instruments a Flask app and emits HTTP server spans", func() {
				assertTempoHasSpansForService(sdkPythonApp)
			})
		})

		Context("musl (python:3.11-alpine)", func() {
			It("instruments a Flask app and emits HTTP server spans", func() {
				assertTempoHasSpansForService(sdkPythonMuslApp)
			})
		})
	})

	Context("Java", func() {
		It("instruments a JDK HTTP server app and emits HTTP server spans", func() {
			assertTempoHasSpansForService(sdkJavaApp)
		})
	})

	Context(".NET", func() {
		Context("glibc (mcr.microsoft.com/dotnet/aspnet:9.0)", func() {
			It("instruments an ASP.NET Core app and emits HTTP server spans", func() {
				assertTempoHasSpansForService(sdkDotnetApp)
			})
		})

		Context("musl (mcr.microsoft.com/dotnet/aspnet:9.0-alpine)", func() {
			It("instruments an ASP.NET Core app and emits HTTP server spans", func() {
				assertTempoHasSpansForService(sdkDotnetMuslApp)
			})
		})
	})
})

// assertTempoHasSpansForService polls Tempo until at least one trace exists
// for the given service name.
func assertTempoHasSpansForService(serviceName string) {
	Eventually(func(g Gomega) {
		matched, n, err := tempoHasTraces(
			fmt.Sprintf(`{ resource.service.name = "%s" }`, serviceName),
			fmt.Sprintf(`{ resource.service.name =~ "%s.*" }`, serviceName),
		)
		g.Expect(err).NotTo(HaveOccurred(), "Tempo query failed")
		g.Expect(n).To(BeNumerically(">", 0),
			"no HTTP server spans in Tempo for service %q yet (last query: %s)", serviceName, matched)
	}).Should(Succeed())
}
