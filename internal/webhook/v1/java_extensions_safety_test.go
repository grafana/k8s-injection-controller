package v1

import (
	"reflect"
	"testing"

	"github.com/grafana/beyla-k8s-injector/internal/config"
	bservices "github.com/grafana/beyla/v3/pkg/services"
	"go.opentelemetry.io/obi/pkg/appolly/app/svc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	safetyCustomerJavaAgentExtensions = "/customer/agent.jar"
	safetyForeignExtensionVolumeName  = "otel-java-extension-foreign"
)

func safetyJavaExtensionMutator(images ...string) *PodMutator {
	return &PodMutator{Cfg: config.SDKInject{
		InjectionMode:       config.InjectionModeImage,
		JavaExtensionImages: images,
		EnabledSDKs: []bservices.InstrumentableType{{
			InstrumentableType: svc.InstrumentableJava,
		}},
	}}
}

func TestReconcileJavaExtensionsSafetyLeavesInitContainersUnchanged(t *testing.T) {
	pm := safetyJavaExtensionMutator("example.com/extensions:v1")
	meta := &metav1.ObjectMeta{}
	spec := &corev1.PodSpec{
		InitContainers: []corev1.Container{{
			Name:         "init",
			Image:        "busybox:latest",
			Env:          []corev1.EnvVar{{Name: "KEEP", Value: "init-value"}},
			VolumeMounts: []corev1.VolumeMount{{Name: "init-data", MountPath: "/init-data"}},
		}},
		Containers: []corev1.Container{{Name: "app"}},
		Volumes:    []corev1.Volume{{Name: "init-data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
	}
	initBefore := spec.InitContainers[0].DeepCopy()

	if warnings := pm.reconcileJavaExtensions(meta, spec, nil); len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if !reflect.DeepEqual(initBefore, spec.InitContainers[0].DeepCopy()) {
		t.Fatalf("init container changed: before=%#v after=%#v", initBefore, spec.InitContainers[0])
	}
	if len(spec.Volumes) != 2 || spec.Volumes[1].Image == nil {
		t.Fatalf("expected Java image volume on pod, got %#v", spec.Volumes)
	}
	app := spec.Containers[0]
	if len(app.VolumeMounts) != 1 || app.VolumeMounts[0].Name != javaExtensionVolumeName(0) {
		t.Fatalf("expected Java volume mount on regular container, got %#v", app.VolumeMounts)
	}
}

func TestReconcileJavaExtensionsSafetyPreservesForeignPrefixAndRejectsCollisionsAtomically(t *testing.T) {
	tests := []struct {
		name      string
		collision func(*corev1.PodSpec)
	}{
		{
			name: "desired volume name",
			collision: func(spec *corev1.PodSpec) {
				spec.Volumes = append(spec.Volumes, corev1.Volume{Name: javaExtensionVolumeName(0), VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
			},
		},
		{
			name: "desired mount path",
			collision: func(spec *corev1.PodSpec) {
				spec.Containers[0].VolumeMounts = append(spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "customer-volume", MountPath: javaExtensionsMountRoot + "/0"})
			},
		},
		{
			name: "desired mount name",
			collision: func(spec *corev1.PodSpec) {
				spec.Containers[0].VolumeMounts = append(spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: javaExtensionVolumeName(0), MountPath: "/customer/path"})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pm := safetyJavaExtensionMutator("example.com/extensions:v1")
			meta := &metav1.ObjectMeta{Annotations: map[string]string{"customer": "annotation"}}
			spec := &corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: "app",
					VolumeMounts: []corev1.VolumeMount{{
						Name:      "otel-java-extension-customer",
						MountPath: "/customer/extensions",
					}},
				}},
				Volumes: []corev1.Volume{{Name: "otel-java-extension-customer", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
			}
			tt.collision(spec)
			beforeSpec := spec.DeepCopy()
			beforeMeta := meta.DeepCopy()

			if warnings := pm.reconcileJavaExtensions(meta, spec, nil); len(warnings) == 0 {
				t.Fatal("expected collision warning")
			}
			if !reflect.DeepEqual(beforeSpec, spec) {
				t.Fatalf("collision partially mutated pod spec:\nbefore=%#v\nafter=%#v", beforeSpec, spec)
			}
			if !reflect.DeepEqual(beforeMeta, meta) {
				t.Fatalf("collision partially mutated metadata:\nbefore=%#v\nafter=%#v", beforeMeta, meta)
			}
		})
	}
}

func TestReconcileJavaExtensionsSafetyRejectsModifiedOwnedResourcesAtomically(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*corev1.PodSpec)
	}{
		{
			name: "owned volume image changed",
			mutate: func(spec *corev1.PodSpec) {
				spec.Volumes[0].Image.Reference = "example.com/customer-replacement:v1"
			},
		},
		{
			name: "owned mount path changed",
			mutate: func(spec *corev1.PodSpec) {
				spec.Containers[0].VolumeMounts[0].MountPath = "/customer/changed"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pm := safetyJavaExtensionMutator("example.com/extensions:v1")
			meta := &metav1.ObjectMeta{}
			spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}
			if warnings := pm.reconcileJavaExtensions(meta, spec, nil); len(warnings) != 0 {
				t.Fatalf("initial injection warnings: %v", warnings)
			}
			tt.mutate(spec)
			beforeSpec := spec.DeepCopy()
			beforeMeta := meta.DeepCopy()

			if warnings := pm.reconcileJavaExtensions(meta, spec, nil); len(warnings) == 0 {
				t.Fatal("expected warning for modified owned resource")
			}
			if !reflect.DeepEqual(beforeSpec, spec) {
				t.Fatalf("unsafe reconciliation changed pod spec:\nbefore=%#v\nafter=%#v", beforeSpec, spec)
			}
			if !reflect.DeepEqual(beforeMeta, meta) {
				t.Fatalf("unsafe reconciliation changed metadata:\nbefore=%#v\nafter=%#v", beforeMeta, meta)
			}
		})
	}
}

