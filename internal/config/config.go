package config

import (
	"crypto/sha256"
	"fmt"

	bservices "github.com/grafana/beyla/v3/pkg/services"
	"go.opentelemetry.io/obi/pkg/appolly/services"
)

type SDKInject struct {
	// Option to disable automatic bouncing of pods, it will be
	// a responsibility of the end-user to bounce the pods to be instrumented
	NoAutoRestart bool `yaml:"disable_auto_restart"`
	// OCI image mount instead of host volume, supported on k8s 1.31+
	ImageVolumePath string `yaml:"image_volume_path"`
	// The host path volume directory which gets mounted into pods
	HostPathVolumeDir string `yaml:"host_path_volume"`
	// The mutator will set the version on pods if this value is set
	// This is used to let Beyla upgrade already instrumented services
	// If the version doesn't match we still bounce existing pods
	SDKPkgVersion string `yaml:"sdk_package_version"`
	// The host mount path where the SDK copy init container copies the files.
	// This is the root path, sdk_version is appended on top
	HostMountPath string `yaml:"host_mount_path"`
	// Tells Beyla that it should delete old SDK versions on the
	// host mount volume. Default true.
	ManageSDKVersions bool `yaml:"manage_sdk_versions"`
	// Default sampler configuration for SDK instrumentation
	// This is used when no sampler is specified in the selector
	DefaultSampler *services.SamplerConfig `yaml:"sampler"`
	// Propagators configuration for SDK instrumentation
	// Common values: tracecontext, baggage, b3, b3multi, jaeger, xray
	Propagators []string `yaml:"propagators"`
	// Export configuration for SDK instrumentation
	// Controls which signals (traces, metrics, logs) should be exported from injected SDKs
	Export SDKExport `yaml:"export"`
	// Resource attributes related settings
	Resources SDKResource `yaml:"resources"`
	// List of enabled SDK auto-instrumentations. Can be used to disable specific
	// language instrumentations.
	EnabledSDKs []bservices.InstrumentableType `yaml:"enabled_sdks"`
	// Enables injection debugging
	Debug        bool   `yaml:"debug"`
	OTELEndpoint string `yaml:"otel_endpoint"`
	OTELProtocol string `yaml:"otel_protocol"`
}

func (s *SDKInject) PackageVersion() string {
	if s.ImageVolumePath != "" {
		h := sha256.Sum224([]byte(s.ImageVolumePath))
		return fmt.Sprintf("%x", h) // 56 chars, fits in 63-char label limit
	}

	return s.SDKPkgVersion
}

func (s *SDKInject) UsesImageVolume() bool {
	return s.ImageVolumePath != ""
}

// SDKExport defines which telemetry signals should be exported from injected SDKs.
// These settings are independent from the global export configuration and allow
// the injector to export metrics/traces/logs even when Beyla uses Prometheus for metrics.
type SDKExport struct {
	// Traces enables trace export from injected SDKs via OTLP
	// Defaults to true (enabled) when not explicitly set
	Traces *bool `yaml:"traces" env:"BEYLA_SDK_EXPORT_TRACES"`
	// Metrics enables metric export from injected SDKs via OTLP
	// Defaults to true (enabled) when not explicitly set
	// Note: SDKs can only export via OTLP, not Prometheus scraping
	Metrics *bool `yaml:"metrics" env:"BEYLA_SDK_EXPORT_METRICS"`
	// Logs enables log export from injected SDKs via OTLP
	// Defaults to false (disabled) when not explicitly set
	Logs *bool `yaml:"logs" env:"BEYLA_SDK_EXPORT_LOGS"`
}

// TracesEnabled returns whether trace export is enabled for SDK instrumentation
// Defaults to true when not explicitly set
func (e SDKExport) TracesEnabled() bool {
	if e.Traces == nil {
		return true // default to enabled
	}
	return *e.Traces
}

// MetricsEnabled returns whether metric export is enabled for SDK instrumentation
// Defaults to true when not explicitly set
func (e SDKExport) MetricsEnabled() bool {
	if e.Metrics == nil {
		return true // default to enabled
	}
	return *e.Metrics
}

// LogsEnabled returns whether log export is enabled for SDK instrumentation
// Defaults to false when not explicitly set
func (e SDKExport) LogsEnabled() bool {
	if e.Logs == nil {
		return false // default to disabled
	}
	return *e.Logs
}

// Resource defines the configuration for the resource attributes, as defined by the OpenTelemetry specification.
// See also: https://github.com/open-telemetry/opentelemetry-specification/blob/v1.8.0/specification/overview.md#resources
type SDKResource struct {
	// Attributes defines attributes that are added to the resource.
	// For example environment: dev
	// +optional
	Attributes map[string]string `yaml:"resourceAttributes" env:"BEYLA_RESOURCE_ATTRIBUTES"`

	// AddK8sUIDAttributes defines whether K8s UID attributes should be collected (e.g. k8s.deployment.uid).
	// +optional
	AddK8sUIDAttributes bool `yaml:"addK8sUIDAttributes" env:"BEYLA_RESOURCE_ADD_K8S_UID_ATTRIBUTES"`

	// AddK8sIPAttribute defines whether the k8s.pod.ip resource attribute should be set
	// from the Kubernetes downward API (status.podIP). Useful for environments where the
	// OTel k8sattributesprocessor cannot infer the pod IP from the connection source
	// (e.g. clusters behind a NAT gateway).
	// +optional
	AddK8sIPAttribute bool `yaml:"addK8sIPAttribute" env:"BEYLA_RESOURCE_ADD_K8S_IP_ATTRIBUTE"`

	// UseLabelsForResourceAttributes defines whether to use common labels for resource attributes:
	// Note: first entry wins:
	//   - `app.kubernetes.io/instance` becomes `service.name`
	//   - `app.kubernetes.io/name` becomes `service.name`
	//   - `app.kubernetes.io/version` becomes `service.version`
	UseLabelsForResourceAttributes bool `yaml:"useLabelsForResourceAttributes,omitempty" env:"BEYLA_RESOURCE_USE_LABELS_FOR_RESOURCE_ATTRIBUTES"`
}
