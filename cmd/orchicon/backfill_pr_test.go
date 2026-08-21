package main

import (
	"encoding/json"
	"testing"
)

func TestNormalizePRState(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"MERGED", "merged"},
		{"OPEN", "open"},
		{"DRAFT", "draft"},
		{"CLOSED", "closed"},
		{"merged", "merged"},
		{"weird", "closed"}, // unknown → closed (conservative)
		{"", "closed"},
	}
	for _, tc := range cases {
		if got := normalizePRState(tc.in); got != tc.want {
			t.Errorf("normalizePRState(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func parseCtx(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	return m
}

func TestMergeRunContext(t *testing.T) {
	got, ok := mergeRunContext([]byte(`{"worktree_branch":"x"}`), map[string]any{"pr_url": "https://github.com/a/b/pull/1", "pr_state": "merged"})
	if !ok {
		t.Fatal("mergeRunContext returned ok=false")
	}
	m := parseCtx(t, got)
	if m["pr_url"] != "https://github.com/a/b/pull/1" || m["pr_state"] != "merged" || m["worktree_branch"] != "x" {
		t.Errorf("unexpected merged context: %s", got)
	}

	// Empty existing context folds fine.
	got2, ok2 := mergeRunContext(nil, map[string]any{"pr_url": "u", "pr_state": "open"})
	if !ok2 {
		t.Fatal("mergeRunContext empty base ok=false")
	}
	m2 := parseCtx(t, got2)
	if m2["pr_url"] != "u" || m2["pr_state"] != "open" {
		t.Errorf("mergeRunContext empty base = %s", got2)
	}

	// Existing pr_url is overwritten (additive merge, latest wins).
	got3, ok3 := mergeRunContext([]byte(`{"pr_url":"old","pr_state":"closed"}`), map[string]any{"pr_url": "new"})
	if !ok3 {
		t.Fatal("mergeRunContext overwrite ok=false")
	}
	m3 := parseCtx(t, got3)
	if m3["pr_url"] != "new" || m3["pr_state"] != "closed" {
		t.Errorf("mergeRunContext overwrite = %s", got3)
	}

	// Invalid existing JSON → treated as empty, add still folds in.
	got4, ok4 := mergeRunContext([]byte(`{`), map[string]any{"pr_url": "u"})
	if !ok4 {
		t.Fatal("mergeRunContext invalid base ok=false")
	}
	m4 := parseCtx(t, got4)
	if m4["pr_url"] != "u" {
		t.Errorf("mergeRunContext invalid base = %s", got4)
	}
}
