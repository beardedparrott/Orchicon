package planecontrol

// imports_test.go — AC8 and AC3, asserted mechanically: this package imports the STANDARD LIBRARY ONLY, so the
// TUI (`cmd/orch`) and a Wails app can depend on it without pulling in the plane's HTTP server; and no second
// copy of the lifecycle re-exec is left behind in cmd/orchicon.

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
				// A standard-library path has NO DOT in its first segment; any module path does.
				first := importPath
				if i := strings.IndexByte(importPath, '/'); i >= 0 {
					first = importPath[:i]
				}
				if strings.Contains(first, ".") {
					t.Errorf("%s imports %q — this package must stay a stdlib-only leaf so the TUI and the "+
						"desktop app can use it without the plane's HTTP server", filepath.Base(path), importPath)
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
