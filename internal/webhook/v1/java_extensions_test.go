package v1

import (
	"encoding/json"
	"reflect"
	"testing"

	bservices "github.com/grafana/beyla/v3/pkg/services"
	"go.opentelemetry.io/obi/pkg/appolly/app/svc"

	"github.com/grafana/beyla-k8s-injector/internal/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func extensionMutator(images ...string) *PodMutator {
	return &PodMutator{Cfg: config.SDKInject{InjectionMode: config.InjectionModeImage, JavaExtensionImages: images, EnabledSDKs: []bservices.InstrumentableType{{InstrumentableType: svc.InstrumentableJava}}}}
}
func TestReconcileJavaExtensionsMergesAndReconcilesOwnedState(t *testing.T) {
	pm := extensionMutator("example/ext:v1")
	meta := &metav1.ObjectMeta{}
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Env: []corev1.EnvVar{{Name: injectorJavaExtensionsEnv, Value: "/customer/a"}, {Name: javaAgentExtensionsEnv, Value: "/customer/agent.jar"}}, VolumeMounts: []corev1.VolumeMount{{Name: "otel-java-extension-foreign", MountPath: "/foreign"}}}}, Volumes: []corev1.Volume{{Name: "otel-java-extension-foreign"}}}
	if w := pm.reconcileJavaExtensions(meta, spec, []corev1.EnvVar{{Name: injectorJavaExtensionsEnv, Value: "/rule/a,/rule/b"}}); len(w) > 0 {
		t.Fatal(w)
	}
	c := &spec.Containers[0]
	if got := c.Env[javaExtensionEnvIndex(c.Env)].Value; got != "/customer/a,/rule/a,/rule/b,/__otel_java_extensions__/0/extensions" {
		t.Fatal(got)
	}
	if c.Env[1].Value != "/customer/agent.jar" {
		t.Fatal(c.Env)
	}
	if len(spec.Volumes) != 2 {
		t.Fatal(spec.Volumes)
	}
	before, _ := json.Marshal(spec)
	if w := pm.reconcileJavaExtensions(meta, spec, []corev1.EnvVar{{Name: injectorJavaExtensionsEnv, Value: "/rule/a,/rule/b"}}); len(w) > 0 {
		t.Fatal(w)
	}
	after, _ := json.Marshal(spec)
	if string(before) != string(after) {
		t.Fatalf("not idempotent\n%s\n%s", before, after)
	}
	pm.Cfg.JavaExtensionImages = nil
	if w := pm.reconcileJavaExtensions(meta, spec, []corev1.EnvVar{{Name: injectorJavaExtensionsEnv, Value: "/rule/a,/rule/b"}}); len(w) > 0 {
		t.Fatal(w)
	}
	if len(spec.Volumes) != 1 || spec.Volumes[0].Name != "otel-java-extension-foreign" {
		t.Fatal(spec.Volumes)
	}
	if spec.Containers[0].Env[0].Value != "/customer/a,/rule/a,/rule/b" {
		t.Fatal(spec.Containers[0].Env)
	}
}
func TestReconcileJavaExtensionsPreservesValueFromAndMalformedState(t *testing.T) {
	pm := extensionMutator("example/ext:v1")
	meta := &metav1.ObjectMeta{}
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Env: []corev1.EnvVar{{Name: injectorJavaExtensionsEnv, ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}}}}}
	if w := pm.reconcileJavaExtensions(meta, spec, nil); len(w) == 0 {
		t.Fatal("expected warning")
	}
	if len(spec.Volumes) != 0 || spec.Containers[0].Env[0].ValueFrom == nil {
		t.Fatal(spec)
	}
	meta.Annotations = map[string]string{javaExtensionOwnershipAnnotation: "{"}
	clean := &corev1.PodSpec{}
	if w := pm.reconcileJavaExtensions(meta, clean, nil); len(w) == 0 || len(clean.Volumes) != 0 {
		t.Fatal(w, clean)
	}
}

func TestReconcileJavaExtensionsHonorsRuleValueFromWithoutImages(t *testing.T) {
	pm := extensionMutator()
	meta := &metav1.ObjectMeta{}
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}
	source := corev1.EnvVar{Name: injectorJavaExtensionsEnv, ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}
	if warnings := pm.reconcileJavaExtensions(meta, spec, []corev1.EnvVar{source}); len(warnings) != 0 {
		t.Fatal(warnings)
	}
	if got := spec.Containers[0].Env; len(got) != 1 || got[0].ValueFrom == nil {
		t.Fatalf("rule ValueFrom not preserved: %#v", got)
	}
	if meta.Annotations != nil {
		t.Fatalf("no-op created annotations: %#v", meta.Annotations)
	}
}

func TestJavaExtensionVolumeHelper(t *testing.T) {
	spec := &corev1.PodSpec{}
	state := javaExtensionState{}
	if !reconcileJavaExtensionVolumes(spec, []string{"example/ext:v1"}, &state) {
		t.Fatal("expected volume creation")
	}
	if len(spec.Volumes) != 1 || len(state.Volumes) != 1 {
		t.Fatalf("unexpected volumes/state: %#v %#v", spec.Volumes, state)
	}
	if reconcileJavaExtensionVolumes(spec, []string{"example/ext:v2"}, &javaExtensionState{}) {
		t.Fatal("expected collision")
	}
}

