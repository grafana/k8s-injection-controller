package config

import (
	"crypto/sha256"
	"fmt"

	"github.com/grafana/beyla-k8s-injector/internal/registry"
	bservices "github.com/grafana/beyla/v3/pkg/services"
	"github.com/grafana/beyla/v3/pkg/webhook/configmap"
	"go.opentelemetry.io/obi/pkg/appolly/services"
)

type SDKInject struct {
	// Option to disable automatic bouncing of pods, it will be
	// a responsibility of the end-user to bounce the pods to be instrumented
	NoAutoRestart bool `yaml:"disable_auto_restart"`
	// OCI image reference mounted into pods via Kubernetes ImageVolumeSource.
	// Requires k8s 1.31+. Required — this is the only supported volume mode.
	ImageVolumePath string `yaml:"image_volume_path"`
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

// PackageVersion returns a stable, label-safe identifier derived from the
// configured image reference. SHA-224 keeps it within the 63-char k8s label
// limit so callers can stamp it onto pods without truncation.
func (s *SDKInject) PackageVersion() string {
	h := sha256.Sum224([]byte(s.ImageVolumePath))
	return fmt.Sprintf("%x", h)
}

func (s *SDKInject) UpdateWithInstrumentation(inst *registry.Instrumentation) {
	s.ImageVolumePath = inst.ImageVolumePath
	s.DefaultSampler = inst.DefaultSampler
	s.Propagators = inst.Propagators
	s.ExportedSignals = inst.ExportedSignals
	s.Resources = inst.Resources
}