func TestReconcileJavaExtensionsSafetyPreservesOwnedVolumeWithCustomerReferences(t *testing.T) {
	for _, containerKind := range []string{"regular container", "init container"} {
		t.Run(containerKind, func(t *testing.T) {
			pm := safetyJavaExtensionMutator("example.com/extensions:v1")
			meta := &metav1.ObjectMeta{}
			spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}
			if warnings := pm.reconcileJavaExtensions(meta, spec, nil); len(warnings) != 0 {
				t.Fatalf("initial injection warnings: %v", warnings)
			}
			userMount := corev1.VolumeMount{Name: javaExtensionVolumeName(0), MountPath: "/customer/extension-reference"}
			if containerKind == "regular container" {
				spec.Containers[0].VolumeMounts = append(spec.Containers[0].VolumeMounts, userMount)
			} else {
				spec.InitContainers = []corev1.Container{{Name: "customer-init", VolumeMounts: []corev1.VolumeMount{userMount}}}
			}
			pm.Cfg.JavaExtensionImages = nil
			beforeSpec := spec.DeepCopy()
			beforeMeta := meta.DeepCopy()

			if warnings := pm.reconcileJavaExtensions(meta, spec, nil); len(warnings) == 0 {
				t.Fatal("expected warning when a customer mount references an owned volume")
			}
			if !reflect.DeepEqual(beforeSpec, spec) {
				t.Fatalf("disabling extensions orphaned or changed a customer reference:\nbefore=%#v\nafter=%#v", beforeSpec, spec)
			}
			if !reflect.DeepEqual(beforeMeta, meta) {
				t.Fatalf("warning changed ownership metadata:\nbefore=%#v\nafter=%#v", beforeMeta, meta)
			}
		})
	}
}

