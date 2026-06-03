/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"go.opentelemetry.io/obi/pkg/appolly/services"
	"gopkg.in/yaml.v3"

	"github.com/grafana/beyla/v3/pkg/webhook/configmap"

	"github.com/grafana/beyla-k8s-injector/internal/config"
	"github.com/grafana/beyla-k8s-injector/internal/metrics"
	"github.com/grafana/beyla-k8s-injector/internal/podinfo"
	"github.com/grafana/beyla-k8s-injector/internal/registry"
	webhookv1 "github.com/grafana/beyla-k8s-injector/internal/webhook/v1"
)

// restartCriterion is one entry in eligible_for_restart.yaml.
type restartCriterion struct {
	Namespace string `json:"namespace,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name,omitempty"`
	Language  string `json:"language,omitempty"` // currently unused
}

const (
	kindDeployment  = "Deployment"
	kindDaemonSet   = "DaemonSet"
	kindReplicaSet  = "ReplicaSet"
	kindStatefulSet = "StatefulSet"
)

// restartKinds is the set of workload kinds we know how to match against a
// pod's owner chain.
var restartKinds = map[string]struct{}{
	kindDeployment:  {},
	kindReplicaSet:  {},
	kindStatefulSet: {},
	kindDaemonSet:   {},
}

// workloadKey uniquely identifies a rollout-capable workload within a reconcile
// pass. Used as a map key to deduplicate: if ten pods in the same Deployment
// all need re-injection, we patch the Deployment exactly once.
type workloadKey struct {
	Namespace string
	Kind      string
	Name      string
}

// resolveWorkload derives the workload to patch for the graceful rollout from a
// pod's resolved owner info. Returns the zero value if no rollout-capable
// workload can be determined (e.g. a standalone ReplicaSet with no Deployment
// parent - those cannot be gracefully rolled).
func resolveWorkload(info podinfoMatcher) workloadKey {
	if info.DeploymentName != "" {
		return workloadKey{Namespace: info.Namespace, Kind: kindDeployment, Name: info.DeploymentName}
	}
	switch info.OwnerKind {
	case kindStatefulSet, kindDaemonSet:
		return workloadKey{Namespace: info.Namespace, Kind: info.OwnerKind, Name: info.OwnerName}
	}
	return workloadKey{}
}

// protectedNamespaces is a hardcoded denylist of namespaces the eviction
// sweep must never touch. A wide or misconfigured selector (e.g.
// k8s_namespace: "*") must not cause us to restart pods here.
var protectedNamespaces = map[string]bool{
	// Kubernetes built-in system namespaces.
	"kube-system":     true,
	"kube-public":     true,
	"kube-node-lease": true,

	// Common infrastructure namespaces.
	"cert-manager":       true,
	"monitoring":         true,
	"local-path-storage": true,
	"grafana-alloy":      true,

	// GKE-managed namespaces.
	"gke-connect":                 true,
	"gke-gmp-system":              true,
	"gke-managed-cim":             true,
	"gke-managed-filestorecsi":    true,
	"gke-managed-metrics-server":  true,
	"gke-managed-system":          true,
	"gke-system":                  true,
	"gke-managed-volumepopulator": true,

	// AKS-managed namespaces.
	"gatekeeper-system": true,

	// The controller's own namespace — deadlock prevention.
	"beyla-k8s-injector": true,
}

// ConfigMapReconciler watches ConfigMaps carrying the SelectorAnnotation and
// keeps the in-memory Registry in sync with their k8s_namespace selections.
type ConfigMapReconciler struct {
	client.Client
	// Clientset is used for the Eviction subresource, which controller-runtime's
	// typed client does not expose.
	Clientset kubernetes.Interface
	Registry  *registry.Registry
	// WebhookReady gates the eviction sweep on the local listener being bound.
	// Necessary but not sufficient — see WebhookServiceAddr below. Optional.
	WebhookReady healthz.Checker
	// WebhookServiceAddr is "<service>.<ns>.svc:443" for our own webhook
	// Service. We TCP-dial it before each eviction sweep: a successful dial
	// means kube-proxy has programmed the Service VIP and the apiserver can
	// reach us. Without this, the first sweep on startup races kube-proxy:
	// our listener is up locally (WebhookReady is green) but the Service has
	// no ready endpoints yet, so apiserver admissions get refused and (with
	// failurePolicy=Ignore) admit pods un-instrumented. Optional; if empty,
	// the dial check is skipped.
	WebhookServiceAddr string
	// DefaultSDKConfig is the controller-wide injection default; per-ConfigMap
	// overrides on the matched Instrumentation are layered on top of it via
	// WithConfigMapOverrides before computing the package version used for
	// the version-skew check. Zero value means "no SDK config wired" — in
	// that mode the webhook is a no-op and we skip evictions to avoid churn.
	DefaultSDKConfig config.SDKInject
	// Metrics records triggered rollouts. Optional; nil is a no-op.
	Metrics *metrics.SDKInjectionMetrics
}

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=patch

