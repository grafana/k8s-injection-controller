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
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
)

const (
	filterTestNS          = "filter-test"
	filterJavaAgentedApp  = "filter-java-agented"
	filterDotnetHookedApp = "filter-dotnet-hooked"
	filterPythonOldApp    = "filter-python-old"
	filterNodejsApp       = "filter-nodejs"

	filterJavaAgentedImage  = "filter-java-agented-app:dev"
	filterDotnetHookedImage = "filter-dotnet-hooked-app:dev"
)

// filterAppDirs returns only the two new images this suite needs;
// filter-python-old reuses safety-python-old-app:dev and filter-nodejs reuses
// sdk-nodejs-app:dev.
func filterAppDirs(projectDir string) []struct{ dir, tag string } {
	base := filepath.Join(projectDir, "test", "e2e", "apps")
	return []struct{ dir, tag string }{
		{filepath.Join(base, "filter-java-agented"), filterJavaAgentedImage},
		{filepath.Join(base, "filter-dotnet-hooked"), filterDotnetHookedImage},
	}
}

var _ = Describe("Beyla startup compatibility filter", Ordered, func() {
	// Beyla's initial process scan omits already-instrumented or unsupported
	// processes from eligible_for_restart. We deploy such workloads BEFORE
	// Beyla starts and assert they're absent (while a compatible Node.js
	// workload is present, proving the scan ran).

	BeforeAll(func() {
		By("waiting for previous filter-test namespace to finish terminating")
		waitNamespaceDeleted(filterTestNS)

		By("deploying incompatible and compatible workloads before Beyla starts")
		Expect(applyManifestFile(filepath.Join(manifestsDir, "beyla-startup-filter-apps.yaml"))).To(Succeed())

		By("waiting for all filter app Deployments to roll out")
		for _, app := range []string{filterJavaAgentedApp, filterDotnetHookedApp, filterPythonOldApp, filterNodejsApp} {
			waitDeploymentReady(app, filterTestNS, 5*time.Minute)
		}

		By("deploying the real Beyla DaemonSet wired to the controller")
		Expect(deployBeyla(filterTestNS)).To(Succeed())

		By("waiting for the Beyla DaemonSet to be ready")
		waitBeylaReady()
	})

	AfterAll(func() {
		tearDownBeyla()

		By("tearing down filter test apps")
		_ = k8sClient.Resources().Delete(suiteCtx,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: filterTestNS}})
	})

	SetDefaultEventuallyTimeout(5 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("Beyla excludes incompatible existing workloads from eligible_for_restart", func() {
		By("waiting for Beyla to scan the namespace and add the compatible workload")
		// Node.js appearing first confirms the scan ran, making the absences meaningful.
		Eventually(func(g Gomega) {
			g.Expect(fetchEligibleForRestart(g)).To(ContainSubstring(filterNodejsApp),
				"compatible workload not yet in eligible_for_restart; Beyla may not have finished scanning")
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("asserting incompatible workloads are absent from eligible_for_restart")
		eligible := fetchEligibleForRestart(Default)
		Expect(eligible).NotTo(ContainSubstring(filterJavaAgentedApp),
			"Java pod with existing -javaagent must not appear in eligible_for_restart")
		Expect(eligible).NotTo(ContainSubstring(filterDotnetHookedApp),
			".NET pod with DOTNET_STARTUP_HOOKS must not appear in eligible_for_restart")
		Expect(eligible).NotTo(ContainSubstring(filterPythonOldApp),
			"Python 3.8 pod must not appear in eligible_for_restart")
	})
})

// fetchEligibleForRestart reads eligible_for_restart.yaml from all of Beyla's
// per-node ConfigMaps and concatenates them.
func fetchEligibleForRestart(g Gomega) string {
	var cms corev1.ConfigMapList
	g.Expect(k8sClient.Resources(ctrlNamespace).List(suiteCtx, &cms,
		resources.WithLabelSelector("beyla.grafana.com/node"))).To(Succeed())
	g.Expect(cms.Items).NotTo(BeEmpty(), "Beyla has not published a per-node ConfigMap yet")
	var out string
	for _, cm := range cms.Items {
		out += cm.Data["eligible_for_restart.yaml"]
	}
	return out
}