func TestReconcileJavaExtensionsSafetyMergesAndReconcilesOnlyOwnedPaths(t *testing.T) {
	pm := safetyJavaExtensionMutator("example.com/extensions:v1")
	meta := &metav1.ObjectMeta{}
	spec := &corev1.PodSpec{Containers: []corev1.Container{{
		Name: "app",
		Env: []corev1.EnvVar{
			{Name: injectorJavaExtensionsEnv, Value: "/customer/base, /shared"},
			{Name: javaAgentExtensionsEnv, Value: safetyCustomerJavaAgentExtensions},
		},
	}}}
	rule := []corev1.EnvVar{{Name: injectorJavaExtensionsEnv, Value: "/rule/old, /shared"}}
	if warnings := pm.reconcileJavaExtensions(meta, spec, rule); len(warnings) != 0 {
		t.Fatalf("initial reconciliation warnings: %v", warnings)
	}
	want := "/customer/base,/shared,/rule/old,/__otel_java_extensions__/0/extensions"
	if got := spec.Containers[0].Env[javaExtensionEnvIndex(spec.Containers[0].Env)].Value; got != want {
		t.Fatalf("merged extension paths = %q, want %q", got, want)
	}
	if got := spec.Containers[0].Env[1].Value; got != safetyCustomerJavaAgentExtensions {
		t.Fatalf("OTEL_JAVAAGENT_EXTENSIONS changed: %q", got)
	}

	firstSpec := spec.DeepCopy()
	firstMeta := meta.DeepCopy()
	if warnings := pm.reconcileJavaExtensions(meta, spec, rule); len(warnings) != 0 {
		t.Fatalf("idempotent reconciliation warnings: %v", warnings)
	}
	if !reflect.DeepEqual(firstSpec, spec) || !reflect.DeepEqual(firstMeta, meta) {
		t.Fatal("reconciling the same rule changed the pod or ownership metadata")
	}

	app := &spec.Containers[0]
	app.Env[javaExtensionEnvIndex(app.Env)].Value += ",/customer/added-later"
	pm.Cfg.JavaExtensionImages = nil
	newRule := []corev1.EnvVar{{Name: injectorJavaExtensionsEnv, Value: "/rule/new"}}
	if warnings := pm.reconcileJavaExtensions(meta, spec, newRule); len(warnings) != 0 {
		t.Fatalf("updated configuration warnings: %v", warnings)
	}
	app = &spec.Containers[0]
	if got, want := app.Env[javaExtensionEnvIndex(app.Env)].Value, "/customer/base,/shared,/customer/added-later,/rule/new"; got != want {
		t.Fatalf("paths after configuration update = %q, want %q", got, want)
	}
	if len(spec.Volumes) != 0 {
		t.Fatalf("disabled configuration left controller volumes: %#v", spec.Volumes)
	}
	if got := app.Env[1].Value; got != safetyCustomerJavaAgentExtensions {
		t.Fatalf("OTEL_JAVAAGENT_EXTENSIONS changed after update: %q", got)
	}

	if warnings := pm.reconcileJavaExtensions(meta, spec, nil); len(warnings) != 0 {
		t.Fatalf("removing rule configuration warnings: %v", warnings)
	}
	app = &spec.Containers[0]
	if got, want := app.Env[javaExtensionEnvIndex(app.Env)].Value, "/customer/base,/shared,/customer/added-later"; got != want {
		t.Fatalf("paths after removing rule input = %q, want %q", got, want)
	}
}

func TestReconcileJavaExtensionsSafetyPreservesValueFromInputs(t *testing.T) {
	t.Run("workload valueFrom wins over rule literal", func(t *testing.T) {
		pm := safetyJavaExtensionMutator("example.com/extensions:v1")
		meta := &metav1.ObjectMeta{}
		spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Env: []corev1.EnvVar{{
			Name:      injectorJavaExtensionsEnv,
			ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}},
		}}}}}
		beforeSpec := spec.DeepCopy()
		beforeMeta := meta.DeepCopy()
		rule := []corev1.EnvVar{{Name: injectorJavaExtensionsEnv, Value: "/rule/extension"}}
		if warnings := pm.reconcileJavaExtensions(meta, spec, rule); len(warnings) == 0 {
			t.Fatal("expected warning for workload valueFrom")
		}
		if !reflect.DeepEqual(beforeSpec, spec) || !reflect.DeepEqual(beforeMeta, meta) {
			t.Fatal("workload valueFrom conflict was not atomic")
		}
	})

	t.Run("rule valueFrom is retained when generated paths cannot be applied", func(t *testing.T) {
		pm := safetyJavaExtensionMutator("example.com/extensions:v1")
		meta := &metav1.ObjectMeta{}
		spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}
		ruleValue := corev1.EnvVar{Name: injectorJavaExtensionsEnv, ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}
		if warnings := pm.reconcileJavaExtensions(meta, spec, []corev1.EnvVar{ruleValue}); len(warnings) == 0 {
			t.Fatal("expected warning because generated paths cannot be combined with rule valueFrom")
		}
		if len(spec.Volumes) != 0 || len(spec.Containers[0].VolumeMounts) != 0 {
			t.Fatalf("generated image resources were applied despite valueFrom conflict: %#v", spec)
		}
		if got := javaExtensionEnvIndex(spec.Containers[0].Env); got < 0 || !reflect.DeepEqual(spec.Containers[0].Env[got], ruleValue) {
			t.Fatalf("rule valueFrom was not retained on pod: %#v", spec.Containers[0].Env)
		}
	})
}

