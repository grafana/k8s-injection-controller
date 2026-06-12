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
