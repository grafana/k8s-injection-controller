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

package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"strings"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/open-policy-agent/cert-controller/pkg/rotator"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/grafana/beyla-k8s-injector/internal/config"
	"github.com/grafana/beyla-k8s-injector/internal/controller"
	"github.com/grafana/beyla-k8s-injector/internal/metrics"
	"github.com/grafana/beyla-k8s-injector/internal/registry"
	webhookv1 "github.com/grafana/beyla-k8s-injector/internal/webhook/v1"
	"github.com/grafana/beyla-k8s-injector/internal/webhookcert"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(corev1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var enableCertRotation bool
	var webhookCertSecret string
	var mutatingWebhookName, validatingWebhookName string
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.BoolVar(&enableCertRotation, "enable-cert-rotation", false,
		"If set, the controller self-manages its webhook serving certificate "+
			"(generates a self-signed CA + cert, persists it to --webhook-cert-secret, "+
			"writes it to --webhook-cert-path, and injects the caBundle into the webhook "+
			"configurations). Use when cert-manager is not installed.")
	flag.StringVar(&webhookCertSecret, "webhook-cert-secret", "",
		"Name of the Secret (in the controller's namespace) used to persist the "+
			"self-managed webhook serving cert. Required when --enable-cert-rotation is set.")
	flag.StringVar(&mutatingWebhookName, "mutating-webhook-name", "",
		"Name of the MutatingWebhookConfiguration to inject the caBundle into "+
			"when --enable-cert-rotation is set.")
	flag.StringVar(&validatingWebhookName, "validating-webhook-name", "",
		"Name of the ValidatingWebhookConfiguration to inject the caBundle into "+
			"when --enable-cert-rotation is set.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	var configPath string
	flag.StringVar(&configPath, "config", "",
		"Path to a YAML file with the SDK injection configuration (config.SDKInject). "+
			"If empty, the webhook still selects pods but does not mutate them.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	// the ctrl logger may display errors in logs while the certificate generation
	// is still happening. This manifests in two separate exception forms:
	// - cert-rotation    secret is not well-formed, cannot update webhook configurations
	// - cert-rotation    could not refresh CA and server certs
	// These transient errors go away soon, but they pollute the logs, we wrap the
	// logger into one that eliminates those errors.
	ctrl.SetLogger(filterCertRotationConflicts(zap.New(zap.UseFlagOptions(&opts))))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	// The controller only trusts ConfigMaps in a single namespace — the one
	// where Beyla writes its per-node state ConfigMaps (its own namespace).
	// Restricting the watch here is the first half of the security model:
	// combined with the namespaced RBAC Role and the validating webhook, an
	// unprivileged ConfigMap created anywhere else cannot steer injection.
	watchNamespace := os.Getenv("WATCH_NAMESPACE")
	if watchNamespace == "" {
		// Fall back to the controller's own namespace (downward API), which is
		// where Beyla is expected to run co-located.
		watchNamespace = os.Getenv("POD_NAMESPACE")
	}
	if watchNamespace == "" {
		setupLog.Error(nil, "no watch namespace configured: set WATCH_NAMESPACE, "+
			"or POD_NAMESPACE via the downward API, to the namespace where Beyla writes its state ConfigMaps")
		os.Exit(1)
	}
	setupLog.Info("restricting ConfigMap watch to a single namespace", "namespace", watchNamespace)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsServerOptions,
		// Scope only the ConfigMap informer to watchNamespace. Pods,
		// ReplicaSets and workloads stay cluster-wide on purpose: the eviction
		// sweep lists pods in arbitrary target namespaces and the pod-state
		// collector scans the whole cluster. A blanket cache.DefaultNamespaces
		// would break both — hence ByObject, restricting ConfigMaps alone.
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.ConfigMap{}: {
					Namespaces: map[string]cache.Config{watchNamespace: {}},
				},
			},
		},
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "c3bd973a.grafana.com",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	reg := registry.New()

	// Injection metrics. The counters and the state collector register on
	// controller-runtime's global registry, so they are exposed on the same
	// --metrics-bind-address endpoint and scraped by the existing ServiceMonitor.
	injMetrics := metrics.NewSDKInjectionMetrics()
	injMetrics.MustRegister(ctrlmetrics.Registry)

	var podMutator *webhookv1.PodMutator
	var sdkConfig config.SDKInject
	if configPath != "" {
		raw, err := os.ReadFile(configPath)
		if err != nil {
			setupLog.Error(err, "Failed to read SDK config file", "path", configPath)
			os.Exit(1)
		}
		if err := yaml.Unmarshal(raw, &sdkConfig); err != nil {
			setupLog.Error(err, "Failed to parse SDK config file", "path", configPath)
			os.Exit(1)
		}
		setupLog.Info("loaded SDK injection config", "path", configPath)
	} else {
		sdkConfig = config.SDKInject{}
		setupLog.Info("no --config provided; webhook will wait for remotely provided injection configuration")
	}

	sdkConfig.SetDefaults()

	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "Failed to build clientset")
		os.Exit(1)
	}

	resolveInjectionMode(&sdkConfig, clientset)

	podMutator = &webhookv1.PodMutator{Cfg: sdkConfig}

	// State gauge: scans the manager pod cache at scrape time and reports the
	// cluster-wide injection state (beyla_injection_pods).
	ctrlmetrics.Registry.MustRegister(metrics.NewPodStateCollector(mgr.GetClient(), reg, sdkConfig))

	// When ENABLE_WEBHOOKS=false we never register the webhook handlers, so the
	// webhook server is never added to the manager and its listener never starts
	// — webhookServer.StartedChecker() would then never go green. Every gate that
	// waits on it must be bypassed in that case, or it blocks forever: the
	// eviction sweep's WebhookReady gate and Service dial below would requeue
	// indefinitely, and the "webhook" readyz check further down would keep the
	// pod permanently NotReady.
	webhooksEnabled := os.Getenv("ENABLE_WEBHOOKS") != "false" // nolint:goconst
	var webhookReady healthz.Checker
	var webhookServiceAddr string
	if webhooksEnabled {
		webhookReady = webhookServer.StartedChecker()
		webhookServiceAddr = os.Getenv("WEBHOOK_SERVICE_ADDR")
	}

	if err := (&controller.ConfigMapReconciler{
		Client:             mgr.GetClient(),
		Clientset:          clientset,
		Registry:           reg,
		WebhookReady:       webhookReady,
		WebhookServiceAddr: webhookServiceAddr,
		DefaultSDKConfig:   sdkConfig,
		Metrics:            injMetrics,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to set up controller", "controller", "ConfigMap")
		os.Exit(1)
	}

	// setupWebhooks registers the mutating + validating webhook handlers. It
	// calls mgr.GetWebhookServer() (via NewWebhookManagedBy / Register), which
	// lazily adds the webhook server to the manager's runnables. In self-signed
	// mode we defer this until the cert rotator has written the cert,
	// otherwise the webhook server's certwatcher fails to start (no cert yet).
	//
	// In self-signed mode the closure runs from the goroutine below AFTER
	// mgr.Start has already been called, so mgr.GetWebhookServer() then triggers
	// mgr.Add(webhookServer) on the already-running manager. controller-runtime
	// v0.24 supports adding runnables after Start — it starts the runnable
	// immediately rather than rejecting it — so this late registration is safe.
	setupWebhooks := func() error {
		if err := webhookv1.SetupPodWebhookWithManager(mgr, reg, mgr.GetAPIReader(), podMutator, injMetrics); err != nil {
			return fmt.Errorf("setting up Pod webhook: %w", err)
		}

		// Validating webhook that gates writes to annotated ConfigMaps. The
		// allowlist is the set of usernames permitted to create/update them —
		// typically Beyla's ServiceAccount. system:masters is always allowed
		// (break-glass). With an empty allowlist only break-glass works, so
		// Beyla itself would be locked out: warn loudly.
		allowedWriters := parseAllowedWriters(os.Getenv("ALLOWED_CONFIGMAP_WRITERS"))
		if len(allowedWriters) == 0 {
			setupLog.Info("WARNING: ALLOWED_CONFIGMAP_WRITERS is empty; only system:masters may write " +
				"injection ConfigMaps. Set it to Beyla's ServiceAccount, e.g. " +
				"system:serviceaccount:<namespace>:<beyla-sa>")
		}
		mgr.GetWebhookServer().Register(webhookv1.ValidateConfigMapPath,
			&webhook.Admission{Handler: webhookv1.NewConfigMapValidator(scheme, allowedWriters)})
		setupLog.Info("registered ConfigMap validating webhook",
			"path", webhookv1.ValidateConfigMapPath, "allowedWriters", allowedWriters)
		return nil
	}

	// Shared signal-handler context: it drives both the cert-rotation goroutine
	// (so it can unblock if the manager shuts down before the cert is ready) and
	// mgr.Start below. ctrl.SetupSignalHandler must be called exactly once per
	// process — it panics on a second call — so it is established here.
	signalCtx := ctrl.SetupSignalHandler()

	if !webhooksEnabled {
		// Webhooks disabled: skip registration and cert rotation entirely. The
		// webhook server is never started, which is why the readiness gates above
		// and the "webhook" readyz check below are also skipped for this mode.
		setupLog.Info("ENABLE_WEBHOOKS=false: skipping webhook registration, cert rotation and webhook readiness gate")
	} else if enableCertRotation {
		// Self-managed cert path (no cert-manager). The rotator generates a
		// self-signed CA + serving cert, persists it to the pre-created Secret,
		// writes it to the webhook cert dir, and injects the caBundle into the
		// webhook configurations. Defer webhook registration until it is ready.
		if webhookCertSecret == "" || webhookCertPath == "" || mutatingWebhookName == "" || validatingWebhookName == "" {
			setupLog.Error(nil, "--enable-cert-rotation requires --webhook-cert-secret, --webhook-cert-path, "+
				"--mutating-webhook-name and --validating-webhook-name")
			os.Exit(1)
		}
		podNamespace := os.Getenv("POD_NAMESPACE")
		if podNamespace == "" {
			setupLog.Error(nil, "POD_NAMESPACE must be set (downward API) when --enable-cert-rotation is set")
			os.Exit(1)
		}
		dnsName, extraDNSNames := webhookcert.ServiceDNSNames(os.Getenv("WEBHOOK_SERVICE_ADDR"))
		if dnsName == "" {
			setupLog.Error(nil, "WEBHOOK_SERVICE_ADDR must be a non-empty host[:port] when --enable-cert-rotation is set")
			os.Exit(1)
		}
		setupFinished := make(chan struct{})
		if err := rotator.AddRotator(mgr, &rotator.CertRotator{
			SecretKey:      types.NamespacedName{Namespace: podNamespace, Name: webhookCertSecret},
			CertDir:        webhookCertPath,
			CAName:         "k8s-injection-controller",
			CAOrganization: "grafana.com",
			DNSName:        dnsName,
			ExtraDNSNames:  extraDNSNames,
			IsReady:        setupFinished,
			Webhooks: []rotator.WebhookInfo{
				{Name: mutatingWebhookName, Type: rotator.Mutating},
				{Name: validatingWebhookName, Type: rotator.Validating},
			},
			// Keep the webhook reconcile path from racing the startup cert
			// refresh; it only needs to inject the CA after the Secret has been
			// projected into this pod.
			EnableReadinessCheck: true,
			// Every replica must provision its own on-disk cert from the shared
			// Secret, so the rotator must run on all replicas, not just the leader.
			RequireLeaderElection: false,
		}); err != nil {
			setupLog.Error(err, "Failed to set up webhook cert rotator")
			os.Exit(1)
		}
		setupLog.Info("self-managing webhook certificate via cert rotator",
			"secret", webhookCertSecret, "dnsName", dnsName)
		go func() {
			select {
			case <-setupFinished:
				setupLog.Info("webhook certificate ready; registering webhooks")
				if err := setupWebhooks(); err != nil {
					setupLog.Error(err, "Failed to register webhooks after cert rotation")
					os.Exit(1)
				}
			case <-signalCtx.Done():
				// Manager is shutting down (e.g. cert provisioning failed before the
				// cert was ready); webhooks were never registered. The process is
				// already on its way out via the mgr.Start error path.
				setupLog.Info("shutting down before webhook certificate was ready; webhooks not registered")
			}
		}()
	} else {
		// cert-manager (or externally) provided cert: register synchronously,
		// exactly as before.
		if err := setupWebhooks(); err != nil {
			setupLog.Error(err, "Failed to register webhooks")
			os.Exit(1)
		}
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}
	// Block readiness until the webhook listener is up. kube-proxy keeps the
	// pod out of the Service endpoints until /readyz returns 200, so the
	// apiserver won't get "connection refused" admissions during boot. Skipped
	// when webhooks are disabled: the server never starts, so this check would
	// never pass and the pod would never become Ready.
	if webhooksEnabled {
		if err := mgr.AddReadyzCheck("webhook", webhookServer.StartedChecker()); err != nil {
			setupLog.Error(err, "Failed to set up webhook ready check")
			os.Exit(1)
		}
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(signalCtx); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

// resolveInjectionMode resolves the "auto" InjectionMode to a concrete mode by
// querying the cluster's server version: we use direct ImageVolumeSource mode on
// k8s 1.31+, otherwise the disk-heavy init-container copy approach is used.
func resolveInjectionMode(cfg *config.SDKInject, clientset kubernetes.Interface) {
	if cfg.InjectionMode != config.InjectionModeAuto {
		setupLog.Info("using configured injection mode", "mode", cfg.InjectionMode)
		return
	}

	info, err := clientset.Discovery().ServerVersion()
	if err != nil {
		setupLog.Error(err, "Failed to determine server version; defaulting to init-container injection mode")
		cfg.InjectionMode = config.InjectionModeInitContainer
		return
	}

	major, minor, err := config.ParseServerVersion(info)
	if err != nil {
		setupLog.Error(err, "Failed to parse server version; defaulting to init-container injection mode",
			"gitVersion", info.GitVersion)
		cfg.InjectionMode = config.InjectionModeInitContainer
		return
	}

	if config.SupportsImageVolume(major, minor) {
		cfg.InjectionMode = config.InjectionModeImage
	} else {
		cfg.InjectionMode = config.InjectionModeInitContainer
	}
	setupLog.Info("injection mode",
		"serverVersion", info.GitVersion, "mode", cfg.InjectionMode)
}

// parseAllowedWriters splits a comma-separated list of usernames, trimming
// whitespace and dropping empty entries.
func parseAllowedWriters(raw string) []string {
	var out []string
	for u := range strings.SplitSeq(raw, ",") {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, u)
		}
	}
	return out
}
