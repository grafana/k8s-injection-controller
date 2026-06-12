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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/docker/docker/api/types/build"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/moby/go-archive"

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
)

var (
	sdkNodejsImage     = "sdk-nodejs-app:dev"
	sdkPythonImage     = "sdk-python-app:dev"
	sdkPythonMuslImage = "sdk-python-musl-app:dev"
	sdkJavaImage       = "sdk-java-app:dev"
	sdkDotnetImage     = "sdk-dotnet-app:dev"
	sdkDotnetMuslImage = "sdk-dotnet-musl-app:dev"
)

// sdkAppDirs returns, in build order, each SDK test app's source directory and
// the image tag to assign it. Used by BeforeSuite to build and load all images.
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

var _ = Describe("SDK auto-instrumentation pipeline", Ordered, func() {
	// This suite exercises the full Beyla -> k8s-injection-controller ->
	// inject-sdk-image -> LGTM pipeline in three acts:
	//
	//   1. Apps running, Beyla absent: Tempo is empty for all SDK app services.
	//   2. Beyla deploys and writes its per-node injection ConfigMap.
	//   3. Each language SDK is installed and spans flow to Tempo.
	//
	// The ordering is enforced by Ginkgo's Ordered container: each spec sees
	// the state left by the previous one, so the "no spans" assertion in act 1
	// genuinely precedes Beyla deployment in act 2.

	BeforeAll(func() {
		manifestsDir := filepath.Join(projectDir, "test", "e2e", "manifests")

		// The safety suite (e2e_instrumentation_safety_test.go) sorts before
		// this file and deletes e2e-lgtm in its AfterAll. Wait for it to be
		// fully gone before trying to recreate it.
		By("waiting for previous lgtm namespace to finish terminating")
		waitNamespaceDeleted(lgtmNS, 2*time.Minute)

		By("deploying grafana/otel-lgtm")
		Expect(applyManifestFile(filepath.Join(manifestsDir, "instrumentation-lgtm.yaml"))).To(Succeed())

		By("deploying instrumentation test apps and load generators")
		Expect(applyManifestFile(filepath.Join(manifestsDir, "instrumentation-apps.yaml"))).To(Succeed())

		By("waiting for otel-lgtm to be ready")
		waitDeploymentReady("lgtm", lgtmNS, 5*time.Minute)

		By("waiting for SDK app Deployments to roll out")
		for _, app := range []string{sdkNodejsApp, sdkPythonApp, sdkPythonMuslApp, sdkJavaApp, sdkDotnetApp, sdkDotnetMuslApp} {
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
	SetDefaultEventuallyPollingInterval(10 * time.Second)

	// ---- Act 1: apps are running and receiving traffic, but uninstrumented ----

	It("apps produce no telemetry before Beyla instruments them", func() {
		By("letting load generators drive traffic to the uninstrumented apps")
		// Apps are Ready (BeforeAll waited for rollouts) and load generators are
		// already curling them. Wait long enough for any hypothetical spans to
		// have been exported before asserting the absence.
		time.Sleep(15 * time.Second)

		By("asserting Tempo has no spans for any SDK app service")
		for _, svcName := range []string{sdkNodejsApp, sdkPythonApp, sdkPythonMuslApp, sdkJavaApp, sdkDotnetApp, sdkDotnetMuslApp} {
			_, n, err := tempoHasTraces(tempoBaseURL,
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
		// Beyla stamps the ConfigMap with the beyla.grafana.com/node annotation
		// (the same key it uses as the selector annotation on the ConfigMap itself).
		Eventually(func(g Gomega) {
			var cms corev1.ConfigMapList
			g.Expect(k8sClient.Resources(ctrlNamespace).List(suiteCtx, &cms,
				resources.WithLabelSelector("beyla.grafana.com/node"))).To(Succeed())
			g.Expect(cms.Items).NotTo(BeEmpty(), "Beyla has not published a per-node injection ConfigMap yet")
		}, 5*time.Minute, 5*time.Second).Should(Succeed())
	})

	// ---- Act 3: each language SDK is installed and spans reach Tempo ----

	Context("Node.js", func() {
		It("instruments an Express app and emits HTTP server spans", func() {
			By("waiting for an instrumented, ready Node.js pod")
			waitInstrumentedReadyPod(sdkTestNS, "app="+sdkNodejsApp)
			By("asserting HTTP server spans arrive in Tempo for service " + sdkNodejsApp)
			assertTempoHasSpansForService(sdkNodejsApp)
		})
	})

	Context("Python", func() {
		Context("glibc (python:3.11-slim)", func() {
			It("instruments a Flask app and emits HTTP server spans", func() {
				By("waiting for an instrumented, ready Python pod")
				waitInstrumentedReadyPod(sdkTestNS, "app="+sdkPythonApp)
				By("asserting HTTP server spans arrive in Tempo for service " + sdkPythonApp)
				assertTempoHasSpansForService(sdkPythonApp)
			})
		})

		Context("musl (python:3.11-alpine)", func() {
			It("instruments a Flask app and emits HTTP server spans", func() {
				By("waiting for an instrumented, ready Python musl pod")
				waitInstrumentedReadyPod(sdkTestNS, "app="+sdkPythonMuslApp)
				By("asserting HTTP server spans arrive in Tempo for service " + sdkPythonMuslApp)
				assertTempoHasSpansForService(sdkPythonMuslApp)
			})
		})
	})

	Context("Java", func() {
		It("instruments a JDK HTTP server app and emits HTTP server spans", func() {
			By("waiting for an instrumented, ready Java pod")
			waitInstrumentedReadyPod(sdkTestNS, "app="+sdkJavaApp)
			By("asserting HTTP server spans arrive in Tempo for service " + sdkJavaApp)
			assertTempoHasSpansForService(sdkJavaApp)
		})
	})

	Context(".NET", func() {
		Context("glibc (mcr.microsoft.com/dotnet/aspnet:9.0)", func() {
			It("instruments an ASP.NET Core app and emits HTTP server spans", func() {
				By("waiting for an instrumented, ready .NET pod")
				waitInstrumentedReadyPod(sdkTestNS, "app="+sdkDotnetApp)
				By("asserting HTTP server spans arrive in Tempo for service " + sdkDotnetApp)
				assertTempoHasSpansForService(sdkDotnetApp)
			})
		})

		Context("musl (mcr.microsoft.com/dotnet/aspnet:9.0-alpine)", func() {
			It("instruments an ASP.NET Core app and emits HTTP server spans", func() {
				By("waiting for an instrumented, ready .NET musl pod")
				waitInstrumentedReadyPod(sdkTestNS, "app="+sdkDotnetMuslApp)
				By("asserting HTTP server spans arrive in Tempo for service " + sdkDotnetMuslApp)
				assertTempoHasSpansForService(sdkDotnetMuslApp)
			})
		})
	})
})

// assertTempoHasSpansForService queries Tempo until at least one trace exists
// for the given service name, or the Eventually timeout fires.
func assertTempoHasSpansForService(serviceName string) {
	Eventually(func(g Gomega) {
		matched, n, err := tempoHasTraces(tempoBaseURL,
			fmt.Sprintf(`{ resource.service.name = "%s" }`, serviceName),
			fmt.Sprintf(`{ resource.service.name =~ "%s.*" }`, serviceName),
		)
		g.Expect(err).NotTo(HaveOccurred(), "Tempo query failed")
		g.Expect(n).To(BeNumerically(">", 0),
			"no HTTP server spans in Tempo for service %q yet (last query: %s)", serviceName, matched)
	}).Should(Succeed())
}

// buildSDKAppImages builds all SDK test app Docker images. Each app lives in
// its own subdirectory under test/e2e/apps/ with its own Dockerfile. The images
// are loaded into Kind by BeforeSuite immediately after this returns.
func buildSDKAppImages(ctx context.Context, projectDir string) error {
	for _, app := range sdkAppDirs(projectDir) {
		if err := buildImage(ctx, app.dir, app.tag); err != nil {
			return fmt.Errorf("building %s: %w", app.tag, err)
		}
	}
	return nil
}

// buildImage builds a Docker image from dir, tagging it as tag. It uses the
// Docker Engine Go SDK so no `docker` CLI is required. The entire dir is used
// as the build context (app directories are small; no exclusions needed).
func buildImage(ctx context.Context, dir, tag string) error {
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("creating docker client: %w", err)
	}
	defer func() { _ = cli.Close() }()

	buildContext, err := archive.TarWithOptions(dir, &archive.TarOptions{})
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
