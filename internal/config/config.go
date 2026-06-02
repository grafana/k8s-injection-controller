package config

import (
	"crypto/sha256"
	"fmt"

	bservices "github.com/grafana/beyla/v3/pkg/services"
	"github.com/grafana/beyla/v3/pkg/webhook/configmap"
	"go.opentelemetry.io/obi/pkg/appolly/services"
)

type SDKInject struct {
	// Option to disable automatic bouncing of pods, it will be
	// a responsibility of the end-user to bounce the pods to be instrumented
	NoAutoRestart bool `yaml:"disable_auto_restart"`
	// OCI image version mounted into pods via Kubernetes ImageVolumeSource.
	// Requires k8s 1.31+. Required — this is the only supported volume mode.
	ImageVersion string `yaml:"image_volume_version"`
	// OCI image reference mounted into pods via Kubernetes ImageVolumeSource.
	// This configuration appends the version info supplied by Beyla's config maps
	// or as a direct controller configuration option.
	ImageVolumeRoot string `yaml:"image_volume_root"`
	// Default sampler configuration for SDK instrumentation
	// This is used when no sampler is specified in the selector
	DefaultSampler *services.SamplerConfig `yaml:"trace_sampler"`
	// Propagators configuration for SDK instrumentation
	// Common values: tracecontext, baggage, b3, b3multi, jaeger, xray
	Propagators []string `yaml:"trace_propagators"`
	// ExportedSignals configuration for SDK instrumentation
	// Controls which signals (traces, metrics, logs) should be exported from injected SDKs
	ExportedSignals configmap.SDKExportedSignals `yaml:"otel_exported_signals"`
	// Resource attributes related settings
	Resources configmap.SDKResource `yaml:"resources"`
	// List of enabled SDK auto-instrumentations. Can be used to disable specific
	// language instrumentations.
	EnabledSDKs []bservices.InstrumentableType `yaml:"enabled_sdks"`
	// Enables injection debugging
	Debug bool `yaml:"debug"`
}

const DefaultImageVolumeRoot = "ghcr.io/grafana/beyla/inject-sdk-image"

// SetDefaults populates zero-value fields with their defaults.
// Call this after unmarshalling from YAML or constructing an empty SDKInject.
func (s *SDKInject) SetDefaults() {
	if s.ImageVolumeRoot == "" {
		s.ImageVolumeRoot = DefaultImageVolumeRoot
	}
}

func (s *SDKInject) ImageVolumePath() string {
	return s.ImageVolumeRoot + ":" + s.ImageVersion
}

// PackageVersion returns a stable, label-safe identifier derived from the
// configured image reference. SHA-224 keeps it within the 63-char k8s label
// limit so callers can stamp it onto pods without truncation.
func (s *SDKInject) PackageVersion() string {
	h := sha256.Sum224([]byte(s.ImageVolumePath()))
	return fmt.Sprintf("%x", h)
}

// WithConfigMapOverrides returns a copy of s with any per-ConfigMap overrides
// from cfg applied on top. Zero/nil fields on cfg are treated as "no override"
// and leave the corresponding default in place.
func (s SDKInject) WithConfigMapOverrides(cfg configmap.InjectConfig) SDKInject {
	out := s
	if cfg.ImageVersion != "" {
		out.ImageVersion = cfg.ImageVersion
	}
	if cfg.DefaultSampler != nil {
		out.DefaultSampler = cfg.DefaultSampler
	}
	if len(cfg.Propagators) > 0 {
		out.Propagators = cfg.Propagators
	}
	if cfg.ExportedSignals.Traces != nil {
		out.ExportedSignals.Traces = cfg.ExportedSignals.Traces
	}
	if cfg.ExportedSignals.Metrics != nil {
		out.ExportedSignals.Metrics = cfg.ExportedSignals.Metrics
	}
	if cfg.ExportedSignals.Logs != nil {
		out.ExportedSignals.Logs = cfg.ExportedSignals.Logs
	}
	if r := cfg.Resources; r.Attributes != nil || r.AddK8sUIDAttributes ||
		r.AddK8sIPAttribute || r.UseLabelsForResourceAttributes {
		out.Resources = configmap.SDKResource{
			Attributes:                     r.Attributes,
			AddK8sUIDAttributes:            r.AddK8sUIDAttributes,
			AddK8sIPAttribute:              r.AddK8sIPAttribute,
			UseLabelsForResourceAttributes: r.UseLabelsForResourceAttributes,
		}
	}
	return out
}
