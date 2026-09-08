package worktree

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/testfixtures"
)

// TestBatchWriteFileEditsPayload asserts the engine's structured file_edits
// output: every applied op yields a per-path ground-truth record (create /
// modify kinds, real before/after sizes, exact unified diff) — computed from
// the in-memory snapshot pair the engine already holds, not from re-reads.
func TestBatchWriteFileEditsPayload(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "mod.txt"), []byte("line1\nline2\nline3\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	out, err := BatchWrite(BaseFor(base), WriteArgs{Writes: []Write{
		{Path: "new.txt", Mode: "create", Content: "package main\n"},
		{Path: "mod.txt", Mode: "overwrite", Content: "line1\nline2x\nline3\n"},
	}})
	if err != nil {
		t.Fatalf("BatchWrite err: %v", err)
	}

	// The output carries the human summary line + the machine payload line.
	if !strings.HasPrefix(out, "batch_write: applied 2 write(s)") {
		t.Fatalf("summary missing: %q", out)
	}
	var env struct {
		Summary   string `json:"summary"`
		FileEdits []struct {
			Path          string `json:"path"`
			Kind          string `json:"kind"`
			UnifiedDiff   string `json:"unified_diff"`
			SizeBefore    int64  `json:"size_before"`
			SizeAfter     int64  `json:"size_after"`
			ExistedBefore bool   `json:"existed_before"`
			SHA256After   string `json:"sha256_after,omitempty"`
		} `json:"file_edits"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(out, "\n", 2)[1]), &env); err != nil {
		t.Fatalf("payload parse: %v\noutput: %s", err, out)
	}
	if len(env.FileEdits) != 2 {
		t.Fatalf("expected 2 file_edits, got %d: %+v", len(env.FileEdits), env.FileEdits)
	}
	byPath := map[string]int{}
	for i, e := range env.FileEdits {
		byPath[e.Path] = i
	}

	// Create: whole-file hunk, before side absent (/dev/null header).
	ne := env.FileEdits[byPath["new.txt"]]
	if ne.Kind != "create" || ne.ExistedBefore || ne.SizeBefore != 0 {
		t.Fatalf("create record wrong: %+v", ne)
	}
	wantCreate := "--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1,1 @@\n+package main\n"
	if ne.UnifiedDiff != wantCreate {
		t.Fatalf("create diff:\n got %q\nwant %q", ne.UnifiedDiff, wantCreate)
	}
	if ne.SHA256After == "" {
		t.Fatalf("create record missing after sha256")
	}

	// Modify: exact ground-truth diff for the real snapshot pair.
	me := env.FileEdits[byPath["mod.txt"]]
	if me.Kind != "modify" || me.SizeBefore != 18 || me.SizeAfter != 19 {
		t.Fatalf("modify record wrong: %+v", me)
	}
	wantModify := "--- a/mod.txt\n+++ b/mod.txt\n@@ -1,3 +1,3 @@\n line1\n-line2\n+line2x\n line3\n"
	if me.UnifiedDiff != wantModify {
		t.Fatalf("modify diff:\n got %q\nwant %q", me.UnifiedDiff, wantModify)
	}
}

// TestBatchWriteFileEditsSharedVectors drives the engine through the SAME
// shared test vectors the ledger engine + TS renderer use: the file_edits
// unified_diff must byte-equal the fixture's expected diff.
func TestBatchWriteFileEditsSharedVectors(t *testing.T) {
	vecs, err := testfixtures.LoadFileEditVectors()
	if err != nil {
		t.Fatalf("load vectors: %v", err)
	}
	for _, v := range vecs {
		v := v
		if v.ExpectedSkipEntry || v.ExpectedBinary {
			continue // engine skips no-ops; binary diffs are empty by design
		}
		if v.ExpectedUnifiedDiff == nil || v.ExpectedTruncated {
			continue // only byte-exact, untruncated vectors are engine-assertable
		}
		if v.ExpectedKind == "delete" {
			// The engine's batch_write has no delete mode (create|overwrite|
			// edit|append) — deletion truth is produced by the ledger
			// engine's ComputeUnifiedDiff (asserted in internal/fileedit) and
			// git reconciliation, not by a write op.
			continue
		}
		t.Run(v.Name, func(t *testing.T) {
			base := t.TempDir()
			var writes []Write
			if v.Before != nil {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(base, v.Path)), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(base, v.Path), []byte(*v.Before), 0o644); err != nil {
					t.Fatalf("seed: %v", err)
				}
				writes = append(writes, Write{Path: v.Path, Mode: "overwrite", Content: orEmpty(v.After)})
			} else {
				writes = append(writes, Write{Path: v.Path, Mode: "create", Content: orEmpty(v.After)})
			}
			out, err := BatchWrite(BaseFor(base), WriteArgs{Writes: writes})
			if err != nil {
				t.Fatalf("BatchWrite err: %v", err)
			}
			env := parseFileEdits(t, out)
			if len(env) != 1 {
				t.Fatalf("expected exactly 1 file_edit, got %d", len(env))
			}
			if env[0].UnifiedDiff != *v.ExpectedUnifiedDiff {
				t.Fatalf("engine diff != fixture diff\n got %q\nwant %q", env[0].UnifiedDiff, *v.ExpectedUnifiedDiff)
			}
		})
	}
}

func orEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// parseFileEdits extracts the structured payload from a BatchWrite output.
func parseFileEdits(t *testing.T, out string) []struct {
	Path          string `json:"path"`
	Kind          string `json:"kind"`
	UnifiedDiff   string `json:"unified_diff"`
	SizeBefore    int64  `json:"size_before"`
	SizeAfter     int64  `json:"size_after"`
	ExistedBefore bool   `json:"existed_before"`
	SHA256After   string `json:"sha256_after,omitempty"`
} {
	t.Helper()
	var env struct {
		FileEdits []struct {
			Path          string `json:"path"`
			Kind          string `json:"kind"`
			UnifiedDiff   string `json:"unified_diff"`
			SizeBefore    int64  `json:"size_before"`
			SizeAfter     int64  `json:"size_after"`
			ExistedBefore bool   `json:"existed_before"`
			SHA256After   string `json:"sha256_after,omitempty"`
		} `json:"file_edits"`
	}
	lines := strings.SplitN(out, "\n", 2)
	if len(lines) < 2 {
		t.Fatalf("no payload line in output: %q", out)
	}
	if err := json.Unmarshal([]byte(lines[1]), &env); err != nil {
		t.Fatalf("payload parse: %v\noutput: %s", err, out)
	}
	return env.FileEdits
}

// TestSingleWriteSingleEditEmitFileEdits: the thin write/edit wrappers ride
// the same engine path and carry the same structured payload.
func TestSingleWriteSingleEditEmitFileEdits(t *testing.T) {
	base := t.TempDir()
	out, err := SingleWrite(BaseFor(base), SingleWriteArgs{FilePath: "w.txt", Content: "alpha\n"})
	if err != nil {
		t.Fatalf("SingleWrite err: %v", err)
	}
	env := parseFileEdits(t, out)
	if len(env) != 1 || env[0].Path != "w.txt" || env[0].Kind != "create" {
		t.Fatalf("write payload wrong: %+v", env)
	}

	out, err = SingleEdit(BaseFor(base), SingleEditArgs{FilePath: "w.txt", OldString: "alpha", NewString: "beta"})
	if err != nil {
		t.Fatalf("SingleEdit err: %v", err)
	}
	env = parseFileEdits(t, out)
	if len(env) != 1 || env[0].Kind != "modify" {
		t.Fatalf("edit payload wrong: %+v", env)
	}
	if !strings.Contains(env[0].UnifiedDiff, "-alpha\n+beta\n") {
		t.Fatalf("edit diff wrong: %q", env[0].UnifiedDiff)
	}
}

