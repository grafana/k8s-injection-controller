package v1

import (
	"slices"
	"testing"

	"github.com/grafana/beyla-k8s-injector/internal/config"
	bservices "github.com/grafana/beyla/v3/pkg/services"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newModeMutator(mode config.InjectionMode) *PodMutator {
	return &PodMutator{Cfg: config.SDKInject{
		ImageVolumeRoot: "ghcr.io/grafana/beyla/inject-sdk-image",
		ImageVersion:    "0.0.13",
		InjectionMode:   mode,
	}}
}

func TestJavaExtensionImagesAreMountedInImageMode(t *testing.T) {
	pm := newModeMutator(config.InjectionModeImage)
	java, err := bservices.ParseInstrumentableType("java")
	if err != nil {
		t.Fatal(err)
	}
	pm.Cfg.EnabledSDKs = []bservices.InstrumentableType{{InstrumentableType: java}}
	pm.Cfg.JavaExtensionImages = []string{"example.test/team/java-ext:v1", "example.test/team/another:v2"}
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "example.test/app", Env: []corev1.EnvVar{{Name: javaAgentExtensionsEnv, Value: "/custom/ext"}}}}}

	pm.mountVolume(spec)
	meta := &metav1.ObjectMeta{}
	pm.instrumentContainer(meta, &spec.Containers[0], nil, false)

	if len(spec.Volumes) != 3 {
		t.Fatalf("volumes = %d, want SDK volume plus 2 extension image volumes", len(spec.Volumes))
	}
	for i, image := range pm.Cfg.JavaExtensionImages {
		name := javaExtensionVolumeName(i)
		volumeIndex := slices.IndexFunc(spec.Volumes, func(v corev1.Volume) bool { return v.Name == name })
		if volumeIndex < 0 || spec.Volumes[volumeIndex].Image == nil || spec.Volumes[volumeIndex].Image.Reference != image {
			t.Fatalf("extension volume %q not configured for image %q: %+v", name, image, spec.Volumes)
		}
		mountIndex := slices.IndexFunc(spec.Containers[0].VolumeMounts, func(v corev1.VolumeMount) bool { return v.Name == name })
		if mountIndex < 0 || !spec.Containers[0].VolumeMounts[mountIndex].ReadOnly {
			t.Fatalf("extension volume %q not mounted read-only: %+v", name, spec.Containers[0].VolumeMounts)
		}
	}
	if got := testEnvValue(spec.Containers[0].Env); got != "/custom/ext,"+javaExtensionsMountRoot+"/0/extensions,"+javaExtensionsMountRoot+"/1/extensions" {
		t.Fatalf("%s = %q", javaAgentExtensionsEnv, got)
	}

	// Mutation should be idempotent and retain an extension path configured by
	// the pod author.
	pm.instrumentContainer(meta, &spec.Containers[0], nil, false)
	if got := testEnvValue(spec.Containers[0].Env); got != "/custom/ext,"+javaExtensionsMountRoot+"/0/extensions,"+javaExtensionsMountRoot+"/1/extensions" {
		t.Fatalf("%s after reinstrumentation = %q", javaAgentExtensionsEnv, got)
	}
}

func TestJavaExtensionImagesHaveNoInitContainerFallback(t *testing.T) {
	pm := newModeMutator(config.InjectionModeInitContainer)
	java, err := bservices.ParseInstrumentableType("java")
	if err != nil {
		t.Fatal(err)
	}
	pm.Cfg.EnabledSDKs = []bservices.InstrumentableType{{InstrumentableType: java}}
	pm.Cfg.JavaExtensionImages = []string{"example.test/team/java-ext:v1"}
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "example.test/app"}}}

	pm.mountVolume(spec)
	pm.addCopyInitContainerIfNeeded(spec)
	pm.instrumentContainer(&metav1.ObjectMeta{}, &spec.Containers[0], nil, false)

	if len(spec.Volumes) != 1 || spec.Volumes[0].Name != injectVolumeName || spec.Volumes[0].Image != nil {
		t.Fatalf("volumes = %+v, want only the SDK emptyDir volume", spec.Volumes)
	}
	if len(spec.InitContainers) != 1 || spec.InitContainers[0].Name != injectInitContainerName {
		t.Fatalf("init containers = %+v, want only the SDK copy init container", spec.InitContainers)
	}
	if got := testEnvValue(spec.Containers[0].Env); got != "" {
		t.Fatalf("%s = %q, want no extension image fallback", javaAgentExtensionsEnv, got)
	}
}

