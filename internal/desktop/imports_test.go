package desktop

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// The shell must stay a LEAF CLIENT: importing the plane's HTTP stack (or its DB,
// or its scheduler) would make the desktop app depend on the server it talks to
// and would drag CGO-adjacent weight into a package that is otherwise testable
// without a webview. Same idiom as internal/planecontrol/imports_test.go.
var forbiddenImports = []string{
	"github.com/beardedparrott/orchicon/internal/api",
	"github.com/beardedparrott/orchicon/internal/db",
	"github.com/beardedparrott/orchicon/internal/scheduler",
}

func TestShellImportsNoPlanePackages(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse package dir: %v", err)
	}
	var seen int
	for name, pkg := range pkgs {
		if strings.HasSuffix(name, "_test") {
			continue
		}
		for path, file := range pkg.Files {
			for _, imp := range file.Imports {
				seen++
				importPath := strings.Trim(imp.Path.Value, `"`)
				for _, forbidden := range forbiddenImports {
					if importPath == forbidden || strings.HasPrefix(importPath, forbidden+"/") {
						t.Errorf("%s imports %q — the desktop shell must not depend on the plane's server packages",
							filepath.Base(path), importPath)
					}
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("no imports were inspected — the test would pass vacuously")
	}
}
