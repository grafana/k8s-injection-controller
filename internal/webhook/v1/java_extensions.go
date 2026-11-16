package v1

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/grafana/beyla-k8s-injector/internal/config"
	"go.opentelemetry.io/obi/pkg/appolly/app/svc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const javaExtensionOwnershipAnnotation = "beyla.grafana.com/java-extension-ownership"

type javaExtensionState struct {
	Volumes []corev1.Volume      `json:"volumes,omitempty"`
	Mounts  []javaExtensionMount `json:"mounts,omitempty"`
	Env     []javaExtensionEnv   `json:"env,omitempty"`
}
type javaExtensionMount struct {
	Container string             `json:"container"`
	Mount     corev1.VolumeMount `json:"mount"`
}
type javaExtensionEnv struct {
	Container string   `json:"container"`
	Paths     []string `json:"paths"`
	Created   bool     `json:"created,omitempty"`
}

// reconcileJavaExtensions reconciles Java image volumes without taking ownership of
// pre-existing pod resources. Warnings are returned for admission to surface.
func (pm *PodMutator) reconcileJavaExtensions(meta *metav1.ObjectMeta, spec *corev1.PodSpec, ruleEnv []corev1.EnvVar) []string {
	warning := func(reason string) []string {
		return []string{"Java extension configuration could not be applied safely; existing settings were preserved: " + reason}
	}
	images := []string(nil)
	if pm.Cfg.InjectionMode == config.InjectionModeImage && pm.CanInstrument(svc.InstrumentableJava) {
		images = pm.javaExtensionImages()
	}
	ruleInput := javaExtensionRuleInput(ruleEnv)
	if len(images) == 0 && meta.Annotations[javaExtensionOwnershipAnnotation] == "" && ruleInput == nil {
		return nil
	}
	var old javaExtensionState
	if raw := meta.Annotations[javaExtensionOwnershipAnnotation]; raw != "" {
		if json.Unmarshal([]byte(raw), &old) != nil {
			return warning("ownership annotation is malformed")
		}
	}
	if ruleInput != nil && ruleInput.ValueFrom != nil {
		conflict := meta.Annotations[javaExtensionOwnershipAnnotation] != ""
		for i := range spec.Containers {
			envIndex := javaExtensionEnvIndex(spec.Containers[i].Env)
			if envIndex >= 0 {
				conflict = true
				continue
			}
			spec.Containers[i].Env = append(spec.Containers[i].Env, *ruleInput.DeepCopy())
		}
		if len(images) > 0 || conflict {
			return warning("rule injector ValueFrom cannot be merged with image paths, existing input, or prior managed state")
		}
		return nil
	}
	work := spec.DeepCopy()
	for _, c := range work.Containers {
		n := 0
		for _, e := range c.Env {
			if e.Name == injectorJavaExtensionsEnv {
				n++
			}
		}
		if n > 1 {
			return warning(fmt.Sprintf("container %q has duplicate injector input variables", c.Name))
		}
	}
	if !cleanupJavaExtensionState(work, old) {
		return warning("previously managed volume or mount was modified or gained an unmanaged reference")
	}
	state := javaExtensionState{}
	if !reconcileJavaExtensionVolumes(work, images, &state) {
		return warning("extension volume name is already in use")
	}
	for i := range work.Containers {
		c := &work.Containers[i]
		if !reconcileJavaExtensionContainer(c, images, ruleEnv, &state) {
			return warning(fmt.Sprintf("container %q has an injector source or extension mount that cannot be safely reconciled", c.Name))
		}
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return warning("could not encode ownership state")
	}
	if len(state.Volumes)+len(state.Mounts)+len(state.Env) > 0 && meta.Annotations == nil {
		meta.Annotations = map[string]string{}
	}
	if len(state.Volumes)+len(state.Mounts)+len(state.Env) == 0 {
		delete(meta.Annotations, javaExtensionOwnershipAnnotation)
	} else {
		meta.Annotations[javaExtensionOwnershipAnnotation] = string(raw)
	}
	*spec = *work
	return nil
}

func cleanupJavaExtensionState(spec *corev1.PodSpec, state javaExtensionState) bool {
	if !previousJavaExtensionStateIsIntact(spec, state) {
		return false
	}
	for _, volume := range state.Volumes {
		for i, existing := range spec.Volumes {
			if existing.Name == volume.Name && reflect.DeepEqual(existing, volume) {
				spec.Volumes = slices.Delete(spec.Volumes, i, i+1)
				break
			}
		}
	}
	for _, mount := range state.Mounts {
		container := javaExtensionContainer(spec, mount.Container)
		for i, existing := range container.VolumeMounts {
			if reflect.DeepEqual(existing, mount.Mount) {
				container.VolumeMounts = slices.Delete(container.VolumeMounts, i, i+1)
				break
			}
		}
	}
	for _, owned := range state.Env {
		container := javaExtensionContainer(spec, owned.Container)
		if container == nil {
			continue
		}
		index := javaExtensionEnvIndex(container.Env)
		if index < 0 || container.Env[index].ValueFrom != nil {
			return false
		}
		paths := javaExtensionPaths(container.Env[index].Value)
		for _, path := range owned.Paths {
			paths = javaExtensionDeletePath(paths, path)
		}
		if owned.Created && len(paths) == 0 {
			container.Env = slices.Delete(container.Env, index, index+1)
		} else {
			container.Env[index].Value = strings.Join(paths, ",")
		}
	}
	return true
}

