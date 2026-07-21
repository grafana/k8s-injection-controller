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

// Package e2e is the end-to-end test suite for the k8s-injection-controller.
// See the per-suite files for what each covers. The suite needs only a
// reachable Docker daemon: it builds images with the Docker Go SDK, drives the
// cluster through e2e-framework Go clients, and renders the controller overlay
// with the kustomize Go API.
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/moby/go-archive"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/support/kind"

	"github.com/grafana/beyla-k8s-injector/test/utils"
)

// allowedConfigMapWriter is on the controller's ALLOWED_CONFIGMAP_WRITERS list.
// Specs impersonate it to write injection ConfigMaps the way Beyla does.
const allowedConfigMapWriter = "system:serviceaccount:beyla-k8s-injector:beyla"

// Webhook cert strategies the suite can run under, selected via the CERT_MODE
// env var. `make test-e2e` invokes the suite once per mode (each in its own
// Kind cluster) so both injection and instrumentation specs are exercised
// under both strategies.
const (
	certModeCertManager = "cert-manager"
	certModeSelfSigned  = "self-signed"
)

const managerImage = "beyla-k8s-injector:dev"

var (
	// certMode is the cert strategy under test (CERT_MODE env, default cert-manager).
	certMode string
	// servingCertSecret is the webhook serving-cert Secret name for certMode.
	// cert-manager issues into `webhook-server-cert`; the self-signed overlay's
	// namePrefix produces a prefixed name. The cert assertion reads this
	// variable instead of hard-coding either name.
	servingCertSecret string

	// clusterName gets a per-mode suffix in BeforeSuite so the two CERT_MODE
	// runs use distinct clusters.
	clusterName = "k8s-injection-controller-e2e"

	projectDir   string
	manifestsDir string

	testCluster    *kind.Cluster
	k8sClient      klient.Client
	clientset      *kubernetes.Clientset
	beylaClientset *kubernetes.Clientset
	suiteCtx       = context.Background()
)

// TestE2E owns the Kind cluster lifecycle: build/load images, create the
// cluster, install CertManager, run specs, destroy the cluster (unless
// KIND_KEEP_CLUSTER=true). The controller runs in init_container injection
// mode, so no ImageVolume feature gate is required.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	var err error

	// Resolve the cert strategy. `make test-e2e` runs the suite twice
	// (CERT_MODE=cert-manager, then CERT_MODE=self-signed).
	certMode = os.Getenv("CERT_MODE")
	if certMode == "" {
		certMode = certModeCertManager
	}
	switch certMode {
	case certModeCertManager:
		servingCertSecret = "webhook-server-cert"
	case certModeSelfSigned:
		servingCertSecret = "beyla-k8s-injector-webhook-server-cert"
	default:
		Fail(fmt.Sprintf("invalid CERT_MODE %q: must be %q or %q",
			certMode, certModeCertManager, certModeSelfSigned))
	}
	clusterName = clusterName + "-" + certMode
	_, _ = fmt.Fprintf(GinkgoWriter, "Running e2e suite in %q cert mode (cluster %q)\n", certMode, clusterName)

	projectDir, err = utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred(), "Failed to resolve project directory")
	manifestsDir = filepath.Join(projectDir, "test", "e2e", "manifests")
	kindConfig := filepath.Join(projectDir, "test", "e2e", "kind-config.yaml")

	By("building the manager image with the Docker Go SDK")
	Expect(buildImage(suiteCtx, projectDir, managerImage, ".git", "bin", "dist")).
		To(Succeed(), "Failed to build the manager image")

	By("building the SDK, safety, and filter test app images")
	for _, app := range allAppDirs(projectDir) {
		Expect(buildImage(suiteCtx, app.dir, app.tag)).
			To(Succeed(), "Failed to build %s", app.tag)
	}

	By("creating the Kind cluster")
	testCluster = kind.NewCluster(clusterName)
	_, err = testCluster.CreateWithConfig(suiteCtx, kindConfig)
	Expect(err).NotTo(HaveOccurred(), "Failed to create the Kind cluster")

	By("loading the manager image into the Kind cluster")
	Expect(testCluster.LoadImage(suiteCtx, managerImage)).
		To(Succeed(), "Failed to load the manager image into Kind")

	By("loading the test app images into the Kind cluster")
	for _, app := range allAppDirs(projectDir) {
		Expect(testCluster.LoadImage(suiteCtx, app.tag)).
			To(Succeed(), "Failed to load %s into Kind", app.tag)
	}

	By("pre-pulling grafana/otel-lgtm into the Kind cluster")
	lgtmImage, err := lgtmImageRef(filepath.Join(manifestsDir, "instrumentation-lgtm.yaml"))
	Expect(err).NotTo(HaveOccurred(), "Failed to resolve lgtm image from manifest")
	pullCtx, pullCancel := context.WithTimeout(suiteCtx, 10*time.Minute)
	defer pullCancel()
	Expect(pullAndLoadImage(pullCtx, clusterName, lgtmImage)).
		To(Succeed(), "Failed to pull/load %s", lgtmImage)

	By("building the Kubernetes clients")
	kubeconfig := testCluster.GetKubeconfig()
	k8sClient, err = klient.NewWithKubeConfigFile(kubeconfig)
	Expect(err).NotTo(HaveOccurred(), "Failed to build the klient client")
	clientset, err = kubernetes.NewForConfig(k8sClient.RESTConfig())
	Expect(err).NotTo(HaveOccurred(), "Failed to build the client-go clientset")

	beylaCfg := rest.CopyConfig(k8sClient.RESTConfig())
	beylaCfg.Impersonate = rest.ImpersonationConfig{UserName: allowedConfigMapWriter}
	beylaClientset, err = kubernetes.NewForConfig(beylaCfg)
	Expect(err).NotTo(HaveOccurred(), "Failed to build the impersonating clientset")

	if certMode == certModeCertManager {
		By("installing CertManager")
		Expect(utils.InstallCertManager(suiteCtx, k8sClient.Resources())).
			To(Succeed(), "Failed to install CertManager")
	} else {
		By("skipping CertManager (self-signed mode runs without it)")
	}

	By("ensuring the controller namespace exists")
	// krusty doesn't reorder, so the rendered overlay lists RBAC before the
	// Namespace. Pre-create to avoid the race.
	Expect(ensureNamespace(ctrlNamespace)).To(Succeed())

	overlay := "test"
	if certMode == certModeSelfSigned {
		overlay = "test-selfsigned"
	}
	By(fmt.Sprintf("deploying the controller-manager (config/%s overlay, rendered with the kustomize Go API)", overlay))
	// forceManagerImage rewrites the manager image in the rendered Deployment
	// before create, so the first pod starts with the locally-built+loaded
	// image — no ErrImagePull window, no post-apply rollout.
	Expect(applyKustomization(filepath.Join(projectDir, "config", overlay))).To(Succeed())

	By("waiting for the controller-manager rollout to finish")
	waitDeploymentReady(ctrlDeployment, ctrlNamespace, 3*time.Minute)

	By("waiting for the webhook to be reachable")
	waitWebhookReachable()
})

