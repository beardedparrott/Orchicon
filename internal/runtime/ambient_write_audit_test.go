package runtime

// ambient_write_audit_test.go — THE CLASS, NOT THE INSTANCE.
//
// Three incidents in one day had one root cause: a path resolved from the PROCESS ENVIRONMENT inside a
// test, or a resolver, that then WRITES.
//
//	1. GIT_DIR leaked into `git -C` — commits landed in, and refs were clobbered on, a real repo.
//	2. ORCHICON_CLAUDE_PROJECT_DIR leaked into a boundary test — every worker's gate reported red.
//	3. XDG_DATA_HOME leaked into opencode's store resolver — `this is not a sqlite database` (29 bytes)
//	   was written over four live stores, and every opencode serve and recovery pass has failed since.
//
// Each was found one round trip at a time, by a report. This file is the alternative: it audits the
// SOURCE for the shape, so the fourth instance is caught here.
//
// It is a source audit for the same reason the repo's engine_path_audit_test.go is one: the property is
// "which function does this call site use", and that is visible in the source, where a NEW call site
// is — not in any behavioural test that would have to guess at it.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// thePlaneStateRoot is the ONE directory a test must never write into: it holds the serving plane's
// live state — the opencode stores, the instance data, the backups.
func thePlaneStateRoot(home string) string {
	return filepath.Join(home, ".local", "share", "orchicon")
}

