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

// Package e2emetrics holds the "full" end-to-end suite: it stands up a real
// telemetry pipeline (otel-lgtm + the controller + a real Beyla DaemonSet + a
// demo app) and asserts the demo app's HTTP metrics reach LGTM, queried with
// PromQL. It is intentionally separate from the test/e2e suite (which exercises
// the controller/webhook in isolation) so the two can evolve independently.
//
// The suite has no binary prerequisites beyond a reachable Docker daemon: it
// builds the manager image with the Docker Go SDK, drives Kubernetes through the
// e2e-framework Go clients (kind + klient), and renders the controller overlay
// with the kustomize Go API. No docker/kubectl/kustomize/make CLI is invoked.
package e2emetrics

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/docker/docker/api/types/build"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/moby/go-archive"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/support/kind"

	"github.com/grafana/beyla-k8s-injector/test/utils"
)

var (
	// managerImage is the manager image built and loaded into the Kind cluster.
	managerImage = "beyla-k8s-injector:dev-metrics"

	// clusterName is the Kind cluster this suite creates and destroys. It is
	// distinct from the test/e2e cluster so both suites can run independently.
	clusterName = "k8s-injection-controller-e2e-metrics"

	// projectDir is the module root, resolved once in BeforeSuite.
	projectDir string

	// testCluster owns the Kind cluster lifecycle for the whole suite.
	testCluster *kind.Cluster
	// k8sClient is the typed client the specs use to drive Kubernetes.
	k8sClient klient.Client
	// clientset backs the subresource calls klient does not expose directly
	// (pod log streaming for failure diagnostics).
	clientset *kubernetes.Clientset
	// suiteCtx scopes the cluster/client operations to the suite run.
	suiteCtx = context.Background()
)

// TestE2EMetrics runs the metrics e2e suite. The suite owns its Kind cluster: it
// builds and loads the manager image, creates the cluster (with the Prometheus
// NodePort mapping from test/e2e_metrics/kind-config.yaml), installs CertManager,
// and tears the cluster down afterwards. The controller is driven in
// init_container injection mode, so no ImageVolume feature gate or specific node
// version is required. A Docker daemon and the `kind` tooling the e2e-framework
// drives must be available.
//
// To keep the Kind cluster after the run (e.g. for debugging), set:
// KIND_KEEP_CLUSTER=true
func TestE2EMetrics(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting k8s-injection-controller metrics e2e test suite\n")
	RunSpecs(t, "metrics e2e suite")
}

var _ = BeforeSuite(func() {
	var err error
	projectDir, err = utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred(), "Failed to resolve project directory")
	kindConfig := filepath.Join(projectDir, "test", "e2e_metrics", "kind-config.yaml")

	By("building the manager image with the Docker Go SDK")
	Expect(buildManagerImage(suiteCtx, projectDir, managerImage)).
		To(Succeed(), "Failed to build the manager image")

	By("creating the Kind cluster")
	testCluster = kind.NewCluster(clusterName)
	_, err = testCluster.CreateWithConfig(suiteCtx, kindConfig)
	Expect(err).NotTo(HaveOccurred(), "Failed to create the Kind cluster")

	By("loading the manager image into the Kind cluster")
	Expect(testCluster.LoadImage(suiteCtx, managerImage)).
		To(Succeed(), "Failed to load the manager image into Kind")

	By("building the Kubernetes clients")
	kubeconfig := testCluster.GetKubeconfig()
	k8sClient, err = klient.NewWithKubeConfigFile(kubeconfig)
	Expect(err).NotTo(HaveOccurred(), "Failed to build the klient client")
	clientset, err = kubernetes.NewForConfig(k8sClient.RESTConfig())
	Expect(err).NotTo(HaveOccurred(), "Failed to build the client-go clientset")

	By("installing CertManager")
	Expect(utils.InstallCertManager(suiteCtx, k8sClient.Resources())).
		To(Succeed(), "Failed to install CertManager")
})

var _ = AfterSuite(func() {
	if testCluster == nil {
		return
	}
	By("exporting kind cluster logs")
	utils.ExportClusterLogs(context.Background(), testCluster, "e2e_metrics")
	if os.Getenv("KIND_KEEP_CLUSTER") == "true" {
		_, _ = fmt.Fprintf(GinkgoWriter, "Keeping Kind cluster (KIND_KEEP_CLUSTER=true)\n")
		return
	}
	By("destroying the Kind cluster")
	Expect(testCluster.Destroy(context.Background())).To(Succeed(), "Failed to destroy the Kind cluster")
})

// buildManagerImage builds the manager image from the project's Dockerfile using
// the Docker Engine Go SDK, so the suite needs no `docker` CLI.
//
// It deliberately does NOT reuse the project's .dockerignore: that file relies on
// `**` blanket-ignore + `!` re-include semantics that only BuildKit honors. The
// SDK's ImageBuild uses the legacy builder, whose context tar prunes any
// directory not named by a literal exclusion pattern (so `!**/*.go` would drop
// cmd/, internal/, ... and the build would fail to find the sources). Instead we
// exclude only the large, build-irrelevant directories and ship the rest; the
// Dockerfile's `COPY . .` + `go build` simply ignores the extra files (e.g.
// _test.go) and the final distroless stage copies only the compiled binary.
func buildManagerImage(ctx context.Context, dir, tag string) error {
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("creating docker client: %w", err)
	}
	defer func() { _ = cli.Close() }()

	buildContext, err := archive.TarWithOptions(dir, &archive.TarOptions{
		ExcludePatterns: []string{".git", "bin", "dist"},
	})
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

	// DisplayJSONMessagesStream drains the streamed build output (required for
	// the build to run to completion) and returns an error if the build failed.
	if err := jsonmessage.DisplayJSONMessagesStream(resp.Body, GinkgoWriter, 0, false, nil); err != nil {
		return fmt.Errorf("image build failed: %w", err)
	}
	return nil
}
