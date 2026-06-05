package v1

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/distribution/reference"
	"github.com/grafana/beyla-k8s-injector/internal/config"
	"go.opentelemetry.io/obi/pkg/appolly/app/svc"
	attr "go.opentelemetry.io/obi/pkg/export/attributes/names"
	"go.opentelemetry.io/obi/pkg/kube/kubecache/informer"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var (
	runtimeScheme     = runtime.NewScheme()
	supportedSDKLangs = []svc.InstrumentableType{svc.InstrumentableDotnet, svc.InstrumentableJava, svc.InstrumentableNodejs, svc.InstrumentablePython}
)

const (
	ResourceAttributeAnnotationPrefix = "resource.opentelemetry.io/"
)

var (
	LabelAppName = []string{
		"app.kubernetes.io/instance",
		"app.kubernetes.io/name",
	}
	LabelAppVersion = []string{"app.kubernetes.io/version"}
)

const (
	injectVolumeName        = "otel-inject-instrumentation"
	injectInitContainerName = "otel-sdk-inject"
	internalMountPath       = "/__otel_sdk_auto_instrumentation__"

	envVarLdPreloadName               = "LD_PRELOAD"
	envVarLdPreloadValue              = internalMountPath + "/dist/injector/libotelinject.so"
	envOtelInjectorConfigFileName     = "OTEL_INJECTOR_CONFIG_FILE"
	envOtelInjectorConfigFileValue    = internalMountPath + "/dist/injector/otelinject.conf"
	envOtelSemConvStabilityName       = "OTEL_SEMCONV_STABILITY_OPT_IN"
	envInjectorOtelExtraResourceAttrs = "OTEL_INJECTOR_RESOURCE_ATTRIBUTES"
	envInjectorOtelServiceName        = "OTEL_INJECTOR_SERVICE_NAME"
	envInjectorOtelServiceVersion     = "OTEL_INJECTOR_SERVICE_VERSION"
	envInjectorOtelServiceNamespace   = "OTEL_INJECTOR_SERVICE_NAMESPACE"
	envInjectorOtelK8sNamespaceName   = "OTEL_INJECTOR_K8S_NAMESPACE_NAME"
	envInjectorOtelK8sPodName         = "OTEL_INJECTOR_K8S_POD_NAME"
	envInjectorOtelK8sPodUID          = "OTEL_INJECTOR_K8S_POD_UID"
	envInjectorOtelK8sContainerName   = "OTEL_INJECTOR_K8S_CONTAINER_NAME"
	envInjectorDebugName              = "OTEL_INJECTOR_LOG_LEVEL"
	envOtelK8sNodeName                = "OTEL_RESOURCE_ATTRIBUTES_NODE_NAME" // stored in OTEL_INJECTOR_RESOURCE_ATTRIBUTES, since there's no individual OTEL_INJECTOR_K8S_NODE_NAME
	envOtelK8sPodIP                   = "OTEL_RESOURCE_ATTRIBUTES_POD_IP"
	envVarSDKVersion                  = "BEYLA_INJECTOR_SDK_PKG_VERSION"

	// Enabling/disabling of language specific SDKs
	envDotnetEnabledName = "DOTNET_AUTO_INSTRUMENTATION_AGENT_PATH_PREFIX"
	envJavaEnabledName   = "JVM_AUTO_INSTRUMENTATION_AGENT_PATH"
	envNodejsEnabledName = "NODEJS_AUTO_INSTRUMENTATION_AGENT_PATH"
	envPythonEnabledName = "PYTHON_AUTO_INSTRUMENTATION_AGENT_PATH_PREFIX"
)

var logger = logf.Log.WithName("pod-mutator")

func init() {
	_ = corev1.AddToScheme(runtimeScheme)
	_ = admissionv1.AddToScheme(runtimeScheme)
}