func TestReconcileJavaExtensionsSafetyCleanupRemovesOnlyOwnedState(t *testing.T) {
	pm := safetyJavaExtensionMutator("example.com/extensions:v1")
	meta := &metav1.ObjectMeta{}
	spec := &corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: "controller-created"},
			{Name: "preexisting-empty", Env: []corev1.EnvVar{{Name: injectorJavaExtensionsEnv, Value: ""}}},
		},
		Volumes: []corev1.Volume{{Name: safetyForeignExtensionVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
	}
	for i := range spec.Containers {
		spec.Containers[i].VolumeMounts = append(spec.Containers[i].VolumeMounts, corev1.VolumeMount{Name: safetyForeignExtensionVolumeName, MountPath: "/foreign/extensions"})
	}
	if warnings := pm.reconcileJavaExtensions(meta, spec, nil); len(warnings) != 0 {
		t.Fatalf("initial reconciliation warnings: %v", warnings)
	}
	pm.Cfg.JavaExtensionImages = nil
	if warnings := pm.reconcileJavaExtensions(meta, spec, nil); len(warnings) != 0 {
		t.Fatalf("cleanup warnings: %v", warnings)
	}
	if len(spec.Volumes) != 1 || spec.Volumes[0].Name != safetyForeignExtensionVolumeName {
		t.Fatalf("cleanup changed foreign volumes: %#v", spec.Volumes)
	}
	if len(spec.Containers[0].VolumeMounts) != 1 || spec.Containers[0].VolumeMounts[0].Name != safetyForeignExtensionVolumeName {
		t.Fatalf("cleanup changed foreign mounts: %#v", spec.Containers[0].VolumeMounts)
	}
	if len(spec.Containers[0].Env) != 0 {
		t.Fatalf("controller-created empty env was not removed: %#v", spec.Containers[0].Env)
	}
	if len(spec.Containers[1].Env) != 1 || spec.Containers[1].Env[0].Name != injectorJavaExtensionsEnv || spec.Containers[1].Env[0].Value != "" {
		t.Fatalf("preexisting empty env was not preserved: %#v", spec.Containers[1].Env)
	}
}

func TestReconcileJavaExtensionsSafetyNoConfigurationDoesNotInitializeMetadata(t *testing.T) {
	pm := safetyJavaExtensionMutator()
	meta := &metav1.ObjectMeta{}
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}
	beforeSpec := spec.DeepCopy()
	beforeMeta := meta.DeepCopy()
	if warnings := pm.reconcileJavaExtensions(meta, spec, nil); len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if !reflect.DeepEqual(beforeSpec, spec) {
		t.Fatalf("empty configuration changed pod spec: before=%#v after=%#v", beforeSpec, spec)
	}
	if !reflect.DeepEqual(beforeMeta, meta) {
		t.Fatalf("empty configuration changed metadata: before=%#v after=%#v", beforeMeta, meta)
	}
}

func TestReconcileJavaExtensionsSafetyMalformedOwnershipIsAtomic(t *testing.T) {
	pm := safetyJavaExtensionMutator("example.com/extensions:v1")
	meta := &metav1.ObjectMeta{Annotations: map[string]string{
		"customer":                       "preserve",
		javaExtensionOwnershipAnnotation: "{malformed",
	}}
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}
	beforeSpec := spec.DeepCopy()
	beforeMeta := meta.DeepCopy()
	if warnings := pm.reconcileJavaExtensions(meta, spec, nil); len(warnings) == 0 {
		t.Fatal("expected malformed ownership warning")
	}
	if !reflect.DeepEqual(beforeSpec, spec) {
		t.Fatalf("malformed ownership changed pod spec: before=%#v after=%#v", beforeSpec, spec)
	}
	if !reflect.DeepEqual(beforeMeta, meta) {
		t.Fatalf("malformed ownership changed metadata: before=%#v after=%#v", beforeMeta, meta)
	}
}
