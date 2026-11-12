package runtime

// AC 1 as a SOURCE guard: no `docker` invocation may re-enter the DAEMON tree.

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

// engineGuardBanned are the identifiers/forms the rename retired.
var engineGuardBanned = []string{
	"DockerBin",
	"d.docker(",
	".dockerFn",
}

// engineGuardAllowed names the cmd/orchicon files whose docker literals are KNOWN and owned by
// another slice of.
var engineGuardAllowed = map[string]string{
	"install.go":        " item 13: the host-install lifecycle still drives docker (containerEngineAvailable must learn podman there)",
	"install_bridge.go": " item 13: docker bridge-IP discovery moves to the engine probe in the install slice",
	"install_host.go":   " item 13: containerEngineAvailable still probes LookPath(\"docker\") and must learn podman there",
	"install_ports.go":  " item 13: port-conflict inspection on the install path",
	"restart.go":        " item 13: restarting the existing control-plane container",
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

// TestEngineGuardCatchesEachCallShape proves the guard is not vacuous: every shape it exists to
// forbid is detected, and the production (seam-based) shape is not.
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
