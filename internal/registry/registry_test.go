/*
Copyright 2026.
*/

package registry

import (
	"testing"

	"go.opentelemetry.io/obi/pkg/appolly/services"

	"github.com/grafana/beyla/v3/pkg/webhook/configmap"
)

// rule is a small helper to build a single-rule Instrumentation.
func rule(sel configmap.K8sSelector) Instrumentation {
	return Instrumentation{InjectConfig: configmap.InjectConfig{Rules: []configmap.Rule{{Selector: sel}}}}
}

// globs wraps one or more patterns as a []services.GlobAttr for selector fields.
func globs(patterns ...string) []services.GlobAttr {
	out := make([]services.GlobAttr, 0, len(patterns))
	for _, p := range patterns {
		out = append(out, services.NewGlob(p))
	}
	return out
}

func TestMatch(t *testing.T) {
	// Pod fixtures with various owner chain shapes.
	rsPod := PodInfo{
		Name: "hello-abc-123", Namespace: "demo",
		OwnerChain: []configmap.Owner{
			{Kind: "Pod", Name: "hello-abc-123"},
			{Kind: "ReplicaSet", Name: "hello-abc"},
			{Kind: "Deployment", Name: "hello"},
		},
	}
	stsPod := PodInfo{
		Name: "db-0", Namespace: "demo",
		OwnerChain: []configmap.Owner{
			{Kind: "Pod", Name: "db-0"},
			{Kind: "StatefulSet", Name: "db"},
		},
	}
	dsPod := PodInfo{
		Name: "agent-xx", Namespace: "kube-system",
		OwnerChain: []configmap.Owner{
			{Kind: "Pod", Name: "agent-xx"},
			{Kind: "DaemonSet", Name: "agent"},
		},
	}
	bareRSPod := PodInfo{
		Name: "raw", Namespace: "demo",
		OwnerChain: []configmap.Owner{
			{Kind: "Pod", Name: "raw"},
			{Kind: "ReplicaSet", Name: "raw-rs"},
			// no Deployment — RS not owned by a Deployment
		},
	}
	barePod := PodInfo{
		Name: "my-debug-pod", Namespace: "debug",
		OwnerChain: []configmap.Owner{
			{Kind: "Pod", Name: "my-debug-pod"},
		},
	}
	labeledPod := PodInfo{
		Name: "labeled-app-1", Namespace: "test-unmatched",
		OwnerChain: []configmap.Owner{
			{Kind: "Pod", Name: "labeled-app-1"},
			{Kind: "ReplicaSet", Name: "labeled-app"},
		},
		Labels:      map[string]string{"inject": "true", "tier": "web"},
		Annotations: map[string]string{"team": "obs"},
	}
	unlabeledPod := PodInfo{
		Name: "unlabeled-app-1", Namespace: "test-unmatched",
		OwnerChain: []configmap.Owner{
			{Kind: "Pod", Name: "unlabeled-app-1"},
			{Kind: "ReplicaSet", Name: "unlabeled-app"},
		},
	}

	tests := []struct {
		name string
		inst Instrumentation
		pod  PodInfo
		want bool
	}{
		// Smoke tests
		{
			name: "empty selector matches everything",
			inst: rule(configmap.K8sSelector{}),
			pod:  rsPod,
			want: true,
		},
		{
			name: "no rules — no match",
			inst: Instrumentation{},
			pod:  rsPod,
			want: false,
		},

		// Owner chain: pod itself is always the first chain entry
		{
			name: "owned pod selectable by pod name",
			inst: rule(configmap.K8sSelector{OwnerNames: globs("hello-abc-123"), OwnerKinds: []string{"Pod"}}),
			pod:  rsPod,
			want: true,
		},
		{
			name: "bare pod selectable by name",
			inst: rule(configmap.K8sSelector{OwnerNames: globs("my-debug-pod"), OwnerKinds: []string{"Pod"}}),
			pod:  barePod,
			want: true,
		},
		{
			name: "pod name glob",
			inst: rule(configmap.K8sSelector{OwnerNames: globs("my-debug-*"), OwnerKinds: []string{"Pod"}}),
			pod:  barePod,
			want: true,
		},

		// Owner chain: direct owner
		{
			name: "RS direct owner in chain",
			inst: rule(configmap.K8sSelector{OwnerNames: globs("hello-abc"), OwnerKinds: []string{"ReplicaSet"}}),
			pod:  rsPod,
			want: true,
		},
		{
			name: "StatefulSet in chain",
			inst: rule(configmap.K8sSelector{OwnerNames: globs("db"), OwnerKinds: []string{"StatefulSet"}}),
			pod:  stsPod,
			want: true,
		},
		{
			name: "DaemonSet in chain",
			inst: rule(configmap.K8sSelector{OwnerNames: globs("agent"), OwnerKinds: []string{"DaemonSet"}}),
			pod:  dsPod,
			want: true,
		},

		// Owner chain: Deployment ancestor via RS chain
		{
			name: "Deployment via RS chain",
			inst: rule(configmap.K8sSelector{OwnerNames: globs("hello"), OwnerKinds: []string{"Deployment"}}),
			pod:  rsPod,
			want: true,
		},
		{
			name: "Deployment glob via RS chain",
			inst: rule(configmap.K8sSelector{OwnerNames: globs("hel*"), OwnerKinds: []string{"Deployment"}}),
			pod:  rsPod,
			want: true,
		},
		{
			name: "no Deployment when RS has no Deployment ancestor",
			inst: rule(configmap.K8sSelector{OwnerKinds: []string{"Deployment"}}),
			pod:  bareRSPod,
			want: false,
		},

		// Namespace
		{
			name: "namespace match",
			inst: rule(configmap.K8sSelector{Namespaces: globs("demo")}),
			pod:  rsPod,
			want: true,
		},
		{
			name: "namespace mismatch",
			inst: rule(configmap.K8sSelector{Namespaces: globs("other")}),
			pod:  rsPod,
			want: false,
		},
		{
			name: "AND of namespace + owner name",
			inst: rule(configmap.K8sSelector{Namespaces: globs("demo"), OwnerNames: globs("hello"), OwnerKinds: []string{"Deployment"}}),
			pod:  rsPod,
			want: true,
		},
		{
			name: "AND fails when namespace misses",
			inst: rule(configmap.K8sSelector{Namespaces: globs("other"), OwnerNames: globs("hello"), OwnerKinds: []string{"Deployment"}}),
			pod:  rsPod,
			want: false,
		},

		// Owner name without kind matches any link by name
		{
			name: "owner name glob across owner kinds",
			inst: rule(configmap.K8sSelector{OwnerNames: globs("*")}),
			pod:  stsPod,
			want: true,
		},

		// Pod labels / annotations
		{
			name: "pod label match",
			inst: rule(configmap.K8sSelector{PodLabels: map[string]services.GlobAttr{"inject": services.NewGlob("true")}}),
			pod:  labeledPod,
			want: true,
		},
		{
			name: "pod label glob match",
			inst: rule(configmap.K8sSelector{PodLabels: map[string]services.GlobAttr{"tier": services.NewGlob("we*")}}),
			pod:  labeledPod,
			want: true,
		},
		{
			name: "pod label value mismatch",
			inst: rule(configmap.K8sSelector{PodLabels: map[string]services.GlobAttr{"inject": services.NewGlob("false")}}),
			pod:  labeledPod,
			want: false,
		},
		{
			name: "pod label missing key misses",
			inst: rule(configmap.K8sSelector{PodLabels: map[string]services.GlobAttr{"inject": services.NewGlob("true")}}),
			pod:  unlabeledPod,
			want: false,
		},
		{
			name: "multiple required labels: all must match",
			inst: rule(configmap.K8sSelector{PodLabels: map[string]services.GlobAttr{"inject": services.NewGlob("true"), "tier": services.NewGlob("web")}}),
			pod:  labeledPod,
			want: true,
		},
		{
			name: "multiple required labels: one missing misses",
			inst: rule(configmap.K8sSelector{PodLabels: map[string]services.GlobAttr{"inject": services.NewGlob("true"), "tier": services.NewGlob("db")}}),
			pod:  labeledPod,
			want: false,
		},
		{
			name: "pod annotation match",
			inst: rule(configmap.K8sSelector{PodAnnotations: map[string]services.GlobAttr{"team": services.NewGlob("obs")}}),
			pod:  labeledPod,
			want: true,
		},
		{
			name: "pod annotation missing key misses",
			inst: rule(configmap.K8sSelector{PodAnnotations: map[string]services.GlobAttr{"team": services.NewGlob("obs")}}),
			pod:  unlabeledPod,
			want: false,
		},
		{
			name: "namespace AND pod label: labeled-app matches",
			inst: rule(configmap.K8sSelector{Namespaces: globs("test-unmatched"), PodLabels: map[string]services.GlobAttr{"inject": services.NewGlob("true")}}),
			pod:  labeledPod,
			want: true,
		},
		{
			name: "namespace AND pod label: unlabeled-app rejected",
			inst: rule(configmap.K8sSelector{Namespaces: globs("test-unmatched"), PodLabels: map[string]services.GlobAttr{"inject": services.NewGlob("true")}}),
			pod:  unlabeledPod,
			want: false,
		},

		// Registry-level: OR across rules (first-match iteration)
		{
			name: "OR across rules — second matches",
			inst: Instrumentation{InjectConfig: configmap.InjectConfig{Rules: []configmap.Rule{
				{Selector: configmap.K8sSelector{Namespaces: globs("nope")}},
				{Selector: configmap.K8sSelector{OwnerNames: globs("hello")}},
			}}},
			pod:  rsPod,
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := New()
			r.Set("test/cm", tc.inst)
			if _, _, got := r.Match(tc.pod); got != tc.want {
				t.Fatalf("Match(%+v) = %v, want %v", tc.pod, got, tc.want)
			}
		})
	}
}

