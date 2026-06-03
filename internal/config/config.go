package config

import (
	"crypto/sha256"
	"fmt"

	bservices "github.com/grafana/beyla/v3/pkg/services"
	"github.com/grafana/beyla/v3/pkg/webhook/configmap"
)

type SDKInject struct {
	// Option to disable automatic bouncing of pods, it will be
	// a responsibility of the end-user to bounce the pods to be instrumented
	NoAutoRestart bool `yaml:"disable_auto_restart"`
	// OCI image version mounted into pods via Kubernetes ImageVolumeSource.
	// Requires k8s 1.31+. Combined with ImageVolumeRoot to form the full image
	// reference. Beyla may override this per-ConfigMap via InjectConfig.ImageVersion.
	ImageVersion string `yaml:"image_version"`
	// OCI image repository mounted into pods via Kubernetes ImageVolumeSource.
	// The version (from this config or a Beyla ConfigMap) is appended to form the
	// full reference, e.g. "<root>:<version>".
	ImageVolumeRoot string `yaml:"image_volume_root"`
	// Resource attributes related settings
	Resources configmap.SDKResource `yaml:"resources"`
	// List of enabled SDK auto-instrumentations. Can be used to disable specific
	// language instrumentations.
	EnabledSDKs []bservices.InstrumentableType `yaml:"enabled_sdks"`
	// Debug enables verbose injector logging (OTEL_INJECTOR_LOG_LEVEL=debug) on
	// instrumented containers. Unlike the signal/sampler/propagator config, Beyla
	// does not write this as a per-rule env var, so it stays a controller-side knob.
	Debug bool `yaml:"debug"`
}

const DefaultImageVolumeRoot = "ghcr.io/grafana/beyla/inject-sdk-image"

// SetDefaults populates zero-value fields with their defaults.
// Call this after unmarshalling from YAML or constructing an empty SDKInject.
func (s *SDKInject) SetDefaults() {
	if s.ImageVolumeRoot == "" {
		s.ImageVolumeRoot = DefaultImageVolumeRoot
	}
	if s.EnabledSDKs == nil {
		for _, lang := range []string{"java", "dotnet", "nodejs", "python"} {
			if t, err := bservices.ParseInstrumentableType(lang); err == nil {
				s.EnabledSDKs = append(s.EnabledSDKs, bservices.InstrumentableType{InstrumentableType: t})
			}
		}
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
// from cfg applied on top. The merged Beyla webhook config (#2819) carries only
// the image version at the InjectConfig level; all per-rule instrumentation
// config now travels as env vars in each Rule.Config.Env. A zero/empty
// ImageVersion is treated as "no override" and leaves the controller default in
// place.
func (s SDKInject) WithConfigMapOverrides(cfg configmap.InjectConfig) SDKInject {
	out := s
	if cfg.ImageVersion != "" {
		out.ImageVersion = cfg.ImageVersion
	}
	return out
}
