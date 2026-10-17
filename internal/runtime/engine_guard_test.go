package runtime

// AC 1 as a SOURCE guard: no `docker` invocation may re-enter the DAEMON tree.
//
// Renaming DockerBin → Engine and d.docker → d.engine is a one-way door only if
// something keeps it shut. The engine binary is resolved in ONE place
// (engine.go, via Engine.binary()) and executed in ONE place (Daemon.engine /
// exec.Command(d.engineBin(), …)); this walks internal/runtime and fails on any
// call that names the engine binary as a string literal — exec.Command,
// exec.CommandContext or exec.LookPath — or on the retired identifiers, so a
// later hurried change cannot quietly reintroduce a second engine identity.
//
// It is an AST scan, not a text match, because the shape being caught is
// "the ENGINE BINARY as a literal argument", and the argument position varies
// with the call (CommandContext puts it second). A regex over source either
// misses `exec.CommandContext(ctx, "docker")` or false-positives on
// `exec.Command(d.engineBin(), "exec", …)`; the AST sees exactly which argument
// is the binary.
//
// cmd/orchicon is walked too, with a SHORT, EXPLICIT allowlist: the host-install
// / control-plane lifecycle commands still drive docker by name. Those are
// ADR-0013 §12 item 13's slice (the install task) — pinning them here as known
// exceptions is deliberate, so the residual is visible rather than forgotten,
// and so any NEW literal in cmd/orchicon still fails this test.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// engineBinaryNames are the engine binaries a literal may not name.
var engineBinaryNames = map[string]bool{"docker": true, "podman": true}

// engineGuardBanned are the identifiers/forms the rename retired. They are
// checked literally (no regex) so a match is unambiguous. `d.docker(` includes
// its paren so a `dockerSomethingElse` identifier cannot trip it.
var engineGuardBanned = []string{
	"DockerBin",
	"d.docker(",
	".dockerFn",
}

// engineGuardAllowed names the cmd/orchicon files whose docker literals are
// KNOWN and owned by another slice of ADR-0013. internal/runtime is NOT allowed
// any entry — the daemon must go through Engine, full stop.
var engineGuardAllowed = map[string]string{
	"install.go":        "ADR-0013 §12 item 13: the host-install lifecycle still drives docker (containerEngineAvailable must learn podman there)",
	"install_bridge.go": "ADR-0013 §12 item 13: docker bridge-IP discovery moves to the engine probe in the install slice",
	"install_host.go":   "ADR-0013 §12 item 13: containerEngineAvailable still probes LookPath(\"docker\") and must learn podman there",
	"install_ports.go":  "ADR-0013 §12 item 13: port-conflict inspection on the install path",
	"restart.go":        "ADR-0013 §12 item 13: restarting the existing control-plane container",
}

// engineLiteralCalls returns every call in src that passes an engine binary NAME
// as a literal to exec.Command / exec.CommandContext / exec.LookPath.
func engineLiteralCalls(t *testing.T, path string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "exec" {
			return true
		}
		argIdx := -1
		switch sel.Sel.Name {
		case "Command", "LookPath":
			argIdx = 0
		case "CommandContext":
			argIdx = 1 // (ctx, name, args…)
		default:
			return true
		}
		if argIdx >= len(call.Args) {
			return true
		}
		lit, ok := call.Args[argIdx].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		name, err := strconv.Unquote(lit.Value)
		if err != nil || !engineBinaryNames[name] {
			return true
		}
		found = append(found, "exec."+sel.Sel.Name+"(…, "+lit.Value+"…)")
		return true
	})
	return found
}

func TestNoDirectEngineInvocation(t *testing.T) {
	roots := []struct {
		dir     string
		daemon  bool // the daemon tree: no exceptions at all
		checked *int
	}{
		{".", true, new(int)},
		{filepath.Join("..", "..", "cmd", "orchicon"), false, new(int)},
	}
	for _, root := range roots {
		entries, err := os.ReadDir(root.dir)
		if err != nil {
			t.Fatalf("read %s: %v", root.dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			if reason, ok := engineGuardAllowed[e.Name()]; ok && !root.daemon {
				t.Logf("known residual docker usage in %s — %s", filepath.Join(root.dir, e.Name()), reason)
				continue
			}
			path := filepath.Join(root.dir, e.Name())
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			*root.checked++
			for _, call := range engineLiteralCalls(t, path, src) {
				t.Errorf("%s: engine binary named literally (%s) — resolve it through Engine/engineBin() instead", path, call)
			}
			for _, banned := range engineGuardBanned {
				if strings.Contains(string(src), banned) {
					t.Errorf("%s: retired identifier %q — the engine seam is Engine / d.engine / engineFn", path, banned)
				}
			}
		}
		if *root.checked == 0 {
			t.Fatalf("the guard checked no files under %s — its roots are wrong and it would pass vacuously", root.dir)
		}
	}
}

// TestEngineGuardCatchesEachCallShape proves the guard is not vacuous: every
// shape it exists to forbid is detected, and the production (seam-based) shape
// is not. A guard whose matcher quietly stops matching is worse than none.
func TestEngineGuardCatchesEachCallShape(t *testing.T) {
	caught := []string{
		"package x\nimport \"os/exec\"\nfunc f() { exec.Command(\"docker\", \"ps\") }\n",
		"package x\nimport \"context\"\nimport \"os/exec\"\nfunc f(ctx context.Context) { exec.CommandContext(ctx, \"podman\", \"ps\") }\n",
		"package x\nimport \"os/exec\"\nfunc f() { exec.LookPath(\"docker\") }\n",
	}
	for _, src := range caught {
		if got := engineLiteralCalls(t, "synthetic.go", []byte(src)); len(got) == 0 {
			t.Errorf("guard missed an engine literal: %s", strings.TrimSpace(src))
		}
	}
	allowed := "package x\nimport \"os/exec\"\nfunc f(bin string) { exec.Command(bin, \"exec\", \"-i\", \"n\") ; exec.LookPath(bin) }\n"
	if got := engineLiteralCalls(t, "synthetic.go", []byte(allowed)); len(got) != 0 {
		t.Errorf("guard false-positived on the seam-based shape: %v", got)
	}
}