func TestReconcileJavaExtensionsNoopPreservesLiteralInput(t *testing.T) {
	pm := extensionMutator()
	meta := &metav1.ObjectMeta{}
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Env: []corev1.EnvVar{{Name: injectorJavaExtensionsEnv, Value: "/customer/a, /customer/a"}}}}}
	before := spec.DeepCopy()
	if warnings := pm.reconcileJavaExtensions(meta, spec, nil); len(warnings) != 0 {
		t.Fatal(warnings)
	}
	if !reflect.DeepEqual(before, spec) || meta.Annotations != nil {
		t.Fatalf("no-op mutated pod/meta: %#v %#v", spec, meta)
	}
}

func TestReconcileJavaExtensionsUsesLastRuleInput(t *testing.T) {
	pm := extensionMutator()
	meta := &metav1.ObjectMeta{}
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}
	rules := []corev1.EnvVar{{Name: injectorJavaExtensionsEnv, Value: "/first"}, {Name: injectorJavaExtensionsEnv, Value: "/last"}}
	if warnings := pm.reconcileJavaExtensions(meta, spec, rules); len(warnings) != 0 {
		t.Fatal(warnings)
	}
	if got := spec.Containers[0].Env[0].Value; got != "/last" {
		t.Fatalf("got %q, want last rule value", got)
	}
}

func TestReconcileJavaExtensionsRuleValueFromPropagatesWithoutImages(t *testing.T) {
	pm := extensionMutator()
	meta := &metav1.ObjectMeta{}
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "one"}, {Name: "two", Env: []corev1.EnvVar{{Name: injectorJavaExtensionsEnv, Value: "/customer"}}}}}
	rule := corev1.EnvVar{Name: injectorJavaExtensionsEnv, ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}
	warnings := pm.reconcileJavaExtensions(meta, spec, []corev1.EnvVar{rule})
	if len(warnings) == 0 {
		t.Fatal("existing literal input should produce a warning")
	}
	if len(spec.Containers[0].Env) != 1 || spec.Containers[0].Env[0].ValueFrom == nil {
		t.Fatal("rule ValueFrom not propagated to empty container")
	}
	if got := spec.Containers[1].Env[0].Value; got != "/customer" {
		t.Fatalf("customer input replaced: %q", got)
	}
	if meta.Annotations != nil {
		t.Fatalf("unexpected ownership annotation: %#v", meta.Annotations)
	}
}

func TestReconcileJavaExtensionsRuleValueFromPreservesPriorOwnershipState(t *testing.T) {
	pm := extensionMutator("example/ext:v1")
	meta := &metav1.ObjectMeta{}
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}
	if warnings := pm.reconcileJavaExtensions(meta, spec, nil); len(warnings) > 0 {
		t.Fatal(warnings)
	}
	beforeSpec := spec.DeepCopy()
	beforeMeta := meta.DeepCopy()
	pm.Cfg.JavaExtensionImages = []string{"example/ext:v2"}
	rule := corev1.EnvVar{Name: injectorJavaExtensionsEnv, ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}
	if warnings := pm.reconcileJavaExtensions(meta, spec, []corev1.EnvVar{rule}); len(warnings) == 0 {
		t.Fatal("expected warning for prior ownership and image paths")
	}
	if len(spec.Volumes) != len(beforeSpec.Volumes) || spec.Volumes[0].Image.Reference != beforeSpec.Volumes[0].Image.Reference {
		t.Fatalf("owned volume changed: %#v", spec.Volumes)
	}
	if !reflect.DeepEqual(meta.Annotations, beforeMeta.Annotations) {
		t.Fatalf("ownership annotation changed: %#v", meta.Annotations)
	}
	if !reflect.DeepEqual(spec.Containers[0].Env, beforeSpec.Containers[0].Env) {
		t.Fatal("existing workload input was overwritten")
	}
}

func TestReconcileJavaExtensionsMalformedOwnershipDoesNotPropagateRuleValueFrom(t *testing.T) {
	pm := extensionMutator()
	meta := &metav1.ObjectMeta{Annotations: map[string]string{javaExtensionOwnershipAnnotation: "{"}}
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}
	beforeSpec := spec.DeepCopy()
	beforeMeta := meta.DeepCopy()
	rule := corev1.EnvVar{Name: injectorJavaExtensionsEnv, ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}
	if warnings := pm.reconcileJavaExtensions(meta, spec, []corev1.EnvVar{rule}); len(warnings) == 0 {
		t.Fatal("expected malformed ownership warning")
	}
	if !reflect.DeepEqual(beforeSpec, spec) || !reflect.DeepEqual(beforeMeta, meta) {
		t.Fatal("malformed ownership changed pod or metadata")
	}
}