// PreloadsSomethingElse reports whether any container or initContainer in the
// pod already has an LD_PRELOAD set to a value other than our injector path.
// We use this to skip both injection (would clobber the user's preload) and
// eviction (a pod we couldn't safely instrument shouldn't be restarted).
func PreloadsSomethingElse(pod *corev1.Pod) bool {
	for i := range pod.Spec.Containers {
		if isLDPreloadConflict(&pod.Spec.Containers[i]) {
			return true
		}
	}
	for i := range pod.Spec.InitContainers {
		if isLDPreloadConflict(&pod.Spec.InitContainers[i]) {
			return true
		}
	}
	return false
}

type PodMutator struct {
	Cfg config.SDKInject
}

func (pm *PodMutator) UpdateConfig(cfg *config.SDKInject) {
	pm.Cfg = *cfg
}

// This is the undesirable mode, since it uses a lot of ephemeral storage.
// Typically it's used on old k8s clusters, version < 1.31.
func (pm *PodMutator) usesInitContainer() bool {
	return pm.Cfg.InjectionMode != config.InjectionModeImage
}

func (pm *PodMutator) buildVolumeDefinition() corev1.Volume {
	if pm.usesInitContainer() {
		// Older clusters (k8s < 1.31) lack ImageVolumeSource. Provision an
		// ephemeral emptyDir that the copy init container populates from the
		// SDK image before the app containers start.
		sizeLimit := resource.MustParse(config.EphemeralVolumeSize)
		return corev1.Volume{
			Name: injectVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					SizeLimit: &sizeLimit,
				},
			},
		}
	}
	// Kubernetes ImageVolumeSource (k8s 1.31+).
	return corev1.Volume{
		Name: injectVolumeName,
		VolumeSource: corev1.VolumeSource{
			Image: &corev1.ImageVolumeSource{
				Reference:  pm.Cfg.ImageVolumePath(),
				PullPolicy: corev1.PullIfNotPresent,
			},
		},
	}
}

func (pm *PodMutator) mountVolume(spec *corev1.PodSpec) {
	if spec.Volumes == nil {
		spec.Volumes = make([]corev1.Volume, 0)
	}

	v := pm.buildVolumeDefinition()

	pos := slices.IndexFunc(spec.Volumes, func(c corev1.Volume) bool {
		return c.Name == injectVolumeName
	})

	if pos < 0 {
		spec.Volumes = append(spec.Volumes, v)
	} else {
		spec.Volumes[pos] = v
	}
}

// addCopyInitContainerIfNeeded adds (or replaces, by name) the init container that
// copies the SDK payload from the SDK image into the shared ephemeral volume.
func (pm *PodMutator) addCopyInitContainerIfNeeded(spec *corev1.PodSpec) {
	if !pm.usesInitContainer() {
		return
	}
	if spec.InitContainers == nil {
		spec.InitContainers = make([]corev1.Container, 0)
	}

	c := corev1.Container{
		Name:            injectInitContainerName,
		Image:           pm.Cfg.ImageVolumePath(),
		ImagePullPolicy: corev1.PullIfNotPresent,
		VolumeMounts: []corev1.VolumeMount{{
			Name:      injectVolumeName,
			MountPath: internalMountPath,
			ReadOnly:  false,
		}},
	}

	pos := slices.IndexFunc(spec.InitContainers, func(ic corev1.Container) bool {
		return ic.Name == injectInitContainerName
	})
	// Replace existing if already exists, rather than chaining it. This helps with
	// customers potentially using this directly in their setup.
	if pos < 0 {
		spec.InitContainers = append(spec.InitContainers, c)
	} else {
		spec.InitContainers[pos] = c
	}
}

func (pm *PodMutator) instrumentContainer(meta *metav1.ObjectMeta, c *corev1.Container, ruleEnv []corev1.EnvVar, spanMetricsSkip bool) {
	pm.addMount(c)
	pm.addEnvVars(meta, c, ruleEnv, spanMetricsSkip)
}

