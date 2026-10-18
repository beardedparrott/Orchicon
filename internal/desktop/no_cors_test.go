package desktop

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// AC5: the origin arrangement must not quietly become a CORS arrangement.
//
// The shell serves the SPA same-origin and proxies the API, so NO CORS header is
// needed anywhere — and adding one would widen what the plane accepts. The
// behavioural half is TestShellAddsNoCORSHeaders (proxy_test.go); this half
// asserts no source file in the shell can grow one.
func TestShellSourcesAddNoCORS(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse package dir: %v", err)
	}
	var seen int
	for name, pkg := range pkgs {
		if strings.HasSuffix(name, "_test") {
			continue
		}
		for path := range pkg.Files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			seen++
			b, err := readSource(filepath.Base(path))
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			if strings.Contains(strings.ToLower(b), "access-control-"+"allow") {
				t.Errorf("%s sets an Access-Control-Allow-* header — the shell is same-origin with the SPA "+
					"and must not add CORS", filepath.Base(path))
			}
		}
	}
	if seen == 0 {
		t.Fatal("no files were inspected — the test would pass vacuously")
	}
}
