package config

import (
	"testing"

	"k8s.io/apimachinery/pkg/version"
)

func TestSetDefaultsInjectionMode(t *testing.T) {
	t.Run("empty mode defaults to auto", func(t *testing.T) {
		s := SDKInject{}
		s.SetDefaults()
		if s.InjectionMode != InjectionModeAuto {
			t.Fatalf("InjectionMode = %q, want %q", s.InjectionMode, InjectionModeAuto)
		}
	})

	t.Run("explicit mode is preserved", func(t *testing.T) {
		s := SDKInject{InjectionMode: InjectionModeImage}
		s.SetDefaults()
		if s.InjectionMode != InjectionModeImage {
			t.Fatalf("InjectionMode = %q, want %q", s.InjectionMode, InjectionModeImage)
		}
	})
}

func TestParseServerVersion(t *testing.T) {
	cases := []struct {
		name      string
		info      version.Info
		wantMajor int
		wantMinor int
		wantErr   bool
	}{
		{"plain", version.Info{Major: "1", Minor: "31"}, 1, 31, false},
		{"gke plus suffix", version.Info{Major: "1", Minor: "31+"}, 1, 31, false},
		{"falls back to gitVersion", version.Info{GitVersion: "v1.30.5-gke.1234"}, 1, 30, false},
		{"gitVersion no v prefix", version.Info{GitVersion: "1.29.0"}, 1, 29, false},
		{"unparseable", version.Info{Major: "x", Minor: "y", GitVersion: "garbage"}, 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			major, minor, err := ParseServerVersion(&tc.info)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got major=%d minor=%d", major, minor)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if major != tc.wantMajor || minor != tc.wantMinor {
				t.Fatalf("got %d.%d, want %d.%d", major, minor, tc.wantMajor, tc.wantMinor)
			}
		})
	}
}

func TestSupportsImageVolume(t *testing.T) {
	cases := []struct {
		major, minor int
		want         bool
	}{
		{1, 30, false},
		{1, 31, false},
		{1, 32, false},
		{1, 35, false},
		{2, 0, true},
		{0, 99, false},
	}
	for _, tc := range cases {
		if got := SupportsImageVolume(tc.major, tc.minor); got != tc.want {
			t.Fatalf("SupportsImageVolume(%d, %d) = %v, want %v", tc.major, tc.minor, got, tc.want)
		}
	}
}
