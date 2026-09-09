package askorchicon

// Live Ask-path proof (operator-run, env-gated) — the acceptance run for
// native-path file/shell parity: the RESTORED Ask tool surface itself
// drives branch → edit → commit → push → PR → admin-squash-merge through
// ExecuteAskTool (the exact entry point chatturn.go's tool loop calls),
// against a throwaway GitHub repo (no product code touched).
//
// Gate: ORCHICON_TEST_LIVE_ASK_PROOF=1. Requires GH_TOKEN with repo scope
// (PR create/merge) and a network path to github.com. Skipped in default
// CI (never in the unit suite).

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

func TestLiveAskNativeToolParityProof(t *testing.T) {
	if os.Getenv("ORCHICON_TEST_LIVE_ASK_PROOF") != "1" {
		t.Skip("live Ask proof disabled: set ORCHICON_TEST_LIVE_ASK_PROOF=1 (operator-run, drives a real PR)")
	}
	dir := "/tmp/orchicon/live/project"
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("proof project dir missing: %s", dir)
	}
	r := testToolRegistry()
	p := (&Service{toolRegistry: r}).NativeAskTools()
	restore := askFileRootStub(func(_ context.Context, _ *db.Pool) (string, error) { return dir, nil })
	defer restore()
	ctx := context.Background()

	// Every phase rides the RESTORED provider surface — the same
	// ExecuteAskTool the native bridge's tool loop calls.
	phase := func(name, args string) string {
		t.Helper()
		out, err := p.ExecuteAskTool(ctx, name, args)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Logf("[ask %s] %s", name, strings.TrimSpace(out))
		return out
	}

	// ask_file_root: the boundary probe names the project dir.
	root := phase("ask_file_root", `{}`)
	if !strings.Contains(root, "project_dir") {
		t.Fatalf("ask_file_root envelope = %q", root)
	}
	// The suite's toolset is visible: product + file/shell tools together.
	defs := p.AskToolDefs()
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	for _, want := range hostSuiteToolNames {
		if !names[want] {
			t.Fatalf("AskToolDefs missing %q", want)
		}
	}
	// branch off develop → edit a file → commit → push.
	phase("bash", `{"command":"git checkout -q develop && git pull -q origin develop && git checkout -qb ask-native-parity-$(date +%s)"}`)
	phase("write", `{"filePath":"ask_native_parity.txt","content":"Ask Orchicon native-path tool parity: restored.\n"}`)
	phase("bash", `{"command":"git add ask_native_parity.txt && git commit -qm 'ask: native-path parity proof' && git push -q -u origin HEAD"}`)
	// PR into develop + admin squash merge (the operator's workflow).
	url := phase("bash", `{"command":"gh pr create --base develop --head $(git branch --show-current) --title 'ask: native-path parity proof' --body 'throwaway proof file; no product code' 2>&1"}`)
	if !strings.Contains(url, "/pull/") {
		t.Fatalf("no PR URL from gh pr create: %q", url)
	}
	mrg := phase("bash", `{"command":"gh pr merge --squash --admin 2>&1"}`)
	state := phase("bash", `{"command":"gh pr view --json state,mergedAt --jq .state"}`)
	if !strings.Contains(state, "MERGED") {
		t.Fatalf("PR not merged (merge output %q, state %q)", mrg, state)
	}
	// The PR URL is the proof artifact — emitted, never swallowed.
	t.Logf("PROOF PR_URL=%s PR_STATE=MERGED", strings.TrimSpace(url))
}
