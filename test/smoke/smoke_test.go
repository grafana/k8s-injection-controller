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

package smoke_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
)

const (
	sdkNodejsApp     = "sdk-nodejs"
	sdkPythonApp     = "sdk-python"
	sdkPythonMuslApp = "sdk-python-musl"
	sdkJavaApp       = "sdk-java"
	sdkDotnetApp     = "sdk-dotnet"
	sdkDotnetMuslApp = "sdk-dotnet-musl"
)

var sdkApps = []string{
	sdkNodejsApp, sdkPythonApp, sdkPythonMuslApp,
	sdkJavaApp, sdkDotnetApp, sdkDotnetMuslApp,
}

var _ = Describe("SDK auto-instrumentation smoke test (k8s-monitoring-helm)", Ordered, func() {
	// The suite deploys Beyla and the injection controller via k8s-monitoring-helm
	// in BeforeSuite. This test:
	//   1. Deploys the SDK test apps into sdkSmokeTestNS.
	//   2. Waits for Beyla to scan the namespace, write per-node ConfigMaps, and
	//      the controller to roll every Deployment with injection applied.
	//   3. Asserts traces and metrics reach the bundled otel-lgtm stack.
	//
	// This is intentionally a happy-path smoke test — it validates the end-to-end
	// wiring of k8s-monitoring-helm → Beyla → k8s-injection-controller → SDK →
	// LGTM with a locally-built controller image, catching integration breaks
	// before a controller release.

	BeforeAll(func() {
		By("deploying SDK test apps into " + sdkSmokeTestNS)
		Expect(applyManifestFile(filepath.Join(smokeManifestsDir, "instrumentation-apps.yaml"))).To(Succeed())

		By("waiting for SDK app Deployments to roll out (uninstrumented)")
		for _, app := range sdkApps {
			waitDeploymentReady(app, sdkSmokeTestNS, 5*time.Minute)
		}
	})

	AfterAll(func() {
		By("tearing down SDK test apps")
		_ = k8sClient.Resources().Delete(suiteCtx,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: sdkSmokeTestNS}})
	})

	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		dumpPodLogs("controller-manager", smokeNamespace, "control-plane=controller-manager")
		dumpPodLogs("beyla", smokeNamespace, "app.kubernetes.io/name=beyla")
		for _, app := range sdkApps {
			dumpPodLogs(app, sdkSmokeTestNS, "app="+app)
		}
		dumpInjectionConfigMaps()
	})

	SetDefaultEventuallyTimeout(5 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("Beyla writes a per-node injection ConfigMap for the SDK test namespace", func() {
		Eventually(func(g Gomega) {
			var cms corev1.ConfigMapList
			g.Expect(k8sClient.Resources(smokeNamespace).List(suiteCtx, &cms,
				resources.WithLabelSelector("beyla.grafana.com/node"))).To(Succeed())
			g.Expect(cms.Items).NotTo(BeEmpty(),
				"Beyla has not published a per-node injection ConfigMap yet")
		}, 5*time.Minute, 5*time.Second).Should(Succeed())

		By("waiting for all SDK app pods to be instrumented and Ready")
		for _, app := range sdkApps {
			waitInstrumentedReadyPod(sdkSmokeTestNS, "app="+app)
		}
	})

	Context("Node.js", func() {
		It("instruments an Express app and emits HTTP server spans and metrics", func() {
			assertTelemetryForService(sdkNodejsApp)
		})
	})

	Context("Python", func() {
		Context("glibc (python:3.11-slim)", func() {
			It("instruments a Flask app and emits HTTP server spans and metrics", func() {
				assertTelemetryForService(sdkPythonApp)
			})
		})

		Context("musl (python:3.11-alpine)", func() {
			It("instruments a Flask app and emits HTTP server spans and metrics", func() {
				assertTelemetryForService(sdkPythonMuslApp)
			})
		})
	})

	Context("Java", func() {
		It("instruments a JDK HTTP server and emits HTTP server spans and metrics", func() {
			assertTelemetryForService(sdkJavaApp)
		})
	})

	Context(".NET", func() {
		Context("glibc (mcr.microsoft.com/dotnet/aspnet:9.0)", func() {
			It("instruments an ASP.NET Core app and emits HTTP server spans and metrics", func() {
				assertTelemetryForService(sdkDotnetApp)
			})
		})

		Context("musl (mcr.microsoft.com/dotnet/aspnet:9.0-alpine)", func() {
			It("instruments an ASP.NET Core app and emits HTTP server spans and metrics", func() {
				assertTelemetryForService(sdkDotnetMuslApp)
			})
		})
	})
})

