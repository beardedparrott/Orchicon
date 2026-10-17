package planecontrol

// imports_test.go — AC8 and AC3, asserted mechanically: this package imports NO package that could drag the
// plane's HTTP server in, so the TUI (`cmd/orch`) and a Wails app can depend on it; and no second copy of the
// lifecycle re-exec is left behind in cmd/orchicon.
//
// "STDLIB ONLY" IS THE RULE, WITH ONE NAMED EXCEPTION — internal/containerengine, which is itself a
// stdlib-only leaf (TestPermittedInternalDependencyIsStdlibOnly re-checks that, so the property holds
// transitively rather than by promise). It is permitted because it is the ONE truth for "is there an
// engine, and which binary is it": the installer and the plane's /enginez ask it too, and a private probe
// here would eventually answer differently and describe an engine that is not there. What must NEVER be
// importable from here is the plane's HTTP server, and that is what the test still forbids.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackageImportsOnlyTheStandardLibrary(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse package dir: %v", err)
	}
	seen := 0
	for name, pkg := range pkgs {
		if strings.HasSuffix(name, "_test") {
			continue
		}
		for path, file := range pkg.Files {
			for _, imp := range file.Imports {
				seen++
				importPath := strings.Trim(imp.Path.Value, "\"")
				if importPath == permittedInternalImport {
					continue
				}
				// A standard-library path has NO DOT in its first segment; any module path does.
				if !isStdlibPath(importPath) {
					t.Errorf("%s imports %q — this package must stay a leaf so the TUI and the "+
						"desktop app can use it without the plane's HTTP server", filepath.Base(path), importPath)
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("no imports were inspected — the test would pass vacuously")
	}
}

// permittedInternalImport is the ONE module path this package may import. See the file header for why.
const permittedInternalImport = "github.com/beardedparrott/orchicon/internal/containerengine"

// isStdlibPath reports whether an import path is the standard library (its first segment has no dot).
func isStdlibPath(importPath string) bool {
	first := importPath
	if i := strings.IndexByte(importPath, '/'); i >= 0 {
		first = importPath[:i]
	}
	return !strings.Contains(first, ".")
}

// TestPermittedInternalDependencyIsStdlibOnly keeps the exception from becoming a loophole: the permitted
// import must itself be a stdlib-only leaf, so AC8 holds TRANSITIVELY — no chain of allowed imports can
// reach the plane's HTTP server.
func TestPermittedInternalDependencyIsStdlibOnly(t *testing.T) {
	dir := filepath.Join("..", "containerengine")
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	seen := 0
	for name, pkg := range pkgs {
		if strings.HasSuffix(name, "_test") {
			continue
		}
		for path, file := range pkg.Files {
			for _, imp := range file.Imports {
				seen++
				importPath := strings.Trim(imp.Path.Value, "\"")
				if !isStdlibPath(importPath) {
					t.Errorf("%s imports %q — the permitted dependency must itself be stdlib-only, or the "+
						"exception above stops guaranteeing anything", filepath.Base(path), importPath)
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("no imports were inspected — the test would pass vacuously")
	}
}

func TestNoLifecycleLogicLeftInCmdOrchicon(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "cmd", "orchicon", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob cmd/orchicon: %v (%d files)", err, len(files))
	}
	needles := []string{`"serve", "--detach"`, `"serve", "--stop"`}
	for _, path := range files {
		base := filepath.Base(path)
		if strings.HasSuffix(base, "_test.go") || base == "plane_control.go" {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, needle := range needles {
			if strings.Contains(string(b), needle) {
				t.Errorf("%s contains %s — the lifecycle re-exec must exist ONLY in internal/planecontrol "+
					"(one implementation, not two)", base, needle)
			}
		}
	}
}
