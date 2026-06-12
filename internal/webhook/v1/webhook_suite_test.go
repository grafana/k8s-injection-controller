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

package v1

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"go.opentelemetry.io/obi/pkg/appolly/services"

	"github.com/grafana/beyla/v3/pkg/webhook/configmap"

	"github.com/grafana/beyla-k8s-injector/internal/config"
	"github.com/grafana/beyla-k8s-injector/internal/registry"
	// +kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var (
	ctx       context.Context
	cancel    context.CancelFunc
	k8sClient client.Client
	cfg       *rest.Config
	testEnv   *envtest.Environment

	// webhookRegistry backs the API-server-registered mutating webhook. Specs
	// mutate it (via Set) to simulate Beyla's ConfigMap changing under a running
	// workload. webhookSDKConfig is the controller-wide injection config the
	// registered PodMutator applies; specs reuse it to recompute the expected
	// PodConfigHash. init_container mode is used so the mutated pod spec is valid
	// on any envtest apiserver (no ImageVolume feature gate needed).
	webhookRegistry  *registry.Registry
	webhookSDKConfig = config.SDKInject{
		ImageVolumeRoot: "ghcr.io/grafana/beyla/inject-sdk-image",
		ImageVersion:    "0.0.13",
		InjectionMode:   config.InjectionModeInitContainer,
	}
)

func TestAPIs(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Webhook Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	var err error
	err = corev1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// +kubebuilder:scaffold:scheme

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: false,

		WebhookInstallOptions: envtest.WebhookInstallOptions{
			// Point at the manifest file directly. Pointing at the whole
			// config/webhook/ directory makes envtest try to install
			// namespace_selector_patch.yaml — a kustomize strategic-merge
			// patch fragment that fails apiserver validation on its own.
			Paths: []string{filepath.Join("..", "..", "..", "config", "webhook", "manifests.yaml")},
		},
	}

	// Retrieve the first found binary directory to allow running tests from IDEs
	if getFirstFoundEnvTestBinaryDir() != "" {
		testEnv.BinaryAssetsDirectory = getFirstFoundEnvTestBinaryDir()
	}

	// cfg is defined in this file globally.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	// start webhook server using Manager.
	webhookInstallOptions := &testEnv.WebhookInstallOptions
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    webhookInstallOptions.LocalServingHost,
			Port:    webhookInstallOptions.LocalServingPort,
			CertDir: webhookInstallOptions.LocalServingCertDir,
		}),
		LeaderElection: false,
		Metrics:        metricsserver.Options{BindAddress: "0"},
	})
	Expect(err).NotTo(HaveOccurred())

	webhookRegistry = registry.New()
	err = SetupPodWebhookWithManager(mgr, webhookRegistry, mgr.GetAPIReader(), &PodMutator{Cfg: webhookSDKConfig}, nil)
	Expect(err).NotTo(HaveOccurred())

	// +kubebuilder:scaffold:webhook

	go func() {
		defer GinkgoRecover()
		err = mgr.Start(ctx)
		Expect(err).NotTo(HaveOccurred())
	}()

	// wait for the webhook server to get ready.
	dialer := &net.Dialer{Timeout: time.Second}
	addrPort := fmt.Sprintf("%s:%d", webhookInstallOptions.LocalServingHost, webhookInstallOptions.LocalServingPort)
	Eventually(func() error {
		conn, err := tls.DialWithDialer(dialer, "tcp", addrPort, &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			return err
		}

		return conn.Close()
	}).Should(Succeed())
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	Eventually(func() error {
		return testEnv.Stop()
	}, time.Minute, time.Second).Should(Succeed())
})

// nsInstrumentation builds a registry.Instrumentation whose single rule matches
// every pod in namespace ns and carries env as the per-rule SDK config — the
// shape Beyla writes into its ConfigMaps. Changing env changes the rule's hash,
// and therefore the PodConfigHash the webhook stamps.
func nsInstrumentation(ns string, env ...corev1.EnvVar) registry.Instrumentation {
	return registry.Instrumentation{InjectConfig: configmap.InjectConfig{
		Rules: []configmap.Rule{{
			Selector: configmap.K8sSelector{
				Namespaces: []services.GlobAttr{services.NewGlob(ns)},
			},
			Config: configmap.RuleConfig{Env: env},
		}},
	}}
}

var _ = Describe("Pod webhook config-change restart detection", func() {
	// Drives the real admission webhook through the envtest API server (unlike
	// the unit specs, which call PodCustomDefaulter.Default directly): create a
	// pod, change the ConfigMap, and assert the previously stamped annotation no
	// longer matches the current config hash — the signal the controller uses to
	// roll the workload.

	mkPod := func(ns, name string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
			},
		}
	}

	It("re-stamps the inject hash so a stale pod is detected for restart when the ConfigMap changes", func() {
		const (
			ns    = "webhook-config-change"
			cmKey = "config-change-cm"
		)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())

		// Step 1: the ConfigMap selects this namespace with config A. A created
		// pod is admitted, instrumented, and annotated with the hash of the SDK
		// config combined with rule config A.
		configA := []corev1.EnvVar{{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: "http://collector-a:4318"}}
		webhookRegistry.Set(cmKey, nsInstrumentation(ns, configA...))

		p1 := mkPod(ns, "demo")
		Expect(k8sClient.Create(ctx, p1)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(p1), p1)).To(Succeed())

		hashA := p1.Annotations[InjectedAnnotation]
		Expect(hashA).NotTo(BeEmpty())
		Expect(hashA).To(Equal(config.PodConfigHash(&webhookSDKConfig, &configmap.RuleConfig{Env: configA})))
		// Under config A the pod is current: no restart needed.
		Expect(IsInstrumentedWithWantedConfig(&p1.Spec, &p1.ObjectMeta, hashA)).To(BeTrue())

		// Step 2: the ConfigMap changes — config B points at a different OTLP
		// endpoint, so the rule (and thus the wanted hash) changes.
		configB := []corev1.EnvVar{{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: "http://collector-b:4318"}}
		webhookRegistry.Set(cmKey, nsInstrumentation(ns, configB...))
		wantB := config.PodConfigHash(&webhookSDKConfig, &configmap.RuleConfig{Env: configB})

		// Step 3: the already-running pod's annotation no longer matches the
		// current config hash. IsInstrumentedWithWantedConfig now returns false,
		// which is exactly the signal the controller uses to roll the workload so
		// the pod is re-admitted under the new config.
		Expect(wantB).NotTo(Equal(hashA))
		Expect(IsInstrumentedWithWantedConfig(&p1.Spec, &p1.ObjectMeta, wantB)).To(BeFalse())

		// The re-admission that the restart triggers stamps the new hash: a pod
		// freshly created under config B carries wantB.
		p2 := mkPod(ns, "demo-restarted")
		Expect(k8sClient.Create(ctx, p2)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(p2), p2)).To(Succeed())
		Expect(p2.Annotations).To(HaveKeyWithValue(InjectedAnnotation, wantB))
	})
})

// getFirstFoundEnvTestBinaryDir locates the first binary in the specified path.
// ENVTEST-based tests depend on specific binaries, usually located in paths set by
// controller-runtime. When running tests directly (e.g., via an IDE) without using
// Makefile targets, the 'BinaryAssetsDirectory' must be explicitly configured.
//
// This function streamlines the process by finding the required binaries, similar to
// setting the 'KUBEBUILDER_ASSETS' environment variable. To ensure the binaries are
// properly set up, run 'make setup-envtest' beforehand.
func getFirstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		logf.Log.Error(err, "Failed to read directory", "path", basePath)
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