func (pm *PodMutator) addMount(c *corev1.Container) {
	if c.VolumeMounts == nil {
		c.VolumeMounts = make([]corev1.VolumeMount, 0)
	}
	idx := slices.IndexFunc(c.VolumeMounts, func(c corev1.VolumeMount) bool {
		return c.Name == injectVolumeName
	})

	volume := &corev1.VolumeMount{
		Name:      injectVolumeName,
		MountPath: internalMountPath,
		ReadOnly:  true,
	}
	if idx < 0 {
		c.VolumeMounts = append(c.VolumeMounts, *volume)
	} else {
		c.VolumeMounts[idx] = *volume
	}
}

// isLDPreloadConflict returns true only when LD_PRELOAD is set to a non-empty
// value that is not Beyla's own injector path. An empty LD_PRELOAD or one
// already set to our value is not a conflict.
// Unlike preloadsSomethingElse,
func isLDPreloadConflict(c *corev1.Container) bool {
	pos, ok := findEnvVar(c, envVarLdPreloadName)
	if !ok {
		return false
	}
	val := c.Env[pos].Value
	return val != "" && val != envVarLdPreloadValue
}

func findEnvVar(c *corev1.Container, name string) (int, bool) {
	pos := slices.IndexFunc(c.Env, func(c corev1.EnvVar) bool {
		return c.Name == name
	})

	return pos, pos >= 0
}

// setEnvVar is a helper function that sets an environment variable only if the value is not empty
func setEnvVarEvenIfEmpty(c *corev1.Container, envVarName, value string) {
	if pos, ok := findEnvVar(c, envVarName); !ok {
		c.Env = append(c.Env, corev1.EnvVar{
			Name:  envVarName,
			Value: value,
		})
	} else {
		c.Env[pos].ValueFrom = nil
		c.Env[pos].Value = value
	}
}

// applyEnvVars merges a list of env vars onto a container, overriding any
// existing entry with the same name. Rule env vars are Beyla-owned and
// intentionally take precedence over a value the pod author set themselves.
func applyEnvVars(c *corev1.Container, vars []corev1.EnvVar) {
	for _, v := range vars {
		if pos, ok := findEnvVar(c, v.Name); ok {
			c.Env[pos] = v
		} else {
			c.Env = append(c.Env, v)
		}
	}
}

// setEnvVar is a helper function that sets an environment variable only if the value is not empty
func setEnvVar(c *corev1.Container, envVarName, value string) {
	if value != "" {
		setEnvVarEvenIfEmpty(c, envVarName, value)
	}
}

func (pm *PodMutator) addEnvVars(meta *metav1.ObjectMeta, c *corev1.Container, ruleEnv []corev1.EnvVar, spanMetricsSkip bool) {
	if c.Env == nil {
		c.Env = []corev1.EnvVar{}
	}

	// Rule env vars are applied first (lowest precedence) so fixed injector
	// vars below can always override them.
	applyEnvVars(c, ruleEnv)
	// we set the SDK version on the environment variable so that
	// we can tell on start, when we scan the processes of the oldest
	// SDK version in use.
	setEnvVar(c, envVarSDKVersion, pm.Cfg.PackageVersion())
	setEnvVar(c, envVarLdPreloadName, envVarLdPreloadValue)
	setEnvVar(c, envOtelInjectorConfigFileName, envOtelInjectorConfigFileValue)
	setEnvVar(c, envOtelSemConvStabilityName, "http")
	if pm.Cfg.Debug {
		setEnvVar(c, envInjectorDebugName, "debug")
	}

	pm.configureContainerEnvVars(meta, c, spanMetricsSkip)
	pm.disableUndesiredSDKs(c)

	// TODO: how do we safely pass it from Beyla to here?
	// for k, v := range pm.exportHeaders {
	//	setEnvVar(c, k, v)
	// }

	logger.Info("env vars", "vars", c.Env)
}