func (r *ConfigMapReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	cmKey := req.String()

	logger.Info("processing config map", "cmKey", cmKey)

	var cm corev1.ConfigMap
	if err := r.Get(ctx, req.NamespacedName, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			r.Registry.Delete(cmKey)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Predicate already filters by annotation, but defend against races where
	// the annotation was removed.
	if _, ok := cm.Annotations[configmap.SelectorAnnotation]; !ok {
		r.Registry.Delete(cmKey)
		return ctrl.Result{}, nil
	}

	inst, restartTargets, err := parseConfigMap(cm.Data)
	if err != nil {
		logger.Error(err, "ignoring ConfigMap with invalid payload")
		r.Registry.Delete(cmKey)
		return ctrl.Result{}, nil
	}

	r.Registry.Set(cmKey, inst)
	if len(restartTargets) == 0 {
		return ctrl.Result{}, nil
	}
	if r.WebhookReady != nil {
		if err := r.WebhookReady(nil); err != nil {
			logger.Info("webhook server not yet ready; deferring eviction sweep", "reason", err.Error())
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
	}
	if r.WebhookServiceAddr != "" {
		if err := dialWebhookService(r.WebhookServiceAddr); err != nil {
			logger.Info("webhook Service not yet routable; deferring eviction sweep",
				"addr", r.WebhookServiceAddr, "reason", err.Error())
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
	}
	if err := r.rolloutMatching(ctx, restartTargets); err != nil {
		logger.Error(err, "failed to trigger rollouts for pre-existing pods")
	}

	// We don't need to resweep, Beyla will notice we restarted and update the config map
	// for us so we can see a new pod that launched while we were away.
	return ctrl.Result{}, nil
}

// parseConfigMap extracts the injection record (from instrumentation.yaml)
// and the eligible-for-restart targets (from eligible_for_restart.yaml).
// Either key may be absent. Restart entries missing the required namespace
// or kind, or naming an unknown kind, are dropped with no error.
//
// The wire schema (configmap.InjectConfig) carries Discovery as obi's
// GlobDefinitionCriteria — a superset of fields Beyla uses internally. We
// translate it down to the injector's typed SelectionCriterion here so the
// matcher hot path stays cheap and ignores fields it can't enforce at
// admission time (open_ports, exe_path, …).
func parseConfigMap(data map[string]string) (registry.Instrumentation, []restartCriterion, error) {
	var inst registry.Instrumentation
	if raw, ok := data[configmap.KeyInstrumentation]; ok {
		var cfg configmap.InjectConfig
		if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
			return registry.Instrumentation{}, nil, fmt.Errorf("parse %s: %w", configmap.KeyInstrumentation, err)
		}
		inst.InjectConfig = cfg
		for _, ga := range cfg.Discovery {
			crit := selectionCriterionFromGlob(&ga)
			if crit.IsEmpty() {
				// Either the entry only carried obi-specific fields we don't
				// honor, or it was empty to begin with. Either way: skip,
				// since an empty criterion would match every pod.
				continue
			}
			inst.Criteria = append(inst.Criteria, crit)
		}
	}
	var restartTargets []restartCriterion
	if raw, ok := data[configmap.KeyEligibleForRestart]; ok {
		var parsed []*configmap.EligibleDeployment
		if err := yaml.Unmarshal([]byte(raw), &parsed); err != nil {
			return registry.Instrumentation{}, nil, fmt.Errorf("parse %s: %w", configmap.KeyEligibleForRestart, err)
		}
		sortEligible(parsed)
		for _, c := range parsed {
			if c == nil || c.Namespace == "" || c.Kind == "" {
				continue
			}
			if _, ok := restartKinds[c.Kind]; !ok {
				continue
			}
			restartTargets = append(restartTargets, restartCriterion{
				Namespace: c.Namespace,
				Kind:      c.Kind,
				Name:      c.Name,
				Language:  c.Language,
			})
		}
	}
	return inst, restartTargets, nil
}

// selectionCriterionFromGlob projects one obi GlobAttributes entry onto the
// injector's match schema. We read the well-known k8s_* metadata keys
// (carried via the inline Metadata map on the obi side) plus the
// k8s_pod_labels / k8s_pod_annotations maps (carried in dedicated fields, not
// the inline map), and ignore everything else — obi's open_ports, exe_path,
// etc. are runtime gates Beyla applies on the agent side, not admission-time
// gates we can apply to a Pod spec. Pod labels and annotations, by contrast,
// ARE on the admission Pod object, so we enforce them.
func selectionCriterionFromGlob(ga *configmap.WebhookKubeOnlySelector) registry.SelectionCriterion {
	get := func(key string) *services.GlobAttr {
		g, ok := ga.Metadata[key]
		if !ok || g == nil || !g.IsSet() {
			return nil
		}
		return g
	}
	return registry.SelectionCriterion{
		K8sPodName:         get("k8s_pod_name"),
		K8sNamespace:       get("k8s_namespace"),
		K8sDeploymentName:  get("k8s_deployment_name"),
		K8sReplicaSetName:  get("k8s_replicaset_name"),
		K8sStatefulSetName: get("k8s_statefulset_name"),
		K8sDaemonSetName:   get("k8s_daemonset_name"),
		K8sOwnerName:       get("k8s_owner_name"),
		K8sPodLabels:       globMap(ga.PodLabels),
		K8sPodAnnotations:  globMap(ga.PodAnnotations),
	}
}

// globMap copies the non-empty glob entries out of an obi pod-label /
// pod-annotation map. Returns nil when nothing is set so an unconfigured clause
// leaves the criterion field empty (and IsEmpty stays accurate).
func globMap(in map[string]*services.GlobAttr) map[string]*services.GlobAttr {
	var out map[string]*services.GlobAttr
	for k, g := range in {
		if g == nil || !g.IsSet() {
			continue
		}
		if out == nil {
			out = make(map[string]*services.GlobAttr, len(in))
		}
		out[k] = g
	}
	return out
}

// sortEligible orders the deserialized list by (Namespace, Name) so the
// downstream loop is deterministic across reconciles. Beyla writes this slice
// in random map-iteration order; canonicalizing here keeps any future
// "did anything actually change?" comparison straightforward.
func sortEligible(eligible []*configmap.EligibleDeployment) {
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].Namespace != eligible[j].Namespace {
			return eligible[i].Namespace < eligible[j].Namespace
		}
		return eligible[i].Name < eligible[j].Name
	})
}