// assertTelemetryForService polls Tempo and Prometheus until both carry data
// for serviceName.
func assertTelemetryForService(serviceName string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		matched, n, err := tempoHasTraces(
			fmt.Sprintf(`{ resource.service.name = "%s" }`, serviceName),
			fmt.Sprintf(`{ resource.service.name =~ "%s.*" }`, serviceName),
		)
		g.Expect(err).NotTo(HaveOccurred(), "Tempo query failed")
		g.Expect(n).To(BeNumerically(">", 0),
			"no spans in Tempo for service %q (last query: %s)", serviceName, matched)

		series, err := promSeriesCountForService(serviceName)
		g.Expect(err).NotTo(HaveOccurred(), "Prometheus query failed")
		g.Expect(series).To(BeNumerically(">", 0),
			"no metrics in Prometheus for service %q", serviceName)
	}).Should(Succeed())
}

// tempoSearchResult is a subset of Tempo's /api/search response.
type tempoSearchResult struct {
	Traces []struct {
		TraceID string `json:"traceID"`
	} `json:"traces"`
}

// tempoHasTraces tries each TraceQL query in order and returns the first to
// produce results, along with the match count. Multiple queries keep the check
// resilient to exact resource.service.name stamping.
func tempoHasTraces(queries ...string) (string, int, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	last := ""
	for _, q := range queries {
		last = q
		u := tempoBaseURL + "/api/search?q=" + url.QueryEscape(q) + "&limit=20"
		resp, err := client.Get(u) //nolint:gosec // test-only local endpoint
		if err != nil {
			return q, 0, err
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return q, 0, err
		}
		if resp.StatusCode != http.StatusOK {
			return q, 0, fmt.Errorf("tempo query %q returned HTTP %d: %s", q, resp.StatusCode, body)
		}
		var sr tempoSearchResult
		if err := json.Unmarshal(body, &sr); err != nil {
			return q, 0, fmt.Errorf("decoding tempo response: %w", err)
		}
		if len(sr.Traces) > 0 {
			return q, len(sr.Traces), nil
		}
	}
	return last, 0, nil
}

// promSeriesResponse is a subset of Prometheus's /api/v1/series response.
type promSeriesResponse struct {
	Status string              `json:"status"`
	Data   []map[string]string `json:"data"`
}

// promSeriesCountForService returns the number of distinct Prometheus series
// carrying service_name=<serviceName>.
func promSeriesCountForService(serviceName string) (int, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	matcher := fmt.Sprintf(`{service_name="%s"}`, serviceName)
	u := promBaseURL + "/api/v1/series?match[]=" + url.QueryEscape(matcher)
	resp, err := client.Get(u) //nolint:gosec // test-only local endpoint
	if err != nil {
		return 0, err
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("prometheus series query returned HTTP %d: %s", resp.StatusCode, body)
	}
	var sr promSeriesResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return 0, fmt.Errorf("decoding prometheus response: %w", err)
	}
	return len(sr.Data), nil
}

// dumpPodLogs writes the tail of logs for matching pods to the Ginkgo writer.
func dumpPodLogs(what, ns, selector string) {
	By(fmt.Sprintf("dumping %s pod logs", what))
	var pods corev1.PodList
	if err := k8sClient.Resources(ns).List(suiteCtx, &pods,
		resources.WithLabelSelector(selector)); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "failed to list %s pods: %v\n", what, err)
		return
	}
	for i := range pods.Items {
		name := pods.Items[i].Name
		stream, err := clientset.CoreV1().Pods(ns).
			GetLogs(name, &corev1.PodLogOptions{TailLines: ptr.To(int64(100))}).Stream(suiteCtx)
		if err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "logs %s/%s: %v\n", ns, name, err)
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
	if err := k8sClient.Resources(smokeNamespace).List(suiteCtx, &cms,
		resources.WithLabelSelector("beyla.grafana.com/node")); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "failed to list injection ConfigMaps: %v\n", err)
		return
	}
	for i := range cms.Items {
		cm := cms.Items[i]
		_, _ = fmt.Fprintf(GinkgoWriter, "=== ConfigMap %s/%s ===\n%v\n",
			cm.Namespace, cm.Name, cm.Data)
	}
}
