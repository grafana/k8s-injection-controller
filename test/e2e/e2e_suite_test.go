//go:build e2e
// +build e2e

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
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/e2e-framework/support/kind"

	"github.com/grafana/beyla-k8s-injector/test/utils"
)

var (
	// managerImage is the manager image to be built and loaded for testing.
	managerImage = "example.com/k8s-injection-controller:v0.0.1"
	// shouldCleanupCertManager tracks whether CertManager was installed by this suite.
	shouldCleanupCertManager = false
	// kindCluster manages the Kind cluster lifecycle for this suite.
	kindCluster *kind.Cluster
)

// envTrue is the value the suite's boolean env-var toggles are compared against.
const envTrue = "true"

// kindClusterName returns the e2e cluster name (KIND_CLUSTER env, else the
// default the Makefile uses), so `go test -tags e2e ./test/e2e/...` and
// `make test-e2e` target the same cluster.
func kindClusterName() string {
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok && v != "" {
		return v
	}
	return "k8s-injection-controller-test-e2e"
}

// TestE2E runs the e2e test suite to validate the solution in an isolated environment.
// The default setup requires Kind and CertManager.
//
// To enable kubectl kuberc (use custom kubectl configurations), set: KUBECTL_KUBERC=true
// By default, kuberc is disabled to ensure consistent test behavior across different environments.
// To skip CertManager installation, set: CERT_MANAGER_INSTALL_SKIP=true
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting k8s-injection-controller e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	ctx := context.Background()

	By("ensuring the e2e Kind cluster exists")
	// sigs.k8s.io/e2e-framework's kind support manages the cluster lifecycle.
	// CreateWithConfig is idempotent — it reuses an existing cluster or creates
	// one from kind-config.yaml (which enables the ImageVolume feature gate and
	// maps LGTM's Prometheus NodePort). Pointing KUBECONFIG at the kubeconfig it
	// captures makes every kubectl/make shell-out target this cluster and
	// sidesteps any unset current-context in the ambient kubeconfig.
	kindCluster = kind.NewCluster(kindClusterName())
	projectDir, err := utils.GetProjectDir()
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to resolve the project directory")
	_, err = kindCluster.CreateWithConfig(ctx, filepath.Join(projectDir, "test", "e2e", "kind-config.yaml"))
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to create the Kind cluster")
	ExpectWithOffset(1, os.Setenv("KUBECONFIG", kindCluster.GetKubeconfig())).
		To(Succeed(), "Failed to point KUBECONFIG at the Kind cluster")

	By("building the manager image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager image")

	By("loading the manager image into the Kind cluster")
	ExpectWithOffset(1, kindCluster.LoadImage(ctx, managerImage)).
		To(Succeed(), "Failed to load the manager image into Kind")

	configureKubectlKubeRC()
	setupCertManager()
})

var _ = AfterSuite(func() {
	teardownCertManager()
	teardownKindCluster()
})

// teardownKindCluster destroys the suite's Kind cluster, unless
// E2E_KEEP_CLUSTER=true (handy for post-mortem debugging of a failed run).
func teardownKindCluster() {
	if kindCluster == nil {
		return
	}
	if os.Getenv("E2E_KEEP_CLUSTER") == envTrue {
		_, _ = fmt.Fprintf(GinkgoWriter, "Keeping Kind cluster %q (E2E_KEEP_CLUSTER=true)\n", kindClusterName())
		return
	}
	By("destroying the Kind cluster")
	if err := kindCluster.Destroy(context.Background()); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "warning: failed to destroy Kind cluster: %v\n", err)
	}
}

// Disable kubectl kuberc by default for test isolation.
// This prevents local kubectl configurations from affecting test behavior.
// To enable kuberc, set: KUBECTL_KUBERC=true
func configureKubectlKubeRC() {
	if os.Getenv("KUBECTL_KUBERC") != envTrue {
		By("disabling kubectl kuberc for test isolation")
		err := os.Setenv("KUBECTL_KUBERC", "false")
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to disable kubectl kuberc")
		_, _ = fmt.Fprintf(GinkgoWriter,
			"kubectl kuberc disabled for consistent test behavior (override with KUBECTL_KUBERC=true)\n")
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter, "kubectl kuberc enabled (KUBECTL_KUBERC=true)\n")
	}
}

// setupCertManager installs CertManager if needed for webhook tests.
// Skips installation if CERT_MANAGER_INSTALL_SKIP=true or if already present.
func setupCertManager() {
	if os.Getenv("CERT_MANAGER_INSTALL_SKIP") == envTrue {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping CertManager installation (CERT_MANAGER_INSTALL_SKIP=true)\n")
		return
	}

	By("checking if CertManager is already installed")
	if utils.IsCertManagerCRDsInstalled() {
		_, _ = fmt.Fprintf(GinkgoWriter, "CertManager is already installed. Skipping installation.\n")
		return
	}

	// Mark for cleanup before installation to handle interruptions and partial installs.
	shouldCleanupCertManager = true

	By("installing CertManager")
	Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install CertManager")
}

// teardownCertManager uninstalls CertManager if it was installed by setupCertManager.
// This ensures we only remove what we installed.
func teardownCertManager() {
	if !shouldCleanupCertManager {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping CertManager cleanup (not installed by this suite)\n")
		return
	}

	By("uninstalling CertManager")
	utils.UninstallCertManager()
}
