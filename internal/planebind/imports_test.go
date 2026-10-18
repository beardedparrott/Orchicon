package planebind

// imports_test.go — AC8, asserted mechanically: this package may import the standard
// library plus internal/planecontrol and internal/instance, and NOTHING else.
//
// That restriction IS the guarantee that control really goes through the shared
// mechanism: if this package could import the api/db/clients layers, a second control
// path could grow here. It also keeps the binding safe to compile inside the desktop
// shell on a branch where the plane's HTTP server is not being linked.

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

var permittedImports = map[string]bool{
	"github.com/beardedparrott/orchicon/internal/planecontrol": true,
	"github.com/beardedparrott/orchicon/internal/instance":     true,
}

func TestPackageImportsOnlyTheSharedControlPath(t *testing.T) {
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
				importPath := strings.Trim(imp.Path.Value, `"`)
				if permittedImports[importPath] {
					continue
				}
				if !isStdlibPath(importPath) {
					t.Errorf("%s imports %q — planebind may import only the standard library plus the shared "+
						"control mechanism (internal/planecontrol) and the instance table (internal/instance)",
						filepath.Base(path), importPath)
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("no imports were inspected — the test would pass vacuously")
	}
}

// isStdlibPath reports whether an import path is the standard library (its first
// segment has no dot), the same rule internal/planecontrol's imports test uses.
func isStdlibPath(importPath string) bool {
	first := importPath
	if i := strings.IndexByte(importPath, '/'); i >= 0 {
		first = importPath[:i]
	}
	return !strings.Contains(first, ".")
}
