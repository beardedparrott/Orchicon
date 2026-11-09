package askorchicon

// tool_projects_git_test.go — THE ASK TOOL NOW SETS UP THE REPOSITORY IT PROMISED.
//
// REPORTED: "I had an ask orchicon session create the project record for me, maybe that's why git init never
// ran?" — exactly right. toolCreateProject wrote the row with db.CreateProject DIRECTLY, and the project SERVICE
// is the only caller of EnsureGitState (internal/project/service.go:131 and :394). So a project created by an
// Ask session never got a repository, while the same project created in the console did. Its first run then had
// no work tree to branch from.
//
// These tests pin the operator's instruction:
//
//	1. project creation asks for the git strategy, and the tool accepts it
//	2. when a git-type strategy is chosen, the tool can run the SAME setup the console runs (EnsureGitState)
//
// Gated on ORCHICON_TEST_DSN like the sibling tool tests; the git half also needs a real `git` binary.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// askCreateProjectRaw returns the tool's whole JSON result, so the git_setup block can be inspected.
func askCreateProjectRaw(t *testing.T, pool *db.Pool, args string) map[string]any {
	t.Helper()
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	out, err := toolCreateProject(ctx, pool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("toolCreateProject: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal result: %v (%s)", err, out)
	}
	return m
}

// A NON-REPO DIRECTORY OUTSIDE ANY REPOSITORY.
func bareDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "orchicon-ask-git-")
	if err != nil {
		t.Fatalf("mktemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// THE STRATEGY IS ACCEPTED AND PERSISTED — requirement 1's other half.
func TestToolCreateProjectAcceptsGitStrategy(t *testing.T) {
	pool := workItemKindTestPool(t)
	for _, want := range []string{"local", "pr", "none"} {
		row, err := askCreateProject(t, pool, `{"title":"Git Strategy `+want+` `+strings.ToLower(db.NewID())+
			`","git_strategy":"`+want+`"}`)
		if err != nil {
			t.Fatalf("git_strategy %q: %v", want, err)
		}
		if row.GitStrategy != want {
			t.Fatalf("git_strategy = %q, want %q", row.GitStrategy, want)
		}
	}
}

// A STRATEGY OUTSIDE THE SET IS REFUSED, not silently defaulted — a project quietly rewritten to `local` runs
// differently from the one the user chose.
func TestToolCreateProjectRejectsAnUnknownGitStrategy(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	_, err := toolCreateProject(ctx, pool, json.RawMessage(
		`{"title":"Bad Strategy `+strings.ToLower(db.NewID())+`","git_strategy":"svn"}`))
	if err == nil {
		t.Fatal("an unknown git_strategy was accepted")
	}
	if !strings.Contains(err.Error(), "git_strategy") {
		t.Fatalf("the refusal does not name the field: %v", err)
	}
}

// THE REPAIR: init_git runs the platform's own setup, so an Ask-created project behaves like a console-created one.
func TestToolCreateProjectInitialisesTheRepositoryWhenAsked(t *testing.T) {
	pool := workItemKindTestPool(t)
	dir := bareDir(t)

	m := askCreateProjectRaw(t, pool, `{"title":"Git Init `+strings.ToLower(db.NewID())+
		`","project_dir":`+quote(dir)+`,"git_strategy":"local","init_git":true}`)

	setup, ok := m["git_setup"].(map[string]any)
	if !ok {
		t.Fatalf("the result carries no git_setup report: %v", m)
	}
	if ran, _ := setup["ran"].(bool); !ran {
		t.Fatalf("init_git was requested and git_setup did not run: %v", setup)
	}
	if errStr, _ := setup["error"].(string); errStr != "" {
		t.Fatalf("git setup failed: %s", errStr)
	}
	// THE REPOSITORY EXISTS ON DISK — the whole point.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("no repository was created in %s: %v", dir, err)
	}
	// AND IT DID WHAT THE CONSOLE DOES: at least an init and an initial commit.
	done, _ := setup["done"].([]any)
	if len(done) == 0 {
		t.Fatalf("git_setup reported no steps: %v", setup)
	}
	var joined string
	for _, d := range done {
		if s, ok := d.(string); ok {
			joined += s + "; "
		}
	}
	for _, want := range []string{"initialised a git repository", "commit"} {
		if !strings.Contains(joined, want) {
			t.Errorf("git setup did not report %q; reported: %s", want, joined)
		}
	}
	// The result describes a project that really points at the directory now holding the repo. (ProjectRow has no
	// json tags, so its keys are the Go field names.)
	if got, _ := m["ProjectDir"].(string); got != dir {
		t.Fatalf("ProjectDir = %q, want %q", got, dir)
	}
}

// NOT ASKED → NOT DONE, AND SAID SO. Silence would let the agent believe the project is set up.
func TestToolCreateProjectSaysWhenItDidNotInitialise(t *testing.T) {
	pool := workItemKindTestPool(t)
	dir := bareDir(t)
	m := askCreateProjectRaw(t, pool, `{"title":"Git No Init `+strings.ToLower(db.NewID())+
		`","project_dir":`+quote(dir)+`,"git_strategy":"local"}`)

	setup, ok := m["git_setup"].(map[string]any)
	if !ok {
		t.Fatalf("no git_setup report: %v", m)
	}
	if ran, _ := setup["ran"].(bool); ran {
		t.Fatal("git setup ran although init_git was not requested")
	}
	reason, _ := setup["reason"].(string)
	if !strings.Contains(reason, "init_git") {
		t.Fatalf("the non-init notice does not explain itself: %q", reason)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		t.Fatal("a repository was created although init_git was not requested")
	}
}

// INITIALISING TWICE IS SAFE — the console re-runs this on every project save.
func TestToolCreateProjectGitInitIsIdempotent(t *testing.T) {
	pool := workItemKindTestPool(t)
	dir := bareDir(t)
	// Two DIFFERENT projects (title drives the slug, which is unique per tenant) on the SAME directory: the second
	// setup must be a no-op, because the console re-runs it on every save.
	first := `{"title":"Git Init Once ` + strings.ToLower(db.NewID()) +
		`","project_dir":` + quote(dir) + `,"git_strategy":"local","init_git":true}`
	second := `{"title":"Git Init Twice ` + strings.ToLower(db.NewID()) +
		`","project_dir":` + quote(dir) + `,"git_strategy":"local","init_git":true}`
	askCreateProjectRaw(t, pool, first)
	m := askCreateProjectRaw(t, pool, second)
	setup, _ := m["git_setup"].(map[string]any)
	if errStr, _ := setup["error"].(string); errStr != "" {
		t.Fatalf("the second setup failed: %s", errStr)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("the repository disappeared on the second run: %v", err)
	}
}

// quote renders a Go string as a JSON string literal, so paths with backslashes and spaces survive.
func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