// TestNoTestResolvesAPublicWritePathFromTheEnvironment is the sweep.
//
// It flags a test file that BOTH reads ambient environment into a path-like variable AND writes to a
// path derived without a t.TempDir() anchor. That is the shape of all three incidents. A test that
// reads an env var purely as a boolean gate, or that writes only under t.TempDir(), is fine and is not
// flagged.
func TestNoTestResolvesAPublicWritePathFromTheEnvironment(t *testing.T) {
	root := repoRoot(t)

	// Resolvers whose answer depends on the PROCESS environment, not their argument. A test that writes
	// to a path obtained from one of these can reach the operator's real state.
	ambientResolvers := []string{
		"openCodeDBPath(",
		"OpenCodeStoreDir(",
		"openCodeAuthPath(",
		"defaultDataDirFor()",
		"os.UserHomeDir()",
	}

	var flagged []string
	err := filepath.Walk(filepath.Join(root, "internal"), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		// This file names these symbols in order to audit them.
		if filepath.Base(p) == "ambient_write_audit_test.go" {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		src := string(b)

		writes := strings.Contains(src, "os.WriteFile(") || strings.Contains(src, "os.MkdirAll(") ||
			strings.Contains(src, "sql.Open(") || strings.Contains(src, "os.Rename(")
		if !writes {
			return nil
		}
		decl := hazardDeclarations(src, ambientResolvers)
		if len(decl) == 0 {
			return nil
		}
		// The write need not be on the same line: the incident was `p := openCodeDBPath(home)` followed by
		// `os.WriteFile(p, ...)` TWO LINES DOWN. So a hazard is flagged when a write call in the SAME FILE
		// names a variable that an ambient resolver initialised.
		isTempAnchored := strings.Contains(src, "t.TempDir()")
		for _, d := range decl {
			if !writesTheVariable(src, d.name) {
				continue
			}
			// Being temp-anchored does NOT clear it: the incident's home WAS a t.TempDir(). What clears it
			// is not handing the ambient resolver's answer to a write at all.
			flagged = append(flagged, fmt.Sprintf("%s: %s (written to by a call naming %q)",
				filepath.ToSlash(p), strings.TrimSpace(d.line), d.name))
		}
		_ = isTempAnchored
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for _, f := range flagged {
		t.Errorf("%s\n  This resolves a path from the PROCESS ENVIRONMENT and writes to it. Build the path "+
			"from a t.TempDir() and confine it (storePathUnderStateRoot) before writing — the shape that "+
			"put a 29-byte stub over four live opencode stores.", f)
	}
	if len(flagged) > 0 {
		t.Logf("the plane's state root on this host is %s — nothing in a test may write there",
			thePlaneStateRoot("/home/<operator>"))
	}
}

// TestTheStateRootGuardCoversEveryPlaneStore pins the BREADTH of the confinement: not just opencode.
//
// THE LESSON THAT MADE THIS NECESSARY. The stub was written into a store that BOTH the opencode serve
// and the recovery engine resolve, so a single bad write disabled serving and recovery at once. A guard
// that only knew about `opencode/` would have missed half the blast radius.
func TestTheStateRootGuardCoversEveryPlaneStore(t *testing.T) {
	home := t.TempDir()
	root := thePlaneStateRoot(home)
	for _, sub := range []string{
		filepath.Join("opencode", "opencode", "opencode.db"),
		filepath.Join("opencode-ask", "opencode", "opencode.db"),
		filepath.Join("opencode", "serve-abc123", "opencode", "opencode.db"),
		filepath.Join("backups", "snapshot.dump"),
		"serve", "logs", "orchicon.log",
	} {
		p := filepath.Join(root, sub)
		if !storePathUnderStateRoot(p, root) {
			t.Errorf("%q is inside the plane's state root but the confinement guard rejects it — the "+
				"guard is narrower than the blast radius", sub)
		}
	}
	if storePathUnderStateRoot("/tmp/unrelated/opencode.db", root) {
		t.Error("the guard accepted a path outside the state root")
	}
}

// hazardDecl is one line that initialises a variable from an ambient resolver.
type hazardDecl struct {
	name string // the variable written to
	line string // the source line, for the report
}

// hazardDeclarations finds a variable whose value DERIVES from an ambient resolver, directly or through
// filepath.Join — `p := openCodeDBPath(home)`, and also `home, _ := os.UserHomeDir()` followed by
// `p := filepath.Join(home, ...)`. The second shape is the same hazard: the write lands in the
// operator's state either way.
func hazardDeclarations(src string, resolvers []string) []hazardDecl {
	var out []hazardDecl
	for _, line := range strings.Split(src, "\n") {
		code := line
		if i := strings.Index(code, "//"); i >= 0 {
			code = code[:i]
		}
		for _, res := range resolvers {
			if !strings.Contains(code, res) {
				continue
			}
			lhs, rhs, ok := strings.Cut(code, ":=")
			if !ok {
				if l, r, ok2 := strings.Cut(code, "="); ok2 && !strings.Contains(l, "==") &&
					!strings.Contains(l, "!=") && !strings.HasPrefix(strings.TrimSpace(l), "if") {
					lhs, rhs = l, r
				} else {
					continue
				}
			}
			if strings.Contains(rhs, res) {
				if name := firstIdentifier(lhs); name != "" {
					out = append(out, hazardDecl{name: name, line: line})
				}
			}
			// TAINT PROPAGATION: a variable built from an already-tainted one is tainted too, which is how
			// `home, _ := os.UserHomeDir()` becomes `p := filepath.Join(home, ...)`.
			for _, tainted := range ambientTainted(src, resolvers) {
				if tainted == res {
					continue
				}
				if strings.Contains(rhs, tainted) {
					if name := firstIdentifier(lhs); name != "" {
						out = append(out, hazardDecl{name: name, line: line})
					}
				}
			}
		}
	}
	return out
}

// ambientTainted returns every variable name in src that derives from an ambient resolver, iterating so
// a chain (home -> dir -> path) is fully tainted.
func ambientTainted(src string, resolvers []string) []string {
	tainted := map[string]struct{}{}
	for pass := 0; pass < 6; pass++ {
		grew := false
		for _, line := range strings.Split(src, "\n") {
			code := line
			if i := strings.Index(code, "//"); i >= 0 {
				code = code[:i]
			}
			lhs, rhs, ok := strings.Cut(code, ":=")
			if !ok {
				continue
			}
			hit := false
			for _, res := range resolvers {
				if strings.Contains(rhs, res) {
					hit = true
					break
				}
			}
			if !hit {
				for name := range tainted {
					if strings.Contains(rhs, name) {
						hit = true
						break
					}
				}
			}
			if !hit {
				continue
			}
			if name := firstIdentifier(lhs); name != "" {
				if _, seen := tainted[name]; !seen {
					tainted[name] = struct{}{}
					grew = true
				}
			}
		}
		if !grew {
			break
		}
	}
	names := make([]string, 0, len(tainted))
	for n := range tainted {
		names = append(names, n)
	}
	return names
}

// writesTheVariable reports whether the file passes name to a write call.
func writesTheVariable(src, name string) bool {
	if name == "" {
		return false
	}
	for _, call := range []string{"os.WriteFile(", "os.MkdirAll(", "sql.Open(", "os.Rename(", "os.Create(", "os.RemoveAll("} {
		for i := 0; i < len(src); {
			j := strings.Index(src[i:], call)
			if j < 0 {
				break
			}
			start := i + j + len(call)
			end := start
			for end < len(src) && src[end] != ')' {
				end++
			}
			if strings.Contains(src[start:end], name) {
				return true
			}
			i = start
		}
	}
	return false
}

// firstIdentifier takes the first name from an assignment's left-hand side.
func firstIdentifier(lhs string) string {
	lhs = strings.TrimSpace(lhs)
	if lhs == "" {
		return ""
	}
	if i := strings.IndexAny(lhs, ","); i >= 0 {
		lhs = lhs[:i]
	}
	lhs = strings.TrimSpace(lhs)
	lhs = strings.TrimPrefix(lhs, "var ")
	var b strings.Builder
	for _, r := range lhs {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		break
	}
	return b.String()
}

// repoRoot walks up to the module root, so the audit does not depend on the test's cwd.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find the module root")
	return ""
}