var _ = AfterSuite(func() {
	if testCluster == nil {
		return
	}
	By("exporting kind cluster logs")
	utils.ExportClusterLogs(context.Background(), testCluster, "e2e")
	if os.Getenv("KIND_KEEP_CLUSTER") == "true" {
		_, _ = fmt.Fprintf(GinkgoWriter, "Keeping Kind cluster (KIND_KEEP_CLUSTER=true)\n")
		return
	}
	By("destroying the Kind cluster")
	Expect(testCluster.Destroy(context.Background())).To(Succeed(), "Failed to destroy the Kind cluster")
})

// buildImage builds a Docker image from dir using the Docker Engine Go SDK
// (no `docker` CLI). excludes is an optional list of directories to omit from
// the build context — used for the manager build to skip large, irrelevant
// dirs (.git, bin, dist). The legacy builder doesn't honor BuildKit-style
// .dockerignore re-includes, so we exclude rather than allowlist.
func buildImage(ctx context.Context, dir, tag string, excludes ...string) error {
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("creating docker client: %w", err)
	}
	defer func() { _ = cli.Close() }()

	buildContext, err := archive.TarWithOptions(dir, &archive.TarOptions{ExcludePatterns: excludes})
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

// lgtmImageRef extracts the grafana/otel-lgtm image ref from the lgtm manifest
// so the suite can pre-pull it without duplicating the pinned tag here.
func lgtmImageRef(manifestPath string) (string, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if s := strings.TrimSpace(line); strings.HasPrefix(s, "image: grafana/otel-lgtm:") {
			return strings.TrimPrefix(s, "image: "), nil
		}
	}
	return "", fmt.Errorf("grafana/otel-lgtm image not found in %s", manifestPath)
}

// pullAndLoadImage pulls a registry image into the Kind node's containerd via
// ctr images pull, bypassing docker save → kind load. On Docker Desktop for Mac,
// docker save always emits the multi-arch manifest-list index regardless of
// local tagging; ctr import --all-platforms then fails on missing non-native
// platform blobs. Pulling via ctr fetches only the requested platform.
func pullAndLoadImage(ctx context.Context, clusterName, ref string) error {
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("creating docker client: %w", err)
	}
	defer func() { _ = cli.Close() }()

	info, err := cli.Info(ctx)
	if err != nil {
		return fmt.Errorf("getting docker info: %w", err)
	}
	platform := "linux/amd64"
	if info.Architecture == "aarch64" || info.Architecture == "arm64" {
		platform = "linux/arm64"
	}

	// ctr doesn't expand short names; prepend docker.io if no registry host.
	ctrRef := ref
	if !strings.Contains(strings.SplitN(ref, "/", 2)[0], ".") {
		ctrRef = "docker.io/" + ref
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "Pulling %s (%s) into Kind via ctr\n", ctrRef, platform)

	kindNode := clusterName + "-control-plane"
	execCreate, err := cli.ContainerExecCreate(ctx, kindNode, container.ExecOptions{
		AttachStdout: true,
		AttachStderr: true,
		Cmd: []string{"ctr", "--namespace=k8s.io", "images", "pull", "--platform", platform, ctrRef},
	})
	if err != nil {
		return fmt.Errorf("creating ctr pull exec in %s: %w", kindNode, err)
	}

	conn, err := cli.ContainerExecAttach(ctx, execCreate.ID, container.ExecStartOptions{})
	if err != nil {
		return fmt.Errorf("attaching ctr pull exec: %w", err)
	}
	defer conn.Close()

	var out bytes.Buffer
	_, _ = stdcopy.StdCopy(&out, &out, conn.Reader)

	result, err := cli.ContainerExecInspect(ctx, execCreate.ID)
	if err != nil {
		return fmt.Errorf("inspecting ctr pull result: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("ctr images pull failed (exit %d):\n%s", result.ExitCode, out.String())
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "ctr pull: %s\n", out.String())
	return nil
}

// allAppDirs returns every test app's source directory and image tag across
// all per-suite app sets, in build order.
func allAppDirs(projectDir string) []struct{ dir, tag string } {
	return append(append(sdkAppDirs(projectDir), safetyAppDirs(projectDir)...), filterAppDirs(projectDir)...)
}
