package runtime

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

// TestAdapterCLINeverBaked is the ADAPTER-NEUTRAL IMAGE INVARIANT guard
// (AC 6): no image built from this repo's Dockerfiles may BAKE an adapter
// CLI. The invariant is already true by design — deploy/runtime/Dockerfile
// and deploy/container/Dockerfile deliberately bind-MOUNT the operator's
// host adapter installs at runtime (license redistributability: Claude
// Code's terms forbid bundling it) — and this guard makes a silent
// regression impossible.
//
// Two properties make it regression-proof rather than a snapshot:
//
//	(a) THE KIND LIST IS LIVE, not hand-maintained: it comes from the
//	    builtin provider catalog (internal/adapter/providers.go via
//	    adapter.BuiltinAdapterKinds), which already declares "claude" even
//	    though only opencode/orchicon are dispatcher-registered. Every
//	    declared kind must be CLASSIFIED by adapterInstalls (the same
//	    declaration the daemon mounts from, bootprofile.go), so a newly
//	    declared adapter — codex, or whatever is next — FAILS this test
//	    until it is classified. A hand-written list would silently miss it,
//	    which is exactly the failure this guard exists to catch (AC 6b).
//
//	    The classification is asserted rather than the needles, so the guard
//	    cannot pass merely because a new kind contributes no needle.
//
//	(b) THE SCAN IS SCOPED: only files whose BASE NAME starts with
//	    "Dockerfile" under deploy/ are read. deploy/container/{orch,orchicon}
//	    are COMMITTED COMPILED BINARIES (67 MB / 97 MB) — opening or grepping
//	    them would report every needle as present. This walk never touches
//	    them, so that failure mode is structurally impossible.
//
// The orchicon binary's own rule is UNCHANGED and deliberately NOT this
// guard's job: the product binary IS legitimately baked into the
// control-plane image (deploy/container/Dockerfile `COPY orchicon ...`)
// because it is the product, not a third-party adapter CLI. Only adapter
// kinds contribute needles.
func TestAdapterCLINeverBaked(t *testing.T) {
	const fakeHome = "/home/orchicon-host"

	kindSet := adapter.BuiltinAdapterKinds()
	if len(kindSet) < 3 {
		t.Fatalf("the builtin provider catalog declared only %d adapter kinds — the LIVE kind source (internal/adapter/providers.go) shrank; this guard derives its coverage from it and would silently under-cover", len(kindSet))
	}
	kinds := make([]string, 0, len(kindSet))
	for k := range kindSet {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)

	// An undeclared kind must be reported unclassified: the guard's
	// classification check must be able to FAIL, not vacuously pass.
	if _, declared := adapterInstallPaths(fakeHome, "codex-unclassified-probe"); declared {
		t.Fatal("an undeclared kind reported declared=true — the classification check is vacuous")
	}

	// (a) Totality over the LIVE catalog: every declared kind must be
	// classified. This is what covers the NEXT adapter by construction.
	needles := make([]string, 0, len(kinds))
	needleKind := map[string]string{}
	for _, kind := range kinds {
		paths, declared := adapterInstallPaths(fakeHome, kind)
		if !declared {
			t.Fatalf("adapter kind %q is declared by the builtin provider catalog but NOT classified in adapterInstalls (internal/runtime/bootprofile.go). Classify it — its host install paths, or none for a bind-mounted product binary — so this guard covers it. An unclassified kind is how the NEXT adapter slips past the mount-never-bake invariant", kind)
		}
		for _, n := range adapterBakeNeedles(kind, paths) {
			if _, dup := needleKind[n]; dup {
				continue
			}
			needleKind[n] = kind
			needles = append(needles, n)
		}
	}
	sort.Strings(needles)
	if len(needles) == 0 {
		t.Fatal("no bake needles derived from the catalog — the guard would pass vacuously")
	}

	// (b) Layer scan: Dockerfiles ONLY.
	files := dockerfilesUnder(t, filepath.Join("..", "..", "deploy"))
	if len(files) < 3 {
		t.Fatalf("guard found only %d Dockerfile(s) under deploy/ (%v) — the discovery walk is broken and the guard would pass vacuously", len(files), files)
	}
	scanned := 0
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		scanned++
		for i, logical := range logicalInstructions(string(body)) {
			for _, n := range needles {
				if strings.Contains(logical, n) {
					t.Fatalf("%s (instruction #%d) bakes adapter %q (needle %q): adapter CLIs must be MOUNTED from the operator's host install at runtime, never baked into an image layer — see deploy/runtime/Dockerfile and deploy/container/Dockerfile\n\tinstruction: %s",
						f, i+1, needleKind[n], n, strings.TrimSpace(logical))
				}
			}
		}
	}
	t.Logf("scanned %d Dockerfile(s) for %d adapter needle(s) across %d catalog kind(s)", scanned, len(needles), len(kinds))
}

// adapterBakeNeedles derives the forbidden substrings for one adapter kind
// from the SHARED install-path declaration (adapterInstallPaths — the same
// source the daemon mounts from), so the mounted set and the forbidden-bake
// set can never drift. A kind with no install paths (orchicon: the product
// binary, bind-mounted by the daemon) contributes NO needles.
func adapterBakeNeedles(kind string, installPaths []string) []string {
	if len(installPaths) == 0 {
		return nil
	}
	out := []string{kind}
	for _, p := range installPaths {
		b := filepath.Base(p)
		out = append(out, b, strings.TrimPrefix(b, "."))
	}
	seen := map[string]struct{}{}
	uniq := out[:0]
	for _, n := range out {
		n = strings.TrimSpace(n)
		if n == "" || len(n) < 3 {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		uniq = append(uniq, n)
	}
	return uniq
}

// dockerfilesUnder returns every file under root whose base name starts with
// "Dockerfile", skipping nothing else — the committed binaries in
// deploy/container/ have unrelated names and are therefore never opened.
func dockerfilesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), "Dockerfile") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

// logicalInstructions returns the Dockerfile INSTRUCTION lines (comments and
// blanks dropped), with line continuations joined, so a needle cannot hide on
// a backslash-continued line. `#` inside an instruction is preserved — only
// a line whose first non-space character is `#` is a comment.
func logicalInstructions(body string) []string {
	var out []string
	var cur string
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if cur == "" {
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
		}
		if strings.HasSuffix(trimmed, "\\") {
			cur += strings.TrimSuffix(trimmed, "\\") + " "
			continue
		}
		cur += trimmed
		out = append(out, cur)
		cur = ""
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
