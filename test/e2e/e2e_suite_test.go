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

// certMode describes one webhook-certificate strategy the suite exercises. Each
// mode runs the full Manager behavior suite in its own throwaway Kind cluster
// (see e2e_test.go), so the two strategies never share cluster state. The
// self-signed mode deliberately runs in a cluster WITHOUT cert-manager, proving
// the in-process rotator needs no cert-manager.
type certMode struct {
	// name is the human label used in spec descriptions and the cluster name.
	name string
	// deployConfig is the kustomize overlay `make deploy` builds for this mode
	// (passed via DEPLOY_CONFIG).
	deployConfig string
	// installCertManager controls whether cert-manager is installed into the
	// mode's cluster before deploying the controller.
	installCertManager bool
	// clusterName is the Kind cluster created/destroyed for this mode.
	clusterName string
	// servingCertSecret is the name of the webhook serving-cert Secret. It
	// differs between modes: cert-manager creates an unprefixed `webhook-server-cert`
	// (the Certificate's literal secretName), while the self-signed overlay ships
	// a namePrefix-applied `beyla-k8s-injector-webhook-server-cert`.
	servingCertSecret string
}

// certModes is the matrix the e2e suite runs. Both run under `make test-e2e`.
var certModes = []certMode{
	{
		name:               "cert-manager",
		deployConfig:       "config/cert-manager",
		installCertManager: true,
		clusterName:        "k8s-injection-controller-test-e2e-certmanager",
		servingCertSecret:  "webhook-server-cert",
	},
	{
		name:               "self-signed",
		deployConfig:       "config/self-signed",
		installCertManager: false,
		clusterName:        "k8s-injection-controller-test-e2e-selfsigned",
		servingCertSecret:  "beyla-k8s-injector-webhook-server-cert",
	},
}

var (
	// managerImage is the manager image built once and loaded into each mode's cluster.
	managerImage = "beyla-k8s-injector:dev"

	// kindConfig is the absolute path to the Kind cluster config (resolved in BeforeSuite).
	kindConfig string

	// testCluster owns the Kind cluster lifecycle for the mode currently running.
	// Reassigned by startClusterForMode at the start of each mode's Ordered block.
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

// TestE2E runs the e2e test suite to validate the solution in an isolated
// environment. The suite builds the manager image once, then runs the full
// Manager behavior suite once per cert mode (cert-manager and self-signed), each
// in its own Kind cluster (with the ImageVolume feature gate from
// test/e2e/kind-config.yaml). Kind and Docker must be on PATH.
//
// To keep the Kind clusters after the run (e.g. for debugging), set:
// KIND_KEEP_CLUSTER=true
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
	kindConfig = filepath.Join(projectDir, "test", "e2e", "kind-config.yaml")

	// The image is mode-independent, so build it once and load it into each
	// mode's cluster as that cluster comes up.
	By("building the manager image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager image")
})

// startClusterForMode creates a fresh Kind cluster for the given cert mode,
// loads the manager image, points the tooling + clients at it, and installs
// cert-manager when the mode requires it. It (re)assigns the package-level
// testCluster/k8sClient/clientset/beylaClientset used by the specs. Invoked from
// each mode's BeforeAll; modes run sequentially so the globals never overlap.
func startClusterForMode(m certMode) {
	By(fmt.Sprintf("creating the Kind cluster for the %s mode", m.name))
	testCluster = kind.NewCluster(m.clusterName)
	_, err := testCluster.CreateWithConfig(suiteCtx, kindConfig)
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

	if m.installCertManager {
		By("installing CertManager")
		Expect(utils.InstallCertManager(suiteCtx, k8sClient.Resources())).
			To(Succeed(), "Failed to install CertManager")
	} else {
		By("skipping CertManager (self-signed mode runs on a cluster without it)")
	}
}

// teardownCluster exports logs and destroys the current mode's Kind cluster
// (unless KIND_KEEP_CLUSTER=true). Invoked from each mode's AfterAll.
func teardownCluster(modeName string) {
	if testCluster == nil {
		return
	}
	By("exporting kind cluster logs")
	utils.ExportClusterLogs(context.Background(), testCluster, "e2e-"+modeName)
	if os.Getenv("KIND_KEEP_CLUSTER") == "true" {
		_, _ = fmt.Fprintf(GinkgoWriter, "Keeping Kind cluster %q (KIND_KEEP_CLUSTER=true)\n", modeName)
		return
	}
	By("destroying the Kind cluster")
	Expect(testCluster.Destroy(context.Background())).To(Succeed(), "Failed to destroy the Kind cluster")
	testCluster = nil
}