// configureContainerEnvVars sets per-pod resource attributes and service identification
// env vars. Signal exporters, propagators, sampler, and debug are owned by Beyla and
// arrive via the rule's Config.Env (applied earlier in addEnvVars).
func (pm *PodMutator) configureContainerEnvVars(meta *metav1.ObjectMeta, container *corev1.Container, spanMetricsSkip bool) {
	extraResAttrs := pm.setResourceAttributes(meta, container)
	if spanMetricsSkip {
		extraResAttrs[attribute.Key(attr.SkipSpanMetrics)] = "true"
	}
	pm.injectEnvVars(extraResAttrs, container)
}

func (pm *PodMutator) injectEnvVars(extraResAttrs map[attribute.Key]string, container *corev1.Container) {
	if len(extraResAttrs) == 0 {
		return
	}
	resourceAttributeList := make([]string, 0, len(extraResAttrs))
	for _, resourceAttributeKey := range slices.Sorted(maps.Keys(extraResAttrs)) {
		resourceAttributeList = append(
			resourceAttributeList,
			fmt.Sprintf("%s=%s", resourceAttributeKey, extraResAttrs[resourceAttributeKey]))
	}
	perPodAttrs := strings.Join(resourceAttributeList, ",")
	// Append per-pod dynamic attributes to any static attributes already set by the rule env vars.
	if pos, ok := findEnvVar(container, envInjectorOtelExtraResourceAttrs); ok && container.Env[pos].Value != "" {
		container.Env[pos].Value = container.Env[pos].Value + "," + perPodAttrs
	} else {
		setEnvVar(container, envInjectorOtelExtraResourceAttrs, perPodAttrs)
	}
}

func (pm *PodMutator) setResourceAttributes(meta *metav1.ObjectMeta, container *corev1.Container) map[attribute.Key]string {
	cfg := pm.Cfg.Resources

	extraResAttrs := map[attribute.Key]string{}

	setEnvVar(container, envInjectorOtelK8sContainerName, container.Name)

	pm.addParentResourceLabels(meta, extraResAttrs, cfg.AddK8sUIDAttributes)

	namespace := setEnvVarFromFieldPath(container, envInjectorOtelK8sNamespaceName, "metadata.namespace")
	podName := setEnvVarFromFieldPath(container, envInjectorOtelK8sPodName, "metadata.name")
	// node name has to be added to extra attributes as there is no dedicated OTEL_INJECTOR_* variable
	extraResAttrs[semconv.K8SNodeNameKey] =
		setEnvVarFromFieldPath(container, envOtelK8sNodeName, "spec.nodeName")

	if cfg.AddK8sIPAttribute {
		extraResAttrs[semconv.K8SPodIPKey] =
			setEnvVarFromFieldPath(container, envOtelK8sPodIP, "status.podIP")
	}

	if cfg.AddK8sUIDAttributes {
		setEnvVarFromFieldPath(container, envInjectorOtelK8sPodUID, "metadata.uid")
	}

	// Set service attributes using dedicated env vars
	setEnvVar(container, envInjectorOtelServiceNamespace, chooseServiceNamespace(meta, cfg.UseLabelsForResourceAttributes, namespace))
	setEnvVar(container, envInjectorOtelServiceName, chooseServiceName(meta, cfg.UseLabelsForResourceAttributes, podName, extraResAttrs))
	setEnvVar(container, envInjectorOtelServiceVersion, chooseServiceVersion(meta, cfg.UseLabelsForResourceAttributes, container))

	// Service instance ID is added to extra attributes since it uses pod name reference
	serviceInstanceId := createServiceInstanceId(meta, namespace, podName, container.Name)
	if serviceInstanceId != "" {
		extraResAttrs[semconv.ServiceInstanceIDKey] = serviceInstanceId
	}

	// attributes from the pod annotations have the highest precedence
	for k, v := range meta.GetAnnotations() {
		if attr, ok := strings.CutPrefix(k, ResourceAttributeAnnotationPrefix); ok {
			extraResAttrs[attribute.Key(attr)] = v
		}
	}
	return extraResAttrs
}

