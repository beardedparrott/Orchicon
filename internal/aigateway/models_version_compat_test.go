package aigateway

// models_version_compat_test.go — `opencode models` ACROSS THE 1.x → 2.x BREAK.
//
// The two versions disagree about this command, and the disagreement is what broke model discovery on
// a WSL install running opencode 2.0.26:
//
//	1.x (verified against 1.18.34):  opencode models [provider] [--refresh] [--verbose]
//	                                 --verbose emits a per-model JSON block with cost and limits.
//	2.x (2.0.26, packages/cli/src/commands/commands.ts):
//	                                 `models` survives, but it declares ONLY the server params
//	                                 (--standalone/--server). There is no --verbose, so the
//	                                 documented invocation is an UNKNOWN OPTION and the CLI exits 1.
//
// So there are two distinct failures to prevent, and the second is the quieter one:
//
//  1. the invocation fails outright (`exit status 1`), and
//  2. had it succeeded, the metadata-free output would have parsed to ZERO models — the parser
//     discarded a bare `provider/model` ref — and discovery would have reported "no models found".
//
// Stubs are shell scripts rather than a real opencode: CI has no opencode, and a test that needs one
// installed is a test that silently does not run.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// modelsStub writes a CLI double that behaves like a given opencode version.
//
// acceptsVerbose models the version difference: when false, any `--verbose` is rejected the way the
// real 2.x CLI rejects it — a non-zero exit with the usage text on stderr — while a bare `models`
// succeeds. callsPath, when non-empty, receives one line per invocation so a test can assert how many
// times each form was tried.
func modelsStub(t *testing.T, acceptsVerbose bool, callsPath string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode")

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	if callsPath != "" {
		b.WriteString("printf '%s\\n' \"$*\" >> " + callsPath + "\n")
	}
	b.WriteString("verbose=0\n")
	b.WriteString("for a in \"$@\"; do [ \"$a\" = \"--verbose\" ] && verbose=1; done\n")
	if acceptsVerbose {
		// 1.x shape: a ref line followed by a metadata block.
		b.WriteString("cat <<'OUT'\n")
		b.WriteString("anthropic/claude-sonnet-4\n")
		b.WriteString("{\n  \"id\": \"claude-sonnet-4\",\n  \"providerID\": \"anthropic\",\n  \"name\": \"Claude Sonnet 4\"\n}\n")
		b.WriteString("OUT\n")
		b.WriteString("exit 0\n")
	} else {
		// 2.x shape: bare refs, and --verbose is an unknown option.
		b.WriteString("if [ \"$verbose\" = \"1\" ]; then\n")
		b.WriteString("  echo 'opencode models' >&2\n")
		b.WriteString("  echo 'Unknown argument: verbose' >&2\n")
		b.WriteString("  exit 1\n")
		b.WriteString("fi\n")
		b.WriteString("cat <<'OUT'\n")
		b.WriteString("anthropic/claude-sonnet-4\n")
		b.WriteString("opencode/big-pickle\n")
		b.WriteString("OUT\n")
		b.WriteString("exit 0\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

func callsTo(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestModelsVerboseIsUsedWhenTheCLIAcceptsIt(t *testing.T) {
	// The 1.x path must be UNCHANGED: metadata is why --verbose is used at all.
	bin := modelsStub(t, true, "")
	d := NewModelDiscoverer(discardLog(), bin)

	models, err := d.fetchModels(context.Background())
	if err != nil {
		t.Fatalf("fetchModels: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("got %d models, want 1 — the metadata form must still be used", len(models))
	}
	if models[0].Name != "Claude Sonnet 4" {
		t.Errorf("Name = %q, want the metadata value — the JSON block must still be parsed",
			models[0].Name)
	}
}

func TestModelsFallsBackWhenTheCLIRejectsVerbose(t *testing.T) {
	// The 2.x path: --verbose is an unknown option, so the minimum invocation is retried and its
	// metadata-free refs must become MODELS rather than an empty list.
	bin := modelsStub(t, false, "")
	d := NewModelDiscoverer(discardLog(), bin)

	models, err := d.fetchModels(context.Background())
	if err != nil {
		t.Fatalf("fetchModels must not fail on a CLI that rejects --verbose: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2 — a bare ref with no metadata is still a model; discarding "+
			"it is how a 2.x install reports 'no models found'", len(models))
	}
	byRef := map[string]*struct{ provider, id string }{}
	for _, m := range models {
		if m.ModelRef == "" {
			t.Errorf("a model with no ModelRef cannot be selected: %+v", m)
		}
		byRef[m.ModelRef] = &struct{ provider, id string }{m.ProviderId, m.Id}
	}
	got, ok := byRef["anthropic/claude-sonnet-4"]
	if !ok {
		t.Fatalf("anthropic/claude-sonnet-4 missing from %v", byRef)
	}
	if got.provider != "anthropic" || got.id != "claude-sonnet-4" {
		t.Errorf("ref split = provider %q id %q, want anthropic / claude-sonnet-4",
			got.provider, got.id)
	}
	// Cost/limits are absent in this form and must stay nil rather than be invented.
	if m := models[0]; m.Cost != nil || m.Limits != nil {
		t.Errorf("the metadata-free form must not fabricate cost/limits: %+v", m)
	}
}

func TestTheVerboseDowngradeIsRemembered(t *testing.T) {
	// A 2.x install must pay for the rejected --verbose ONCE, not on every refresh.
	calls := filepath.Join(t.TempDir(), "calls.txt")
	bin := modelsStub(t, false, calls)

	d := NewModelDiscoverer(discardLog(), bin)
	for i := 0; i < 3; i++ {
		if _, err := d.fetchModels(context.Background()); err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
	}

	verboseCalls := 0
	for _, c := range callsTo(t, calls) {
		if strings.Contains(c, "--verbose") {
			verboseCalls++
		}
	}
	if verboseCalls != 1 {
		t.Errorf("--verbose was attempted %d times across 3 fetches, want exactly 1 — the resolved "+
			"argv must be remembered so a 2.x install stops re-paying for a rejection it has "+
			"already seen.\n  calls: %v", verboseCalls, callsTo(t, calls))
	}
}

func TestAFailedProbeIsNotRemembered(t *testing.T) {
	// The other half of the same rule, and the one that protects 1.x operators: a CLI that fails
	// for a TRANSIENT reason must not be permanently downgraded to the metadata-free form, because
	// that silently loses every model's cost and context window.
	calls := filepath.Join(t.TempDir(), "calls.txt")
	dir := t.TempDir()
	bin := filepath.Join(dir, "opencode")
	// Fails entirely (no `models` support at all), then is replaced by a working 1.x CLI.
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + calls + "\necho 'temporarily broken' >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	d := NewModelDiscoverer(discardLog(), bin)
	if _, err := d.fetchModels(context.Background()); err == nil {
		t.Fatal("a CLI that fails on both forms must still error")
	}

	// Now the CLI works, and the metadata form must be tried AGAIN rather than assumed dead.
	working := modelsStub(t, true, calls)
	if err := os.WriteFile(bin, []byte(readFile(t, working)), 0o755); err != nil {
		t.Fatalf("replace: %v", err)
	}
	models, err := d.fetchModels(context.Background())
	if err != nil {
		t.Fatalf("fetch after recovery: %v", err)
	}
	if len(models) != 1 || models[0].Name != "Claude Sonnet 4" {
		t.Errorf("the metadata form must be retried after a transient failure, got %+v", models)
	}
	_ = atomic.AddInt64
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestBothFormsFailingReportsTheMinimumInvocation(t *testing.T) {
	// When neither works, the error must describe `models` alone — that is the invocation that
	// cannot work at all — and carry the CLI's own words (see cli.go). Reporting the --verbose
	// failure would suggest a flag problem where the command itself is broken.
	dir := t.TempDir()
	bin := filepath.Join(dir, "opencode")
	script := "#!/bin/sh\necho 'this build has no models command' >&2\nexit " + strconv.Itoa(3) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	d := NewModelDiscoverer(discardLog(), bin)
	_, err := d.fetchModels(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "this build has no models command") {
		t.Errorf("the CLI's own words must survive, got: %v", err)
	}
	if !strings.Contains(err.Error(), "opencode models:") {
		t.Errorf("the error must name the minimum invocation, got: %v", err)
	}
	// The invocation PREFIX must be the minimum one. The CLI's own stderr is appended after it and
	// may mention any flag, so only the prefix is asserted (see the same note in cli_error_test.go).
	if strings.Contains(err.Error(), "opencode models --verbose") {
		t.Errorf("the invocation reported must be the minimum one, not the --verbose form, got: %v", err)
	}
}