func reconcileJavaExtensionVolumes(spec *corev1.PodSpec, images []string, state *javaExtensionState) bool {
	for i, image := range images {
		name := javaExtensionVolumeName(i)
		if slices.ContainsFunc(spec.Volumes, func(volume corev1.Volume) bool { return volume.Name == name }) {
			return false
		}
		volume := javaExtensionImageVolume(name, image)
		spec.Volumes = append(spec.Volumes, volume)
		state.Volumes = append(state.Volumes, volume)
	}
	return true
}

func reconcileJavaExtensionContainer(container *corev1.Container, images []string, ruleEnv []corev1.EnvVar, state *javaExtensionState) bool {
	for i := range images {
		name := javaExtensionVolumeName(i)
		path := javaExtensionsMountRoot + "/" + strconv.Itoa(i)
		if slices.ContainsFunc(container.VolumeMounts, func(mount corev1.VolumeMount) bool { return mount.Name == name || mount.MountPath == path }) {
			return false
		}
		mount := corev1.VolumeMount{Name: name, MountPath: path, ReadOnly: true}
		container.VolumeMounts = append(container.VolumeMounts, mount)
		state.Mounts = append(state.Mounts, javaExtensionMount{container.Name, mount})
	}
	index := javaExtensionEnvIndex(container.Env)
	rule := javaExtensionRuleInput(ruleEnv)
	if index >= 0 && container.Env[index].ValueFrom != nil {
		return rule == nil && len(images) == 0
	}
	if index >= 0 && rule != nil && rule.ValueFrom != nil {
		return false
	}
	if index < 0 && rule != nil && rule.ValueFrom != nil {
		container.Env = append(container.Env, *rule.DeepCopy())
		return true
	}
	base := ""
	if index >= 0 {
		base = container.Env[index].Value
	}
	ruleValue := ""
	if rule != nil {
		ruleValue = rule.Value
	}
	generated := []string{}
	for i := range images {
		generated = append(generated, javaExtensionsMountRoot+"/"+strconv.Itoa(i)+"/extensions")
	}
	merged := javaExtensionPaths(base, ruleValue, strings.Join(generated, ","))
	if len(merged) == 0 {
		return true
	}
	if index < 0 {
		container.Env = append(container.Env, corev1.EnvVar{Name: injectorJavaExtensionsEnv, Value: strings.Join(merged, ",")})
	} else {
		container.Env[index].Value = strings.Join(merged, ",")
	}
	added := javaExtensionAddedPaths(merged, javaExtensionPaths(base))
	if len(added) > 0 {
		state.Env = append(state.Env, javaExtensionEnv{container.Name, added, index < 0})
	}
	return true
}

func javaExtensionRuleInput(env []corev1.EnvVar) *corev1.EnvVar {
	for i := len(env) - 1; i >= 0; i-- {
		if env[i].Name == injectorJavaExtensionsEnv {
			return &env[i]
		}
	}
	return nil
}
func javaExtensionContainer(s *corev1.PodSpec, n string) *corev1.Container {
	for i := range s.Containers {
		if s.Containers[i].Name == n {
			return &s.Containers[i]
		}
	}
	return nil
}

func javaExtensionImageVolume(name, image string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{Image: &corev1.ImageVolumeSource{Reference: image, PullPolicy: corev1.PullIfNotPresent}}}
}

func previousJavaExtensionStateIsIntact(spec *corev1.PodSpec, state javaExtensionState) bool {
	for _, owned := range state.Volumes {
		found := false
		for _, volume := range spec.Volumes {
			if volume.Name == owned.Name && reflect.DeepEqual(volume, owned) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for _, owned := range state.Mounts {
		container := javaExtensionContainer(spec, owned.Container)
		if container == nil || !slices.ContainsFunc(container.VolumeMounts, func(mount corev1.VolumeMount) bool { return reflect.DeepEqual(mount, owned.Mount) }) {
			return false
		}
	}
	for _, volume := range state.Volumes {
		for i := range spec.Containers {
			for _, mount := range spec.Containers[i].VolumeMounts {
				if mount.Name == volume.Name && !ownedJavaExtensionMount(state, spec.Containers[i].Name, mount) {
					return false
				}
			}
		}
		for i := range spec.InitContainers {
			for _, mount := range spec.InitContainers[i].VolumeMounts {
				if mount.Name == volume.Name && !ownedJavaExtensionMount(state, spec.InitContainers[i].Name, mount) {
					return false
				}
			}
		}
	}
	return true
}

func ownedJavaExtensionMount(state javaExtensionState, container string, mount corev1.VolumeMount) bool {
	for _, owned := range state.Mounts {
		if owned.Container == container && reflect.DeepEqual(owned.Mount, mount) {
			return true
		}
	}
	return false
}
func javaExtensionEnvIndex(e []corev1.EnvVar) int {
	return slices.IndexFunc(e, func(x corev1.EnvVar) bool { return x.Name == injectorJavaExtensionsEnv })
}
func javaExtensionPaths(values ...string) []string {
	out := []string{}
	for _, v := range values {
		for p := range strings.SplitSeq(v, ",") {
			p = strings.TrimSpace(p)
			if p != "" && !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out
}
func javaExtensionDeletePath(s []string, p string) []string {
	for i, x := range s {
		if x == p {
			return slices.Delete(s, i, i+1)
		}
	}
	return s
}
func javaExtensionAddedPaths(a, b []string) []string {
	out := []string{}
	for _, x := range a {
		if !slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}
