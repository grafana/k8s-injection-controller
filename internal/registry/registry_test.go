/*
Copyright 2026.
*/

package registry

import (
	"testing"

	"go.opentelemetry.io/obi/pkg/appolly/services"
)

// g is a small helper to build a *services.GlobAttr in test struct literals.
func g(pattern string) *services.GlobAttr {
	a := services.NewGlob(pattern)
	return &a
}

func TestMatch(t *testing.T) {
	rsPod := PodInfo{
		Name: "hello-abc-123", Namespace: "demo",
		OwnerKind: "ReplicaSet", OwnerName: "hello-abc",
		DeploymentName: "hello",
	}
	stsPod := PodInfo{
		Name: "db-0", Namespace: "demo",
		OwnerKind: "StatefulSet", OwnerName: "db",
	}
	dsPod := PodInfo{
		Name: "agent-xx", Namespace: "kube-system",
		OwnerKind: "DaemonSet", OwnerName: "agent",
	}
	bareRSPod := PodInfo{
		Name: "raw", Namespace: "demo",
		OwnerKind: "ReplicaSet", OwnerName: "raw-rs",
	}
	labeledPod := PodInfo{
		Name: "labeled-app-1", Namespace: "test-unmatched",
		OwnerKind: "ReplicaSet", OwnerName: "labeled-app",
		Labels:      map[string]string{"inject": "true", "tier": "web"},
		Annotations: map[string]string{"team": "obs"},
	}
	unlabeledPod := PodInfo{
		Name: "unlabeled-app-1", Namespace: "test-unmatched",
		OwnerKind: "ReplicaSet", OwnerName: "unlabeled-app",
	}

	tests := []struct {
		name     string
		criteria []SelectionCriterion
		pod      PodInfo
		want     bool
	}{
		{
			name:     "empty criterion matches everything",
			criteria: []SelectionCriterion{{}},
			pod:      rsPod,
			want:     true,
		},
		{
			name:     "no criteria, no match",
			criteria: nil,
			pod:      rsPod,
			want:     false,
		},
		{
			name:     "namespace literal match",
			criteria: []SelectionCriterion{{K8sNamespace: g("demo")}},
			pod:      rsPod,
			want:     true,
		},
		{
			name:     "namespace literal miss",
			criteria: []SelectionCriterion{{K8sNamespace: g("other")}},
			pod:      rsPod,
			want:     false,
		},
		{
			name:     "namespace glob match",
			criteria: []SelectionCriterion{{K8sNamespace: g("dem*")}},
			pod:      rsPod,
			want:     true,
		},
		{
			name:     "pod name match",
			criteria: []SelectionCriterion{{K8sPodName: g("hello-abc-123")}},
			pod:      rsPod,
			want:     true,
		},
		{
			name:     "pod name glob match",
			criteria: []SelectionCriterion{{K8sPodName: g("hello-*")}},
			pod:      rsPod,
			want:     true,
		},
		{
			name:     "deployment via RS chain",
			criteria: []SelectionCriterion{{K8sDeploymentName: g("hello")}},
			pod:      rsPod,
			want:     true,
		},
		{
			name:     "deployment glob via RS chain",
			criteria: []SelectionCriterion{{K8sDeploymentName: g("hel*")}},
			pod:      rsPod,
			want:     true,
		},
		{
			name:     "deployment requires resolved name; bare RS pod misses",
			criteria: []SelectionCriterion{{K8sDeploymentName: g("raw-rs")}},
			pod:      bareRSPod,
			want:     false,
		},
		{
			name:     "replicaset match (direct owner)",
			criteria: []SelectionCriterion{{K8sReplicaSetName: g("hello-abc")}},
			pod:      rsPod,
			want:     true,
		},
		{
			name:     "replicaset name on STS pod misses",
			criteria: []SelectionCriterion{{K8sReplicaSetName: g("db")}},
			pod:      stsPod,
			want:     false,
		},
		{
			name:     "statefulset match",
			criteria: []SelectionCriterion{{K8sStatefulSetName: g("db")}},
			pod:      stsPod,
			want:     true,
		},
		{
			name:     "daemonset match",
			criteria: []SelectionCriterion{{K8sDaemonSetName: g("agent")}},
			pod:      dsPod,
			want:     true,
		},
		{
			name:     "AND of namespace + deployment",
			criteria: []SelectionCriterion{{K8sNamespace: g("demo"), K8sDeploymentName: g("hello")}},
			pod:      rsPod,
			want:     true,
		},
		{
			name:     "AND fails when one field misses",
			criteria: []SelectionCriterion{{K8sNamespace: g("other"), K8sDeploymentName: g("hello")}},
			pod:      rsPod,
			want:     false,
		},
		{
			name:     "k8s_owner_name matches Deployment via RS chain",
			criteria: []SelectionCriterion{{K8sOwnerName: g("hello")}},
			pod:      rsPod,
			want:     true,
		},
		{
			name:     "k8s_owner_name matches direct RS owner",
			criteria: []SelectionCriterion{{K8sOwnerName: g("hello-abc")}},
			pod:      rsPod,
			want:     true,
		},
		{
			name:     "k8s_owner_name glob across owner kinds",
			criteria: []SelectionCriterion{{K8sOwnerName: g("*")}},
			pod:      stsPod,
			want:     true,
		},
		{
			name:     "k8s_owner_name AND k8s_deployment_name (both match)",
			criteria: []SelectionCriterion{{K8sOwnerName: g("hello"), K8sDeploymentName: g("hello")}},
			pod:      rsPod,
			want:     true,
		},
		{
			name:     "k8s_owner_name AND k8s_deployment_name (owner mismatch)",
			criteria: []SelectionCriterion{{K8sOwnerName: g("other"), K8sDeploymentName: g("hello")}},
			pod:      rsPod,
			want:     false,
		},
		{
			name:     "pod label match",
			criteria: []SelectionCriterion{{K8sPodLabels: map[string]*services.GlobAttr{"inject": g("true")}}},
			pod:      labeledPod,
			want:     true,
		},
		{
			name:     "pod label glob match",
			criteria: []SelectionCriterion{{K8sPodLabels: map[string]*services.GlobAttr{"tier": g("we*")}}},
			pod:      labeledPod,
			want:     true,
		},
		{
			name:     "pod label value mismatch",
			criteria: []SelectionCriterion{{K8sPodLabels: map[string]*services.GlobAttr{"inject": g("false")}}},
			pod:      labeledPod,
			want:     false,
		},
		{
			name:     "pod label missing key misses",
			criteria: []SelectionCriterion{{K8sPodLabels: map[string]*services.GlobAttr{"inject": g("true")}}},
			pod:      unlabeledPod,
			want:     false,
		},
		{
			name: "namespace AND pod label: labeled-app matches",
			criteria: []SelectionCriterion{{
				K8sNamespace: g("test-unmatched"),
				K8sPodLabels: map[string]*services.GlobAttr{"inject": g("true")},
			}},
			pod:  labeledPod,
			want: true,
		},
		{
			name: "namespace AND pod label: unlabeled-app rejected (regression for dropped label clause)",
			criteria: []SelectionCriterion{{
				K8sNamespace: g("test-unmatched"),
				K8sPodLabels: map[string]*services.GlobAttr{"inject": g("true")},
			}},
			pod:  unlabeledPod,
			want: false,
		},
		{
			name: "multiple required labels: all must match",
			criteria: []SelectionCriterion{{
				K8sPodLabels: map[string]*services.GlobAttr{"inject": g("true"), "tier": g("web")},
			}},
			pod:  labeledPod,
			want: true,
		},
		{
			name: "multiple required labels: one missing misses",
			criteria: []SelectionCriterion{{
				K8sPodLabels: map[string]*services.GlobAttr{"inject": g("true"), "tier": g("db")},
			}},
			pod:  labeledPod,
			want: false,
		},
		{
			name:     "pod annotation match",
			criteria: []SelectionCriterion{{K8sPodAnnotations: map[string]*services.GlobAttr{"team": g("obs")}}},
			pod:      labeledPod,
			want:     true,
		},
		{
			name:     "pod annotation missing key misses",
			criteria: []SelectionCriterion{{K8sPodAnnotations: map[string]*services.GlobAttr{"team": g("obs")}}},
			pod:      unlabeledPod,
			want:     false,
		},
		{
			name: "OR across criteria",
			criteria: []SelectionCriterion{
				{K8sNamespace: g("nope")},
				{K8sDeploymentName: g("hello")},
			},
			pod:  rsPod,
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := New()
			r.Set("test/cm", Instrumentation{Criteria: tc.criteria})
			if _, got := r.Match(tc.pod); got != tc.want {
				t.Fatalf("Match(%+v) = %v, want %v", tc.pod, got, tc.want)
			}
		})
	}
}