// chooseServiceName returns the service name to be used in the instrumentation.
// See https://opentelemetry.io/docs/specs/semconv/non-normative/k8s-attributes/#how-servicename-should-be-calculated
func chooseServiceName(meta *metav1.ObjectMeta, useLabelsForResourceAttributes bool, podName string, resources map[attribute.Key]string) string {
	if name := chooseLabelOrAnnotation(meta, useLabelsForResourceAttributes, semconv.ServiceNameKey, LabelAppName); name != "" {
		return name
	}
	if name := resources[semconv.K8SDeploymentNameKey]; name != "" {
		return name
	}
	if name := resources[semconv.K8SReplicaSetNameKey]; name != "" {
		return name
	}
	if name := resources[semconv.K8SStatefulSetNameKey]; name != "" {
		return name
	}
	if name := resources[semconv.K8SDaemonSetNameKey]; name != "" {
		return name
	}
	if name := resources[semconv.K8SCronJobNameKey]; name != "" {
		return name
	}
	if name := resources[semconv.K8SJobNameKey]; name != "" {
		return name
	}
	return podName
}

// chooseLabelOrAnnotation returns the value of the label or annotation with the given key.
// The precedence is as follows:
// 1. annotation with key resource.opentelemetry.io/<resource>.
// 2. label with key labelKey.
func chooseLabelOrAnnotation(meta *metav1.ObjectMeta, useLabelsForResourceAttributes bool, res attribute.Key, labelKeys []string) string {
	if v := meta.GetAnnotations()[(ResourceAttributeAnnotationPrefix + string(res))]; v != "" {
		return v
	}
	if useLabelsForResourceAttributes {
		for _, labelKey := range labelKeys {
			if v := meta.GetLabels()[labelKey]; v != "" {
				return v
			}
		}
	}
	return ""
}

// chooseServiceVersion returns the service version to be used in the instrumentation.
// See https://opentelemetry.io/docs/specs/semconv/non-normative/k8s-attributes/#how-serviceversion-should-be-calculated
func chooseServiceVersion(meta *metav1.ObjectMeta, useLabelsForResourceAttributes bool, container *corev1.Container) string {
	v := chooseLabelOrAnnotation(meta, useLabelsForResourceAttributes, semconv.ServiceVersionKey, LabelAppVersion)
	if v != "" {
		return v
	}
	var err error
	v, err = parseServiceVersionFromImage(container.Image)
	if err != nil {
		return ""
	}
	return v
}

// chooseServiceNamespace returns the service.namespace to be used in the instrumentation.
// See https://opentelemetry.io/docs/specs/semconv/non-normative/k8s-attributes/#how-servicenamespace-should-be-calculated
func chooseServiceNamespace(meta *metav1.ObjectMeta, useLabelsForResourceAttributes bool, namespaceName string) string {
	namespace := chooseLabelOrAnnotation(meta, useLabelsForResourceAttributes, semconv.ServiceNamespaceKey, nil)
	if namespace != "" {
		return namespace
	}
	return namespaceName
}

var errCannotRetrieveImage = errors.New("cannot retrieve image name")

// parseServiceVersionFromImage parses the service version for differently-formatted image names
// according to https://opentelemetry.io/docs/specs/semconv/non-normative/k8s-attributes/#how-serviceversion-should-be-calculated
func parseServiceVersionFromImage(image string) (string, error) {
	ref, err := reference.Parse(image)
	if err != nil {
		return "", err
	}

	namedRef, ok := ref.(reference.Named)
	if !ok {
		return "", errCannotRetrieveImage
	}
	var tag, digest string
	if taggedRef, ok := namedRef.(reference.Tagged); ok {
		tag = taggedRef.Tag()
	}
	if digestedRef, ok := namedRef.(reference.Digested); ok {
		digest = digestedRef.Digest().String()
	}
	if digest != "" {
		if tag != "" {
			return fmt.Sprintf("%s@%s", tag, digest), nil
		}
		return digest, nil
	}
	if tag != "" {
		return tag, nil
	}

	return "", errCannotRetrieveImage
}

