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
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

func TestWithConfigMapOverrides(t *testing.T) {
	base := SDKInject{
		ImageVersion:    "0",
		ImageVolumeRoot: "default",
	}

	t.Run("empty wire config keeps the default version", func(t *testing.T) {
		got := base.WithConfigMapOverrides(configmap.InjectConfig{})
		if got.ImageVersion != base.ImageVersion {
			t.Fatalf("ImageVersion = %q, want %q", got.ImageVersion, base.ImageVersion)
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

	t.Run("source struct is not mutated", func(t *testing.T) {
		snapshot := base
		_ = base.WithConfigMapOverrides(configmap.InjectConfig{ImageVersion: "x"})
		if !reflect.DeepEqual(base, snapshot) {
			t.Fatalf("base was mutated by WithConfigMapOverrides")
		}
	})
}

func TestWithRuleOverrides(t *testing.T) {
	base := SDKInject{JavaExtensionImages: []string{"registry.example/default:v1"}}
	t.Run("rule images replace defaults in order", func(t *testing.T) {
		got := base.WithRuleOverrides(configmap.RuleConfig{Env: []corev1.EnvVar{{
			Name: JavaExtensionImagesEnv, Value: " registry.example/first:v1, ,registry.example/second:v2 ",
		}}})
		assert.Equal(t, []string{"registry.example/first:v1", "registry.example/second:v2"}, got.JavaExtensionImages)
		assert.Equal(t, []string{"registry.example/default:v1"}, base.JavaExtensionImages)
	})
	t.Run("explicit empty value disables defaults", func(t *testing.T) {
		got := base.WithRuleOverrides(configmap.RuleConfig{Env: []corev1.EnvVar{{Name: JavaExtensionImagesEnv}}})
		assert.Empty(t, got.JavaExtensionImages)
	})
	t.Run("unrelated env leaves defaults", func(t *testing.T) {
		got := base.WithRuleOverrides(configmap.RuleConfig{Env: []corev1.EnvVar{{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: "http://collector"}}})
		assert.Equal(t, base.JavaExtensionImages, got.JavaExtensionImages)
	})
}

func TestPodConfigHash(t *testing.T) {
	t.Run("idempotency", func(t *testing.T) {
		assert.Equal(t,
			PodConfigHash(
				&SDKInject{ImageVolumeRoot: "foo/sdks", ImageVersion: "3.12"},
				&configmap.RuleConfig{Mode: configmap.ModeSkip}),
			PodConfigHash(
				&SDKInject{ImageVolumeRoot: "foo/sdks", ImageVersion: "3.12"},
				&configmap.RuleConfig{Mode: configmap.ModeSkip}))
	})
	t.Run("Java extension image changes hash", func(t *testing.T) {
		rule := &configmap.RuleConfig{Mode: configmap.ModeInstall}
		first := PodConfigHash(&SDKInject{ImageVolumeRoot: "foo/sdks", ImageVersion: "3.12", JavaExtensionImages: []string{"example/ext:v1"}}, rule)
		second := PodConfigHash(&SDKInject{ImageVolumeRoot: "foo/sdks", ImageVersion: "3.12", JavaExtensionImages: []string{"example/ext:v2"}}, rule)
		assert.NotEqual(t, first, second)
	})
	t.Run("per-rule Java extension image changes hash", func(t *testing.T) {
		cfg := &SDKInject{ImageVolumeRoot: "foo/sdks", ImageVersion: "3.12"}
		first := PodConfigHash(cfg, &configmap.RuleConfig{Env: []corev1.EnvVar{{Name: JavaExtensionImagesEnv, Value: "example/ext:v1"}}})
		second := PodConfigHash(cfg, &configmap.RuleConfig{Env: []corev1.EnvVar{{Name: JavaExtensionImagesEnv, Value: "example/ext:v2"}}})
		assert.NotEqual(t, first, second)
	})
	t.Run("no collisions", func(t *testing.T) {
		history := map[string]struct{}{
			PodConfigHash(
				&SDKInject{ImageVolumeRoot: "foo/sdks", ImageVersion: "3.12"},
				&configmap.RuleConfig{Mode: configmap.ModeSkip}): {},
		}
		v := PodConfigHash(
			&SDKInject{ImageVolumeRoot: "bar/sdks", ImageVersion: "3.12"},
			&configmap.RuleConfig{Mode: configmap.ModeSkip})
		assert.NotContains(t, history, v)
		history[v] = struct{}{}
		v = PodConfigHash(
			&SDKInject{ImageVolumeRoot: "bar/sdks", ImageVersion: "3.11"},
			&configmap.RuleConfig{Mode: configmap.ModeSkip})
		assert.NotContains(t, history, v)
		history[v] = struct{}{}
		v = PodConfigHash(
			&SDKInject{ImageVolumeRoot: "bar/sdks", ImageVersion: "3.11"},
			&configmap.RuleConfig{Mode: configmap.ModeInstall})
		assert.NotContains(t, history, v)
	})
}