func TestSetAndDelete(t *testing.T) {
	r := New()
	pod := PodInfo{Name: "p", Namespace: "demo"}

	// Initial set: matches.
	r.Set("a/cm1", Instrumentation{Criteria: []SelectionCriterion{{K8sNamespace: g("demo")}}})
	if _, ok := r.Match(pod); !ok {
		t.Fatalf("expected match after Set")
	}

	// Update with empty criteria == delete.
	r.Set("a/cm1", Instrumentation{})
	if _, ok := r.Match(pod); ok {
		t.Fatalf("expected no match after empty Set")
	}

	// Two CMs cover the same pod; deleting one keeps the match alive.
	r.Set("a/cm1", Instrumentation{Criteria: []SelectionCriterion{{K8sNamespace: g("demo")}}})
	r.Set("a/cm2", Instrumentation{Criteria: []SelectionCriterion{{K8sNamespace: g("demo")}}})
	r.Delete("a/cm1")
	if _, ok := r.Match(pod); !ok {
		t.Fatalf("expected match still covered by cm2")
	}
	r.Delete("a/cm2")
	if _, ok := r.Match(pod); ok {
		t.Fatalf("expected no match after deleting all CMs")
	}
}

func TestMatch_Exclusion(t *testing.T) {
	// "all pods in namespace demo except deployment skip-me"
	include := []SelectionCriterion{{K8sNamespace: g("demo")}}
	exclude := []SelectionCriterion{{K8sNamespace: g("demo"), K8sDeploymentName: g("skip-me")}}

	excluded := PodInfo{
		Name: "skip-me-abc-1", Namespace: "demo",
		OwnerKind: "ReplicaSet", OwnerName: "skip-me-abc", DeploymentName: "skip-me",
	}
	kept := PodInfo{
		Name: "keep-me-abc-1", Namespace: "demo",
		OwnerKind: "ReplicaSet", OwnerName: "keep-me-abc", DeploymentName: "keep-me",
	}

	tests := []struct {
		name     string
		criteria []SelectionCriterion
		exclude  []SelectionCriterion
		pod      PodInfo
		want     bool
	}{
		{
			name:     "excluded pod is skipped even though it matches include",
			criteria: include,
			exclude:  exclude,
			pod:      excluded,
			want:     false,
		},
		{
			name:     "non-excluded pod in the same namespace is still matched",
			criteria: include,
			exclude:  exclude,
			pod:      kept,
			want:     true,
		},
		{
			name:     "exclude that does not match leaves the include intact",
			criteria: include,
			exclude:  []SelectionCriterion{{K8sNamespace: g("other")}},
			pod:      kept,
			want:     true,
		},
		{
			name:     "exclude only, no include, matches nothing",
			criteria: nil,
			exclude:  exclude,
			pod:      excluded,
			want:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := New()
			r.Set("test/cm", Instrumentation{Criteria: tc.criteria, ExcludeCriteria: tc.exclude})
			if _, got := r.Match(tc.pod); got != tc.want {
				t.Errorf("Match() = %v, want %v", got, tc.want)
			}
		})
	}
}