// chooseServiceInstanceId returns the service.instance.id to be used in the instrumentation.
// See https://opentelemetry.io/docs/specs/semconv/non-normative/k8s-attributes/#how-serviceinstanceid-should-be-calculated
func createServiceInstanceId(meta *metav1.ObjectMeta, namespaceName, podName, containerName string) string {
	// Do not use labels for service instance id,
	// because multiple containers in the same pod would get the same service instance id,
	// which violates the uniqueness requirement of service instance id -
	// see https://opentelemetry.io/docs/specs/semconv/resource/#service-experimental.
	// We still allow the user to set the service instance id via annotation, because this is explicitly set by the user.
	serviceInstanceId := chooseLabelOrAnnotation(meta, false, semconv.ServiceInstanceIDKey, nil)
	if serviceInstanceId != "" {
		return serviceInstanceId
	}

	if namespaceName != "" && podName != "" && containerName != "" {
		resNames := []string{namespaceName, podName, containerName}
		return strings.Join(resNames, ".")
	}
	return ""
}

// setEnvVarFromFieldPath is a helper function that sets an environment variable from a Kubernetes downwards API field path
func setEnvVarFromFieldPath(container *corev1.Container, envVarName, fieldPath string) string {
	container.Env = append(container.Env, corev1.EnvVar{
		Name: envVarName,
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				FieldPath: fieldPath,
			},
		},
	})
	return fmt.Sprintf("$(%s)", envVarName)
}

func (pm *PodMutator) addParentResourceLabels(meta *metav1.ObjectMeta, resources map[attribute.Key]string, includeUID bool) {
	for _, owner := range ownersFrom(meta) {
		resourceAttribute := getResourceAttribute(owner.Kind)
		if resourceAttribute != "" {
			resources[resourceAttribute] = owner.Name
		}
	}
	if includeUID {
		for _, owner := range meta.OwnerReferences {
			resourceAttribute := getResourceAttribute(owner.Kind)
			if resourceAttribute != "" {
				resources[resourceAttribute] = string(owner.UID)
			}
		}
	}
}

func getResourceAttribute(kind string) attribute.Key {
	switch strings.ToLower(kind) {
	case "replicaset":
		return semconv.K8SReplicaSetNameKey
	case "deployment":
		return semconv.K8SDeploymentNameKey
	case "statefulset":
		return semconv.K8SStatefulSetNameKey
	case "daemonset":
		return semconv.K8SDaemonSetNameKey
	case "job":
		return semconv.K8SJobNameKey
	case "cronjob":
		return semconv.K8SCronJobNameKey
	default:
		return ""
	}
}

// Setting an empty environment variable is picked up by the
// injector as disabled instrumentation for that language.
func (pm *PodMutator) disableUndesiredSDKs(c *corev1.Container) {
	for _, supported := range supportedSDKLangs {
		if !pm.CanInstrument(supported) {
			switch supported {
			case svc.InstrumentableDotnet:
				setEnvVarEvenIfEmpty(c, envDotnetEnabledName, "")
			case svc.InstrumentableJava:
				setEnvVarEvenIfEmpty(c, envJavaEnabledName, "")
			case svc.InstrumentableNodejs:
				setEnvVarEvenIfEmpty(c, envNodejsEnabledName, "")
			case svc.InstrumentablePython:
				setEnvVarEvenIfEmpty(c, envPythonEnabledName, "")
			}
		}
	}
}

func (pm *PodMutator) CanInstrument(kind svc.InstrumentableType) bool {
	for _, k := range pm.Cfg.EnabledSDKs {
		if k.InstrumentableType == kind {
			return true
		}
	}
	return false
}

