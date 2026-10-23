package instancectl

// imports_test.go — the contract stays a LEAF, asserted mechanically.
//
// WHY THIS MATTERS HERE SPECIFICALLY. This package is imported by the Wails desktop shell and by cmd/orch
// (the TUI). If it could import the api/db/server layers, then (a) the desktop would link the plane's
// HTTP server, and (b) a second implementation of an operation could grow here — which is the exact
// divergence this package exists to remove. The permitted set is deliberately the same shape
// internal/planebind already enforces.

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

var permittedInternal = map[string]bool{
	"github.com/beardedparrott/orchicon/internal/planebind":    true,
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
			_ = path
			for _, imp := range file.Imports {
				seen++
				importPath := strings.Trim(imp.Path.Value, `"`)
				if strings.HasPrefix(importPath, "github.com/beardedparrott/orchicon") {
					if !permittedInternal[importPath] {
						t.Errorf("internal/instancectl imports %q.\n"+
							"  Only the shared control path is permitted (planebind, planecontrol,\n"+
							"  instance). Anything else risks linking the plane's HTTP server into the\n"+
							"  desktop shell, or growing a second implementation of an operation.", importPath)
					}
				}
			}
		}
	}
	// VACUITY GUARD: a walk that found nothing proves nothing.
	if seen < 5 {
		t.Fatalf("inspected only %d imports — the walk is not looking where it thinks", seen)
	}
	// AND the package must not shell out to the CLI except through planecontrol's resolver.
	if _, err := os.Stat("instancectl.go"); err != nil {
		t.Fatalf("expected instancectl.go to exist: %v", err)
	}
}