func TestJavaExtensionImagesRequireEnabledJavaInstrumentation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		enabledSDK string
	}{
		{name: "Java disabled", enabledSDK: "nodejs"},
		{name: "non-Java rule", enabledSDK: "python"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pm := newModeMutator(config.InjectionModeImage)
			sdk, err := bservices.ParseInstrumentableType(tc.enabledSDK)
			if err != nil {
				t.Fatal(err)
			}
			pm.Cfg.EnabledSDKs = []bservices.InstrumentableType{{InstrumentableType: sdk}}
			pm.Cfg.JavaExtensionImages = []string{"example.test/team/java-ext:v1"}
			spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}
			pm.mountVolume(spec)
			pm.instrumentContainer(&metav1.ObjectMeta{}, &spec.Containers[0], nil, false)

			if slices.ContainsFunc(spec.Volumes, func(v corev1.Volume) bool { return v.Image != nil && v.Name != injectVolumeName }) {
				t.Fatalf("unexpected extension image volume for %s: %+v", tc.name, spec.Volumes)
			}
			if got := testEnvValue(spec.Containers[0].Env); got != "" {
				t.Fatalf("%s = %q, want empty for %s", javaAgentExtensionsEnv, got, tc.name)
			}
		})
	}
}

func TestJavaExtensionImagesIgnoreEmptyAndInvalidReferences(t *testing.T) {
	pm := newModeMutator(config.InjectionModeImage)
	java, err := bservices.ParseInstrumentableType("java")
	if err != nil {
		t.Fatal(err)
	}
	pm.Cfg.EnabledSDKs = []bservices.InstrumentableType{{InstrumentableType: java}}
	pm.Cfg.JavaExtensionImages = []string{"", "   ", "not an image", "example.test/team/valid:v1"}
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}
	pm.mountVolume(spec)
	pm.instrumentContainer(&metav1.ObjectMeta{}, &spec.Containers[0], nil, false)

	if len(spec.Volumes) != 2 || spec.Volumes[1].Image.Reference != "example.test/team/valid:v1" {
		t.Fatalf("volumes = %+v, want SDK image and only the valid extension image", spec.Volumes)
	}
	if got, want := testEnvValue(spec.Containers[0].Env), javaExtensionsMountRoot+"/0/extensions"; got != want {
		t.Fatalf("%s = %q, want %q", javaAgentExtensionsEnv, got, want)
	}
}

func TestJavaExtensionImageListChangeReplacesPriorMutation(t *testing.T) {
	pm := newModeMutator(config.InjectionModeImage)
	java, err := bservices.ParseInstrumentableType("java")
	if err != nil {
		t.Fatal(err)
	}
	pm.Cfg.EnabledSDKs = []bservices.InstrumentableType{{InstrumentableType: java}}
	pm.Cfg.JavaExtensionImages = []string{"example.test/team/old:v1"}
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}
	meta := &metav1.ObjectMeta{}
	pm.mountVolume(spec)
	pm.instrumentContainer(meta, &spec.Containers[0], nil, false)

	pm.Cfg.JavaExtensionImages = []string{"example.test/team/new:v2"}
	pm.mountVolume(spec)
	pm.instrumentContainer(meta, &spec.Containers[0], nil, false)

	if len(spec.Volumes) != 2 || spec.Volumes[1].Image.Reference != "example.test/team/new:v2" {
		t.Fatalf("volumes after reinjection = %+v, want replaced extension image", spec.Volumes)
	}
	if got, want := testEnvValue(spec.Containers[0].Env), javaExtensionsMountRoot+"/0/extensions"; got != want {
		t.Fatalf("%s after reinjection = %q, want %q", javaAgentExtensionsEnv, got, want)
	}
	if len(spec.Containers[0].VolumeMounts) != 2 {
		t.Fatalf("volume mounts after reinjection = %+v, want SDK and one extension mount", spec.Containers[0].VolumeMounts)
	}
}

