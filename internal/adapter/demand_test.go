package adapter

import "testing"

// serveDependent mirrors runtime.Lifecycle.ServeDependent (opencode-only,
// empty → opencode). It is passed IN to NeedsServe — the primitive never
// hardcodes the serve dependency, which is what keeps the run gate and the
// host serve on one predicate (AC 7).
func serveDependent(kind string) bool {
	if kind == "" {
		kind = DefaultAdapterKind
	}
	return kind == DefaultAdapterKind
}

func TestAdapterDemandSetKinds(t *testing.T) {
	cases := []struct {
		name string
		refs []string
		want []string
	}{
		{
			name: "3-segment explicit kinds",
			refs: []string{"orchicon/deepseek/deepseek-v4-flash", "opencode/anthropic/claude-sonnet-4"},
			want: []string{"opencode", "orchicon"},
		},
		{
			name: "2-segment legacy infers opencode",
			refs: []string{"anthropic/claude-sonnet-4"},
			want: []string{"opencode"},
		},
		{
			name: "bare model id infers opencode",
			refs: []string{"deepseek-v4-flash"},
			want: []string{"opencode"},
		},
		{
			name: "empty ref falls back to the default kind (conservative)",
			refs: []string{""},
			want: []string{"opencode"},
		},
		{
			name: "malformed ref falls back to the default kind (conservative)",
			refs: []string{"orchicon/"},
			want: []string{"opencode"},
		},
		{
			name: "no refs is the empty set",
			refs: nil,
			want: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AdapterDemandSet(tc.refs...).Kinds()
			if len(got) != len(tc.want) {
				t.Fatalf("Kinds() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Kinds() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestDemandSetHasAndEmpty(t *testing.T) {
	d := AdapterDemandSet("orchicon/deepseek/deepseek-v4-flash")
	if !d.Has("orchicon") {
		t.Error("Has(orchicon) = false, want true")
	}
	if d.Has("opencode") {
		t.Error("Has(opencode) = true, want false for a native-only set")
	}
	if d.Empty() {
		t.Error("Empty() = true, want false")
	}
	var nilSet DemandSet
	if !nilSet.Empty() {
		t.Error("nil DemandSet Empty() = false, want true")
	}
	if nilSet.Has("opencode") {
		t.Error("nil DemandSet Has() = true, want false")
	}
}

func TestDemandSetNeedsServe(t *testing.T) {
	if AdapterDemandSet("orchicon/deepseek/deepseek-v4-flash").NeedsServe(serveDependent) {
		t.Error("native-only demand set NeedsServe = true, want false (opencode-free plane)")
	}
	if !AdapterDemandSet("anthropic/claude-sonnet-4").NeedsServe(serveDependent) {
		t.Error("legacy opencode ref NeedsServe = false, want true")
	}
	if !AdapterDemandSet("orchicon/deepseek/x", "opencode/anthropic/y").NeedsServe(serveDependent) {
		t.Error("mixed demand set NeedsServe = false, want true")
	}
	// The conservative empty/malformed ref keeps opencode demand.
	if !AdapterDemandSet("").NeedsServe(serveDependent) {
		t.Error("empty ref NeedsServe = false, want true (conservative)")
	}
	// No refs at all → nothing to serve.
	if AdapterDemandSet().NeedsServe(serveDependent) {
		t.Error("empty demand set NeedsServe = true, want false")
	}
	// A nil predicate means no kind knowledge: never claims a serve is needed.
	if AdapterDemandSet("anthropic/claude-sonnet-4").NeedsServe(nil) {
		t.Error("nil predicate NeedsServe = true, want false")
	}
}

// TestAdapterDemandSetIsServedByAdapterKind pins the single-computation
// invariant: the demand set's kind for a ref IS what the dispatcher would
// route it to (adapter.AdapterKind), so demand and dispatch cannot drift.
func TestAdapterDemandSetIsServedByAdapterKind(t *testing.T) {
	refs := []string{
		"orchicon/deepseek/deepseek-v4-flash",
		"anthropic/claude-sonnet-4",
		"opencode/anthropic/claude-sonnet-4",
		"bare-model",
	}
	for _, ref := range refs {
		want := AdapterKind(ref)
		if want == "" {
			want = DefaultAdapterKind
		}
		if got := AdapterDemandSet(ref); !got.Has(want) || len(got) != 1 {
			t.Errorf("AdapterDemandSet(%q) = %v, want exactly {%s}", ref, got.Kinds(), want)
		}
	}
}