func TestSetAndDelete(t *testing.T) {
	r := New()
	pod := PodInfo{Name: "p", Namespace: "demo", OwnerChain: []configmap.Owner{{Kind: "Pod", Name: "p"}}}

	nsDemo := rule(configmap.K8sSelector{Namespaces: globs("demo")})

	// Initial set: matches.
	r.Set("a/cm1", nsDemo)
	if _, _, ok := r.Match(pod); !ok {
		t.Fatalf("expected match after Set")
	}

	// Update with empty rules == delete.
	r.Set("a/cm1", Instrumentation{})
	if _, _, ok := r.Match(pod); ok {
		t.Fatalf("expected no match after empty Set")
	}

	// Two CMs cover the same pod; deleting one keeps the match alive.
	r.Set("a/cm1", nsDemo)
	r.Set("a/cm2", nsDemo)
	r.Delete("a/cm1")
	if _, _, ok := r.Match(pod); !ok {
		t.Fatalf("expected match still covered by cm2")
	}
	r.Delete("a/cm2")
	if _, _, ok := r.Match(pod); ok {
		t.Fatalf("expected no match after deleting all CMs")
	}
}

// TestMatch_SkipRules verifies that a rule with Mode: skip excludes a matching
// pod (first-match-wins), which is how Beyla's exclude_instrument is honored:
// it is emitted as a leading skip rule.
func TestMatch_SkipRules(t *testing.T) {
	helloPod := PodInfo{
		Name: "hello-abc-123", Namespace: "demo",
		OwnerChain: []configmap.Owner{
			{Kind: "Pod", Name: "hello-abc-123"},
			{Kind: "ReplicaSet", Name: "hello-abc"},
			{Kind: "Deployment", Name: "hello"},
		},
	}
	skip := func(sel configmap.K8sSelector) configmap.Rule {
		return configmap.Rule{Selector: sel, Config: configmap.RuleConfig{Mode: configmap.ModeSkip}}
	}
	install := func(sel configmap.K8sSelector) configmap.Rule {
		return configmap.Rule{Selector: sel}
	}
	inst := func(rules ...configmap.Rule) Instrumentation {
		return Instrumentation{InjectConfig: configmap.InjectConfig{Rules: rules}}
	}

	tests := []struct {
		name string
		inst Instrumentation
		want bool
	}{
		{
			name: "leading skip rule excludes a matching pod",
			inst: inst(
				skip(configmap.K8sSelector{Namespaces: globs("demo"), OwnerNames: globs("hello")}),
				install(configmap.K8sSelector{Namespaces: globs("demo")}),
			),
			want: false,
		},
		{
			name: "skip rule that does not match falls through to install",
			inst: inst(
				skip(configmap.K8sSelector{OwnerNames: globs("other")}),
				install(configmap.K8sSelector{Namespaces: globs("demo")}),
			),
			want: true,
		},
		{
			name: "install before skip — first match wins, pod is instrumented",
			inst: inst(
				install(configmap.K8sSelector{Namespaces: globs("demo")}),
				skip(configmap.K8sSelector{OwnerNames: globs("hello")}),
			),
			want: true,
		},
		{
			name: "skip-only rule matching means not instrumented",
			inst: inst(skip(configmap.K8sSelector{Namespaces: globs("demo")})),
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := New()
			r.Set("test/cm", tc.inst)
			if _, _, got := r.Match(helloPod); got != tc.want {
				t.Fatalf("Match() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBFPGeneratesSpanMetrics covers the BPF span-metrics gate: a pod only
// generates span metrics when its instrument has BPFConfig.SpanMetrics enabled
// AND a (non-skip) BPF rule selects it.
func TestBFPGeneratesSpanMetrics(t *testing.T) {
	helloPod := PodInfo{
		Name: "hello-abc-123", Namespace: "demo",
		OwnerChain: []configmap.Owner{
			{Kind: "Pod", Name: "hello-abc-123"},
			{Kind: "ReplicaSet", Name: "hello-abc"},
			{Kind: "Deployment", Name: "hello"},
		},
	}

	bpf := func(spanMetrics bool, rules ...configmap.Rule) Instrumentation {
		return Instrumentation{InjectConfig: configmap.InjectConfig{
			BPFConfig: configmap.BPFConfig{SpanMetrics: spanMetrics, Rules: rules},
		}}
	}
	install := func(sel configmap.K8sSelector) configmap.Rule {
		return configmap.Rule{Selector: sel}
	}
	skip := func(sel configmap.K8sSelector) configmap.Rule {
		return configmap.Rule{Selector: sel, Config: configmap.RuleConfig{Mode: configmap.ModeSkip}}
	}

	tests := []struct {
		name string
		inst Instrumentation
		want bool
	}{
		{
			// Empty config: no BPFConfig at all.
			name: "empty bpfconfig — no span metrics",
			inst: Instrumentation{},
			want: false,
		},
		{
			// Top-level inject rules exist, but BPF span metrics is off.
			name: "config with install rules but span metrics not set",
			inst: Instrumentation{InjectConfig: configmap.InjectConfig{
				Rules: []configmap.Rule{install(configmap.K8sSelector{Namespaces: globs("demo")})},
			}},
			want: false,
		},
		{
			// SpanMetrics off even though a matching BPF rule is present.
			name: "matching bpf rule but span metrics disabled",
			inst: bpf(false, install(configmap.K8sSelector{Namespaces: globs("demo")})),
			want: false,
		},
		{
			// SpanMetrics on but there are no BPF rules to select the pod.
			name: "span metrics enabled but no bpf rules",
			inst: bpf(true),
			want: false,
		},
		{
			name: "span metrics enabled with matching bpf rule",
			inst: bpf(true, install(configmap.K8sSelector{Namespaces: globs("demo")})),
			want: true,
		},
		{
			name: "span metrics enabled but bpf rule does not match",
			inst: bpf(true, install(configmap.K8sSelector{Namespaces: globs("other-ns")})),
			want: false,
		},
		{
			name: "span metrics enabled but matching bpf rule is skip",
			inst: bpf(true, skip(configmap.K8sSelector{Namespaces: globs("demo")})),
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := New()
			// Insert directly rather than via Set: Set treats an empty top-level
			// Rules slice as a delete, which would drop BPF-only instruments
			// before BFPGeneratesSpanMetrics ever sees them.
			r.instruments["test/cm"] = tc.inst
			if _, got := r.BPFGeneratesSpanMetrics(helloPod); got != tc.want {
				t.Fatalf("BFPGeneratesSpanMetrics() = %v, want %v", got, tc.want)
			}
		})
	}
}