func testEnvValue(envVars []corev1.EnvVar) string {
	for _, env := range envVars {
		if env.Name == javaAgentExtensionsEnv {
			return env.Value
		}
	}
	return ""
}

func TestMountVolumeImageMode(t *testing.T) {
	pm := newModeMutator(config.InjectionModeImage)
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}}

	pm.mountVolume(spec)
	pm.addCopyInitContainerIfNeeded(spec)

	if len(spec.Volumes) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(spec.Volumes))
	}
	v := spec.Volumes[0]
	if v.Name != injectVolumeName {
		t.Fatalf("volume name = %q, want %q", v.Name, injectVolumeName)
	}
	if v.Image == nil {
		t.Fatalf("expected ImageVolumeSource in image mode")
	}
	if v.Image.Reference != "ghcr.io/grafana/beyla/inject-sdk-image:0.0.13" {
		t.Fatalf("image reference = %q", v.Image.Reference)
	}
	if v.EmptyDir != nil {
		t.Fatalf("did not expect emptyDir in image mode")
	}
	if len(spec.InitContainers) != 0 {
		t.Fatalf("did not expect an init container in image mode, got %d", len(spec.InitContainers))
	}
}

func TestMountVolumeInitContainerMode(t *testing.T) {
	pm := newModeMutator(config.InjectionModeInitContainer)
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}}

	pm.mountVolume(spec)
	pm.addCopyInitContainerIfNeeded(spec)

	// Ephemeral emptyDir volume with a size limit, not an image volume.
	if len(spec.Volumes) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(spec.Volumes))
	}
	v := spec.Volumes[0]
	if v.EmptyDir == nil {
		t.Fatalf("expected emptyDir volume in init-container mode")
	}
	if v.Image != nil {
		t.Fatalf("did not expect ImageVolumeSource in init-container mode")
	}
	if v.EmptyDir.SizeLimit == nil || v.EmptyDir.SizeLimit.String() != config.EphemeralVolumeSize {
		t.Fatalf("emptyDir size limit = %v, want %s", v.EmptyDir.SizeLimit, config.EphemeralVolumeSize)
	}

	// Copy init container present and correctly configured.
	if len(spec.InitContainers) != 1 {
		t.Fatalf("expected 1 init container, got %d", len(spec.InitContainers))
	}
	ic := spec.InitContainers[0]
	if ic.Name != injectInitContainerName {
		t.Fatalf("init container name = %q, want %q", ic.Name, injectInitContainerName)
	}
	if ic.Image != "ghcr.io/grafana/beyla/inject-sdk-image:0.0.13" {
		t.Fatalf("init container image = %q", ic.Image)
	}
	if len(ic.VolumeMounts) != 1 || ic.VolumeMounts[0].Name != injectVolumeName {
		t.Fatalf("init container volume mount = %+v", ic.VolumeMounts)
	}
	if ic.VolumeMounts[0].MountPath != internalMountPath {
		t.Fatalf("init container mount path = %q, want %q", ic.VolumeMounts[0].MountPath, internalMountPath)
	}
	if ic.VolumeMounts[0].ReadOnly {
		t.Fatalf("init container mount must be read-write so it can copy the payload")
	}
}

func TestAddCopyInitContainerIdempotent(t *testing.T) {
	pm := newModeMutator(config.InjectionModeInitContainer)
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}}

	pm.mountVolume(spec)
	pm.addCopyInitContainerIfNeeded(spec)
	// Re-running (e.g. re-instrumentation at a new version) must replace in
	// place, not duplicate.
	pm.mountVolume(spec)
	pm.addCopyInitContainerIfNeeded(spec)

	if len(spec.Volumes) != 1 {
		t.Fatalf("expected volume to be replaced in place, got %d", len(spec.Volumes))
	}
	if len(spec.InitContainers) != 1 {
		t.Fatalf("expected init container to be replaced in place, got %d", len(spec.InitContainers))
	}
}

func TestUsesInitContainerAutoUnresolvedIsSafe(t *testing.T) {
	// "auto" should be resolved at boot, but if it somehow reaches the mutator
	// unresolved we fall back to the older-cluster-safe init-container mode.
	pm := newModeMutator(config.InjectionModeAuto)
	if !pm.usesInitContainer() {
		t.Fatalf("unresolved auto mode should fall back to init-container mode")
	}
}
