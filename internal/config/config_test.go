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
