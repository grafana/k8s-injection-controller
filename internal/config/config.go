package config

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	bservices "github.com/grafana/beyla/v3/pkg/services"
	"github.com/grafana/beyla/v3/pkg/webhook/configmap"
	"go.opentelemetry.io/obi/pkg/appolly/services"
	"k8s.io/apimachinery/pkg/version"
)

// InjectionMode selects how the SDK auto-instrumentation payload is delivered
// into instrumented pods.
type InjectionMode string

const (
	// InjectionModeImage mounts the SDK image directly via a Kubernetes
	// ImageVolumeSource. Requires k8s 1.31+.
	InjectionModeImage InjectionMode = "image"
	// InjectionModeInitContainer provisions an ephemeral emptyDir volume and
	// adds an init container that copies the SDK payload into it. Works on
	// clusters older than 1.31 that lack ImageVolumeSource support.
	InjectionModeInitContainer InjectionMode = "init_container"
	// InjectionModeAuto resolves to InjectionModeImage on k8s 1.31+ and
	// InjectionModeInitContainer for older versions. The check runs once at controller
	// boot against the cluster's reported server version.
	InjectionModeAuto InjectionMode = "auto"
)

type SDKInject struct {
	// Option to disable automatic bouncing of pods, it will be
	// a responsibility of the end-user to bounce the pods to be instrumented
	NoAutoRestart bool `yaml:"disable_auto_restart"`
	// Version tag of the SDK OCI image. Required. Combined with ImageVolumeRoot
	// to form the image reference used by both injection modes (mounted
	// directly via ImageVolumeSource, or run as the copy init container).
	ImageVersion string `yaml:"image_version"`
	// OCI image repository carrying the SDK payload. This configuration appends
	// the version info supplied by Beyla's config maps or as a direct
	// controller configuration option. See InjectionMode for how it is used.
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
	// InjectionMode selects how the SDK payload is delivered into pods:
	// "image" (direct ImageVolumeSource, k8s 1.31+), "init_container"
	// (ephemeral volume populated by a copy init container) or "auto"
	// (resolved at boot from the cluster's server version). Defaults to "auto".
	InjectionMode InjectionMode `yaml:"injection_mode"`
	// Enables injection debugging
	Debug bool `yaml:"debug"`
}

const DefaultImageVolumeRoot = "ghcr.io/grafana/beyla/inject-sdk-image"

// EphemeralVolumeSize bounds the emptyDir volume provisioned in
// init-container mode. 250Mi leaves some amount of headroom, since
// the actual current image size is a bit less than 200Mi.
const EphemeralVolumeSize = "250Mi"

// SetDefaults populates zero-value fields with their defaults.
// Call this after unmarshalling from YAML or constructing an empty SDKInject.
func (s *SDKInject) SetDefaults() {
	if s.ImageVolumeRoot == "" {
		s.ImageVolumeRoot = DefaultImageVolumeRoot
	}
	if s.InjectionMode == "" {
		s.InjectionMode = InjectionModeAuto
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

// versionRegex extracts the major and minor components from a version
// string such as "v1.31.5-gke.1234" or "1.29.0".
var versionRegex = regexp.MustCompile(`^v?(\d+)\.(\d+)`)

// ParseServerVersion extracts the integer major and minor version from the
// cluster's reported server version. It first tries the dedicated Major/Minor
// fields (trimming non-numeric suffixes like the "31+" some managed providers
// report), falling back to parsing version when those are unusable.
func ParseServerVersion(info *version.Info) (major, minor int, err error) {
	major, errMajor := parseVersionComponent(info.Major)
	minor, errMinor := parseVersionComponent(info.Minor)
	if errMajor == nil && errMinor == nil {
		return major, minor, nil
	}

	if m := versionRegex.FindStringSubmatch(info.GitVersion); m != nil {
		major, _ = strconv.Atoi(m[1])
		minor, _ = strconv.Atoi(m[2])
		return major, minor, nil
	}

	return 0, 0, fmt.Errorf("cannot parse server version: major=%q minor=%q gitVersion=%q",
		info.Major, info.Minor, info.GitVersion)
}

// parseVersionComponent parses a version component, tolerating a trailing
// non-numeric suffix (e.g. the "+" in the "31+" minor that GKE reports).
func parseVersionComponent(s string) (int, error) {
	s = strings.TrimRight(s, "+")
	return strconv.Atoi(s)
}

// SupportsImageVolume reports whether the given server version supports the
// Kubernetes ImageVolumeSource, which graduated to beta (on by default) in
// 1.31.
func SupportsImageVolume(major, minor int) bool {
	return major > 1 || (major == 1 && minor >= 31)
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