// AlreadyInstrumented reports whether the pod is already instrumented at the
// requested SDK package version, in which case the caller should skip
// mutation. A pod instrumented at a *different* version returns false so the
// webhook re-instruments it on top of the older payload — the mutator's
// env-var and volume-mount writes are idempotent and update-in-place.
//
// We treat the in-pod state as the truth, checking both our own annotation
// (set on the last admission that mutated this pod) and the SDK-version env
// we stamp onto every instrumented container. The two checks mirror Beyla's
// own AlreadyInstrumented on the agent side, so the agent and the webhook
// agree on what "instrumented at version X" means.
func AlreadyInstrumented(spec *corev1.PodSpec, meta *metav1.ObjectMeta, wantVersion string) bool {
	if val, ok := meta.Annotations[InjectedAnnotation]; ok && val != "" {
		return val == wantVersion
	}
	for i := range spec.Containers {
		if v, ok := sdkVersionEnvValue(&spec.Containers[i]); ok && v != "" {
			return v == wantVersion
		}
	}
	for i := range spec.InitContainers {
		if v, ok := sdkVersionEnvValue(&spec.InitContainers[i]); ok && v != "" {
			return v == wantVersion
		}
	}
	return false
}

// IsInstrumented reports whether the pod currently carries our instrumentation
// at *any* SDK version. Unlike AlreadyInstrumented — which compares against a
// specific wanted version to decide whether to (re-)inject — this is the signal
// the controller uses to decide whether a pod that no longer matches any
// rule must be restarted to *remove* its instrumentation. We
// treat the in-pod state as the truth, checking both our annotation and the
// SDK-version env we stamp onto every instrumented container.
func IsInstrumented(spec *corev1.PodSpec, meta *metav1.ObjectMeta) bool {
	if val, ok := meta.Annotations[InjectedAnnotation]; ok && val != "" {
		return true
	}
	for i := range spec.Containers {
		if v, ok := sdkVersionEnvValue(&spec.Containers[i]); ok && v != "" {
			return true
		}
	}
	for i := range spec.InitContainers {
		if v, ok := sdkVersionEnvValue(&spec.InitContainers[i]); ok && v != "" {
			return true
		}
	}
	return false
}

// sdkVersionEnvValue returns the value of the SDK-version env var we stamp onto
// every instrumented container, if present. It's the per-container half of the
// in-pod instrumentation signal used by AlreadyInstrumented and IsInstrumented.
func sdkVersionEnvValue(c *corev1.Container) (string, bool) {
	pos, ok := findEnvVar(c, envVarSDKVersion)
	if !ok {
		return "", false
	}
	return c.Env[pos].Value, true
}

func ownersFrom(meta *metav1.ObjectMeta) []*informer.Owner {
	if len(meta.OwnerReferences) == 0 {
		// If no owner references' found, return itself as owner
		return []*informer.Owner{{Kind: "Pod", Name: meta.Name}}
	}
	owners := make([]*informer.Owner, 0, len(meta.OwnerReferences))
	for i := range meta.OwnerReferences {
		or := &meta.OwnerReferences[i]
		owners = append(owners, &informer.Owner{Kind: or.Kind, Name: or.Name})
		// ReplicaSets usually have a Deployment as owner too. Returning it as well
		if or.APIVersion == "apps/v1" && or.Kind == "ReplicaSet" {
			// we heuristically extract the Deployment name from the replicaset name
			if idx := strings.LastIndexByte(or.Name, '-'); idx > 0 {
				owners = append(owners, &informer.Owner{Kind: "Deployment", Name: or.Name[:idx]})
				// we already have what we need for decoration and selection. Ignoring any other owner
				// it might hypothetically have (it would be a rare case)
				return owners
			}
		}
		if or.APIVersion == "batch/v1" && or.Kind == "Job" {
			// we heuristically extract the CronJob name from the Job name
			if idx := strings.LastIndexByte(or.Name, '-'); idx > 0 {
				owners = append(owners, &informer.Owner{Kind: "CronJob", Name: or.Name[:idx]})
				// we already have what we need for decoration and selection. Ignoring any other owner
				// it might hypothetically have (it would be a rare case)
				return owners
			}
		}
	}
	return owners
}
