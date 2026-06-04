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

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/support/kind"

	"github.com/grafana/beyla-k8s-injector/test/utils"
)

// allowedConfigMapWriter is the identity on the controller's
// ALLOWED_CONFIGMAP_WRITERS list (Beyla's ServiceAccount, see
// config/manager/manager.yaml). The suite impersonates it to write injection
// ConfigMaps, exactly as Beyla does in production; the kind-admin identity is
// intentionally rejected by the ConfigMap validating webhook.
const allowedConfigMapWriter = "system:serviceaccount:beyla-k8s-injector:beyla"

var (
	// managerImage is the manager image to be built and loaded for testing.
	managerImage = "beyla-k8s-injector:dev"

	// clusterName is the Kind cluster the suite creates and destroys. The
	clusterName = "k8s-injection-controller-test-e2e"

	// testCluster owns the Kind cluster lifecycle for the whole suite.
	testCluster *kind.Cluster
	// k8sClient is the typed client the specs use instead of shelling out to kubectl.
	k8sClient klient.Client
	// clientset backs the subresource calls klient does not expose directly
	// (ServiceAccount token requests and pod log streaming).
	clientset *kubernetes.Clientset
	// beylaClientset impersonates allowedConfigMapWriter so the suite can write
	// the annotated injection ConfigMaps past the validating webhook, the way
	// Beyla's ServiceAccount does in production.
	beylaClientset *kubernetes.Clientset
	// suiteCtx scopes the cluster/client operations to the suite run.
	suiteCtx = context.Background()
)

// TestE2E runs the e2e test suite to validate the solution in an isolated environment.
// The suite owns its Kind cluster: it builds and loads the manager image, creates the
// cluster (with the ImageVolume feature gate from test/e2e/kind-config.yaml), installs
// CertManager, and tears the cluster down afterwards. Kind and Docker must be on PATH.
//
// To keep the Kind cluster after the run (e.g. for debugging), set: KIND_KEEP_CLUSTER=true
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting k8s-injection-controller e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	// Resolve the kind config path up front: the first utils.Run chdirs to the
	// project root, so compute the absolute path while it is still unambiguous.
	projectDir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred(), "Failed to resolve project directory")
	kindConfig := filepath.Join(projectDir, "test", "e2e", "kind-config.yaml")

	By("building the manager image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager image")

	By("creating the Kind cluster")
	testCluster = kind.NewCluster(clusterName)
	_, err = testCluster.CreateWithConfig(suiteCtx, kindConfig)
	Expect(err).NotTo(HaveOccurred(), "Failed to create the Kind cluster")

	By("loading the manager image into the Kind cluster")
	Expect(testCluster.LoadImage(suiteCtx, managerImage)).
		To(Succeed(), "Failed to load the manager image into Kind")

	By("pointing kubectl/kustomize tooling at the Kind cluster")
	// support/kind writes an isolated kubeconfig; export it so the surviving
	// `make install`/`make deploy` shell-outs (kustomize + kubectl) target it.
	kubeconfig := testCluster.GetKubeconfig()
	Expect(os.Setenv("KUBECONFIG", kubeconfig)).To(Succeed(), "Failed to export KUBECONFIG")

	By("building the Kubernetes clients")
	k8sClient, err = klient.NewWithKubeConfigFile(kubeconfig)
	Expect(err).NotTo(HaveOccurred(), "Failed to build the klient client")
	clientset, err = kubernetes.NewForConfig(k8sClient.RESTConfig())
	Expect(err).NotTo(HaveOccurred(), "Failed to build the client-go clientset")

	beylaCfg := rest.CopyConfig(k8sClient.RESTConfig())
	beylaCfg.Impersonate = rest.ImpersonationConfig{UserName: allowedConfigMapWriter}
	beylaClientset, err = kubernetes.NewForConfig(beylaCfg)
	Expect(err).NotTo(HaveOccurred(), "Failed to build the impersonating clientset")

	By("installing CertManager")
	Expect(utils.InstallCertManager(suiteCtx, k8sClient.Resources())).
		To(Succeed(), "Failed to install CertManager")
})

var _ = AfterSuite(func() {
	if testCluster == nil {
		return
	}
	if os.Getenv("KIND_KEEP_CLUSTER") == "true" {
		_, _ = fmt.Fprintf(GinkgoWriter, "Keeping Kind cluster (KIND_KEEP_CLUSTER=true)\n")
		return
	}
	By("destroying the Kind cluster")
	Expect(testCluster.Destroy(context.Background())).To(Succeed(), "Failed to destroy the Kind cluster")
})
