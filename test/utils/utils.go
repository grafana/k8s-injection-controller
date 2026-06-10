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

package utils

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/support/kind"
)

const (
	certmanagerVersion = "v1.20.2"
	certmanagerURLTmpl = "https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml"
)

// Run executes the provided command within this context. It is reserved for the
// few shell-outs that have no client-go equivalent (the `make` targets that wrap
// the image build and the kustomize-driven install/deploy).
func Run(cmd *exec.Cmd) (string, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "chdir dir: %q\n", err)
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%q failed with error %q: %w", command, string(output), err)
	}

	return string(output), nil
}

// InstallCertManager applies the upstream cert-manager release bundle through the
// klient resource client and waits until its webhook Deployment is Available, so
// the suite can rely on it to issue the controller's serving certificate.
//
// The bundle's CustomResourceDefinitions (and any other types absent from the
// client-go scheme) are decoded as unstructured objects and created via the REST
// mapper, so no extra scheme registration is required.
func InstallCertManager(ctx context.Context, r *resources.Resources) error {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	manifest, err := fetchManifest(ctx, url)
	if err != nil {
		return err
	}

	if err := decoder.DecodeEach(ctx, strings.NewReader(manifest),
		decoder.CreateIgnoreAlreadyExists(r)); err != nil {
		return fmt.Errorf("applying cert-manager manifest: %w", err)
	}

	// Wait for cert-manager-webhook to be ready, which can take time if cert-manager
	// was re-installed after uninstalling on a cluster.
	webhook := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "cert-manager-webhook", Namespace: "cert-manager"},
	}
	if err := wait.For(
		conditions.New(r).DeploymentConditionMatch(webhook, appsv1.DeploymentAvailable, corev1.ConditionTrue),
		wait.WithContext(ctx),
		wait.WithTimeout(5*time.Minute),
		wait.WithInterval(5*time.Second),
	); err != nil {
		return fmt.Errorf("waiting for cert-manager-webhook to become available: %w", err)
	}

	return nil
}

// fetchManifest downloads a manifest over HTTP and returns its contents. It
// retries on transient failures (e.g. a TLS handshake timeout reaching GitHub),
// which would otherwise flake the whole suite in BeforeSuite.
func fetchManifest(ctx context.Context, url string) (string, error) {
	const attempts = 5
	client := &http.Client{Timeout: 60 * time.Second}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			backoff := time.Duration(attempt-1) * 3 * time.Second
			_, _ = fmt.Fprintf(GinkgoWriter, "retrying fetch of %s (attempt %d/%d) after %s: %v\n",
				url, attempt, attempts, backoff, lastErr)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(backoff):
			}
		}
		body, err := fetchManifestOnce(ctx, client, url)
		if err == nil {
			return body, nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("fetching %s after %d attempts: %w", url, attempts, lastErr)
}

// fetchManifestOnce performs a single HTTP GET of url and returns its body.
func fetchManifestOnce(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching %s: unexpected status %s", url, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", url, err)
	}
	return string(body), nil
}

// ExportClusterLogs runs `kind export logs` for the given cluster and writes the
// result (every node's journal plus all pod container logs) into
// testoutput/<suite>/ under the project root, creating the directory tree if it
// does not exist. It is best-effort: any failure is logged to GinkgoWriter and
// swallowed, so collecting logs during teardown can never mask the real test
// outcome. Call it from AfterSuite before destroying the cluster.
func ExportClusterLogs(ctx context.Context, cluster *kind.Cluster, suite string) {
	if cluster == nil {
		return
	}
	projectDir, err := GetProjectDir()
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "skipping log export: %v\n", err)
		return
	}
	dest := filepath.Join(projectDir, "testoutput", suite)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "skipping log export: creating %s: %v\n", dest, err)
		return
	}
	if err := cluster.ExportLogs(ctx, dest); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "exporting cluster logs to %s: %v\n", dest, err)
		return
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "exported kind cluster logs to %s\n", dest)
}

// GetProjectDir returns the module root (the directory holding go.mod) by
// walking up from the current working directory. Walking to go.mod keeps this
// correct regardless of which test/<suite> directory the caller runs from
// (e.g. test/e2e), which a fixed path-strip would not.
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, fmt.Errorf("failed to get current working directory: %w", err)
	}
	for dir := wd; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached the filesystem root without finding go.mod; fall back to
			// the legacy behavior so callers still get a best-effort answer.
			return strings.ReplaceAll(wd, "/test/e2e", ""), nil
		}
		dir = parent
	}
}

// UncommentCode searches for target in the file and remove the comment prefix
// of the target content. The target content may span multiple lines.
func UncommentCode(filename, target, prefix string) error {
	// false positive
	// nolint:gosec
	content, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("failed to read file %q: %w", filename, err)
	}
	strContent := string(content)

	idx := strings.Index(strContent, target)
	if idx < 0 {
		return fmt.Errorf("unable to find the code %q to be uncommented", target)
	}

	out := new(bytes.Buffer)
	_, err = out.Write(content[:idx])
	if err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	scanner := bufio.NewScanner(bytes.NewBufferString(target))
	if !scanner.Scan() {
		return nil
	}
	for {
		if _, err = out.WriteString(strings.TrimPrefix(scanner.Text(), prefix)); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
		// Avoid writing a newline in case the previous line was the last in target.
		if !scanner.Scan() {
			break
		}
		if _, err = out.WriteString("\n"); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
	}

	if _, err = out.Write(content[idx+len(target):]); err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	// false positive
	// nolint:gosec
	if err = os.WriteFile(filename, out.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write file %q: %w", filename, err)
	}

	return nil
}