// rolloutMatching processes the restart targets from a single ConfigMap. For
// each target it lists pods in target.Namespace whose owner chain matches
// target.Kind (and optionally target.Name) and decides, per pod, whether the
// owning workload needs a rollout:
//
//   - Instrument: the pod matches a selection criterion but is not yet
//     instrumented at the current SDK version — restart so the webhook injects
//     on recreation. Pods the webhook would not mutate (no SDK config, foreign
//     LD_PRELOAD) are skipped.
//   - Uninstrument: the pod matches no criterion but is currently instrumented
//     — restart so the webhook re-admits it, finds no match, and leaves it
//     clean, removing the instrumentation.
//
// A pod that neither matches nor is instrumented needs no action. Bare pods are
// skipped (no controller to recreate them).
func (r *ConfigMapReconciler) rolloutMatching(ctx context.Context, targets []restartCriterion) error {
	logger := log.FromContext(ctx)

	// One LIST per distinct namespace, regardless of how many entries name it.
	byNamespace := map[string][]restartCriterion{}
	for _, t := range targets {
		byNamespace[t.Namespace] = append(byNamespace[t.Namespace], t)
	}

	toRestart := map[workloadKey]struct{}{}

	for namespace, nsTargets := range byNamespace {
		if protectedNamespaces[namespace] {
			logger.Info("skipping protected namespace", "namespace", namespace)
			continue
		}

		var pods corev1.PodList
		if err := r.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
			return fmt.Errorf("list pods in %s: %w", namespace, err)
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.DeletionTimestamp != nil {
				continue
			}
			if len(pod.OwnerReferences) == 0 {
				continue
			}
			info := podinfo.Resolve(ctx, r.Client, pod)
			if !matchesAnyTarget(info, nsTargets) {
				continue
			}
			inst, ok := r.Registry.Match(info)
			if !ok {
				// Pod no longer matches any selection criterion. If it is
				// currently instrumented, restart its workload so the webhook
				// re-admits it, finds no match, and drops the instrumentation.
				// An un-instrumented non-matching pod needs no action.
				if !webhookv1.IsInstrumented(&pod.Spec, &pod.ObjectMeta) {
					continue
				}
				logger.Info("pod no longer matches selection criteria; scheduling rollout to remove instrumentation",
					"namespace", pod.Namespace, "pod", pod.Name)
			} else {
				// Pod matches: this is the (re-)instrumentation path.
				effective := r.DefaultSDKConfig.WithConfigMapOverrides(inst.InjectConfig)
				if effective.ImageVersion == "" {
					// No SDK config in the default or in the CM override: the
					// webhook would not mutate, so evicting accomplishes nothing.
					continue
				}
				if webhookv1.AlreadyInstrumented(&pod.Spec, &pod.ObjectMeta, effective.PackageVersion()) {
					continue
				}
				if webhookv1.PreloadsSomethingElse(pod) {
					continue
				}
			}
			key := resolveWorkload(info)
			if key == (workloadKey{}) {
				logger.Info("skipping pod: owner is not a rollout capable workload", "namespace", pod.Namespace, "pod", pod.Name, "ownerKind", info.OwnerKind, "ownerName", info.OwnerName)
				continue
			}
			toRestart[key] = struct{}{}

		}
	}

	// RFC3339Nano (not RFC3339) so two rollouts of the same workload within the
	// same wall-clock second produce distinct marker values. A second-precision
	// marker would make the second merge-patch a no-op (identical annotation
	// value => unchanged pod-template hash => no rollout), so an instrument roll
	// immediately followed by an uninstrument roll would silently fail to remove
	// instrumentation.
	restartTime := time.Now().Format(time.RFC3339Nano)
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"beyla.grafana.com/restartedAt":%q}}}}}`, restartTime)
	var errs []error
	for key := range toRestart {
		var err error
		switch key.Kind {
		case kindDeployment:
			_, err = r.Clientset.AppsV1().Deployments(key.Namespace).Patch(ctx, key.Name, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
		case kindStatefulSet:
			_, err = r.Clientset.AppsV1().StatefulSets(key.Namespace).Patch(ctx, key.Name, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
		case kindDaemonSet:
			_, err = r.Clientset.AppsV1().DaemonSets(key.Namespace).Patch(ctx, key.Name, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
		}
		if err != nil {
			logger.Error(err, "failed to patch workload for rollout", "namespace", key.Namespace, "name", key.Name, "kind", key.Kind)
			errs = append(errs, fmt.Errorf("patch %s %s/%s: %w", key.Kind, key.Namespace, key.Name, err))
		} else {
			logger.Info("triggered rollout", "namespace", key.Namespace, "name", key.Name, "kind", key.Kind)
			r.Metrics.RecordRestart(key.Namespace, key.Kind, key.Name)
		}
	}
	return errors.Join(errs...)
}

// matchesAnyTarget reports whether the pod's owner chain satisfies any of the
// supplied restart targets. All targets here have already been validated to
// share the pod's namespace.
func matchesAnyTarget(info podinfoMatcher, targets []restartCriterion) bool {
	for _, t := range targets {
		if matchesTarget(info, t) {
			return true
		}
	}
	return false
}

// podinfoMatcher decouples matchesTarget from the registry.PodInfo concrete
// type so it stays trivially testable.
type podinfoMatcher = registry.PodInfo

func matchesTarget(info podinfoMatcher, t restartCriterion) bool {
	switch t.Kind {
	case kindDeployment:
		// Pod is owned (transitively) by a Deployment when podinfo.Resolve
		// populates DeploymentName via the RS chain — or directly, if the
		// pod's controller ref is itself a Deployment.
		if info.DeploymentName == "" {
			return false
		}
		return t.Name == "" || t.Name == info.DeploymentName
	case kindReplicaSet, kindStatefulSet, kindDaemonSet:
		if info.OwnerKind != t.Kind {
			return false
		}
		return t.Name == "" || t.Name == info.OwnerName
	}
	return false
}

// hasSelectorAnnotation is the predicate filter we apply to the ConfigMap
// watch so the controller only wakes up for objects we care about.
func hasSelectorAnnotation(obj client.Object) bool {
	_, ok := obj.GetAnnotations()[configmap.SelectorAnnotation]
	return ok
}

func (r *ConfigMapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	annotated := predicate.NewPredicateFuncs(hasSelectorAnnotation)
	// The Watches on ReplicaSets exists purely to hydrate the manager cache so
	// podinfo.Resolve (called per pod during eviction) reads RSes from the
	// informer instead of making per-pod API calls. The handler returns no
	// reconcile requests — RS events do not trigger ConfigMap reconciles.
	return ctrl.NewControllerManagedBy(mgr).
		Named("configmap-selector").
		For(&corev1.ConfigMap{}, builder.WithPredicates(annotated)).
		Watches(&appsv1.ReplicaSet{}, handler.EnqueueRequestsFromMapFunc(noEnqueue)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

func noEnqueue(_ context.Context, _ client.Object) []reconcile.Request {
	return nil
}

// dialWebhookService TCP-dials our own webhook Service. A successful TLS
// handshake (the apiserver speaks HTTPS to us) confirms kube-proxy has
// programmed the Service VIP and the cert is being served. We tolerate any
// cert (InsecureSkipVerify) — we're not validating identity, only routability.
func dialWebhookService(addr string) error {
	d := &net.Dialer{Timeout: 2 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}
