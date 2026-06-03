/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package config

import (
	"reflect"
	"testing"

	"github.com/grafana/beyla/v3/pkg/webhook/configmap"
	"go.opentelemetry.io/obi/pkg/appolly/services"
)

func TestWithConfigMapOverrides(t *testing.T) {
	base := SDKInject{
		ImageVersion:    "0",
		ImageVolumeRoot: "default",
		DefaultSampler:  &services.SamplerConfig{Name: "parentbased_always_on"},
		Propagators:     []string{"tracecontext"},
		ExportedSignals: configmap.SDKExportedSignals{
			Traces:  new(true),
			Metrics: new(true),
			Logs:    new(false),
		},
		Resources: configmap.SDKResource{
			Attributes:          map[string]string{"env": "dev"},
			AddK8sUIDAttributes: false,
			AddK8sIPAttribute:   false,
		},
	}

	t.Run("empty wire config keeps every default", func(t *testing.T) {
		got := base.WithConfigMapOverrides(configmap.InjectConfig{})
		if !reflect.DeepEqual(got, base) {
			t.Fatalf("expected base unchanged.\n got: %+v\nwant: %+v", got, base)
		}
	})

	t.Run("ImageVersion override wins when set; empty preserves default", func(t *testing.T) {
		got := base.WithConfigMapOverrides(configmap.InjectConfig{ImageVersion: "9"})
		if got.ImageVersion != "9" {
			t.Fatalf("ImageVersion = %q, want %q", got.ImageVersion, "9")
		}
		got = base.WithConfigMapOverrides(configmap.InjectConfig{ImageVersion: ""})
		if got.ImageVersion != base.ImageVersion {
			t.Fatalf("empty override should preserve default, got %q", got.ImageVersion)
		}
	})

	t.Run("DefaultSampler nil preserves default; set replaces", func(t *testing.T) {
		got := base.WithConfigMapOverrides(configmap.InjectConfig{DefaultSampler: nil})
		if got.DefaultSampler != base.DefaultSampler {
			t.Fatalf("nil sampler should preserve default")
		}
		newSampler := &services.SamplerConfig{Name: "traceidratio", Arg: "0.1"}
		got = base.WithConfigMapOverrides(configmap.InjectConfig{DefaultSampler: newSampler})
		if got.DefaultSampler != newSampler {
			t.Fatalf("DefaultSampler not replaced: %+v", got.DefaultSampler)
		}
	})

	t.Run("Propagators empty preserves default; set replaces", func(t *testing.T) {
		got := base.WithConfigMapOverrides(configmap.InjectConfig{Propagators: nil})
		if !reflect.DeepEqual(got.Propagators, base.Propagators) {
			t.Fatalf("nil propagators should preserve default, got %v", got.Propagators)
		}
		got = base.WithConfigMapOverrides(configmap.InjectConfig{Propagators: []string{}})
		if !reflect.DeepEqual(got.Propagators, base.Propagators) {
			t.Fatalf("empty propagators slice should preserve default, got %v", got.Propagators)
		}
		got = base.WithConfigMapOverrides(configmap.InjectConfig{Propagators: []string{"b3", "baggage"}})
		if !reflect.DeepEqual(got.Propagators, []string{"b3", "baggage"}) {
			t.Fatalf("Propagators not replaced: %v", got.Propagators)
		}
	})

	t.Run("ExportedSignals override is per-signal", func(t *testing.T) {
		// Only Metrics overridden; Traces/Logs preserve defaults.
		got := base.WithConfigMapOverrides(configmap.InjectConfig{
			ExportedSignals: configmap.SDKExportedSignals{Metrics: new(false)},
		})
		if got.ExportedSignals.Traces == nil || *got.ExportedSignals.Traces != true {
			t.Fatalf("Traces default lost: %+v", got.ExportedSignals.Traces)
		}
		if got.ExportedSignals.Metrics == nil || *got.ExportedSignals.Metrics != false {
			t.Fatalf("Metrics not overridden: %+v", got.ExportedSignals.Metrics)
		}
		if got.ExportedSignals.Logs == nil || *got.ExportedSignals.Logs != false {
			t.Fatalf("Logs default lost: %+v", got.ExportedSignals.Logs)
		}
	})

	t.Run("Resources block is all-or-nothing", func(t *testing.T) {
		// Zero block preserves the default.
		got := base.WithConfigMapOverrides(configmap.InjectConfig{Resources: configmap.SDKResource{}})
		if !reflect.DeepEqual(got.Resources, base.Resources) {
			t.Fatalf("zero Resources should preserve default, got %+v", got.Resources)
		}
		// Any non-zero field flips the whole block to the override.
		got = base.WithConfigMapOverrides(configmap.InjectConfig{
			Resources: configmap.SDKResource{AddK8sUIDAttributes: true},
		})
		want := configmap.SDKResource{AddK8sUIDAttributes: true}
		if !reflect.DeepEqual(got.Resources, want) {
			t.Fatalf("Resources override: got %+v want %+v", got.Resources, want)
		}
		// The base's Attributes do NOT leak through — override replaces wholesale.
		if got.Resources.Attributes != nil {
			t.Fatalf("base Attributes should not leak through Resources override: %+v", got.Resources.Attributes)
		}
	})

	t.Run("Resources triggers on Attributes alone", func(t *testing.T) {
		got := base.WithConfigMapOverrides(configmap.InjectConfig{
			Resources: configmap.SDKResource{Attributes: map[string]string{"region": "eu"}},
		})
		if !reflect.DeepEqual(got.Resources.Attributes, map[string]string{"region": "eu"}) {
			t.Fatalf("Attributes-only override not applied: %+v", got.Resources)
		}
	})

	t.Run("source struct is not mutated", func(t *testing.T) {
		snapshot := base
		_ = base.WithConfigMapOverrides(configmap.InjectConfig{
			ImageVersion: "x",
			Propagators:  []string{"b3"},
			Resources:    configmap.SDKResource{AddK8sIPAttribute: true},
		})
		if !reflect.DeepEqual(base, snapshot) {
			t.Fatalf("base was mutated by WithConfigMapOverrides")
		}
	})
}
