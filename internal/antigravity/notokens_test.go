package antigravity_test

// notokens_test.go — THE NO-TOKEN-HANDLING GUARANTEE, enforced rather than promised.
//
// Orchicon launches Google's own agy binary and never touches Google credentials: no reading
// ~/.gemini/**, no copying oauth_creds.json, no OS-keyring lookups. That promise is the whole
// reason an operator can enable the personal sign-in path at all, so it is checked mechanically on
// every `go test` run: any literal naming a Google credential path, and any keyring/credential-store
// API, in internal/antigravity (including its subpackages) fails the build.
//
// WHY AN AST WALK AND NOT A GREP. A grep over source also matches the strings this package is
// REQUIRED to contain — the operator-facing refusal message says "host keyring", and the doc
// comments explain the limitation. Comments and the required messages must stay legal while an
// actual filepath.Join(home, ".gemini", "oauth_creds.json") must not; only a parse can tell the two
// apart.
//
// The scan is a pure function over file contents so it can be self-tested with a PLANTED source
// (see TestNoTokenHandlingDetectsAPlantedRead): a guard nobody has seen fail is a guard nobody
// knows works.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tokenFinding is one forbidden construct, located for the failure message.
type tokenFinding struct {
	File string
	Line int
	What string
}

func (f tokenFinding) String() string { return fmt.Sprintf("%s:%d: %s", f.File, f.Line, f.What) }

// forbiddenImportSubstrings are the credential-store client libraries. Matching on the import PATH
// catches every alias: a package aliased to kr still imports go-keyring.
var forbiddenImportSubstrings = []string{
	"keyring", "libsecret", "secret-service", "godbus/dbus", "wincred", "go-credential",
}

// forbiddenLiteralSubstrings are the credential PATHS and CLI accessors. These cannot be mentioned
// legitimately anywhere in this package: the adapter has no business naming them.
var forbiddenLiteralSubstrings = []string{
	".gemini", "oauth_creds", "oauth-creds", "secret-tool", "find-generic-password", "security find-generic",
}

// forbiddenSelectors are package-level selectors that reach a keyring/credential API.
var forbiddenSelectors = map[string]bool{
	"keyring": true, "keychain": true, "wincred": true, "secretservice": true, "credread": true, "credwrite": true,
}

// scanForTokenHandling reports every credential-store access in the given Go sources.
func scanForTokenHandling(files map[string][]byte) ([]tokenFinding, error) {
	var out []tokenFinding
	fset := token.NewFileSet()
	for name, src := range files {
		file, err := parser.ParseFile(fset, name, src, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		line := func(pos token.Pos) int { return fset.Position(pos).Line }
		for _, imp := range file.Imports {
			path := strings.ToLower(strings.Trim(imp.Path.Value, "\""))
			for _, bad := range forbiddenImportSubstrings {
				if strings.Contains(path, bad) {
					out = append(out, tokenFinding{name, line(imp.Pos()), "imports a keyring/credential-store client: " + path})
				}
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.BasicLit:
				if node.Kind != token.STRING {
					return true
				}
				val := strings.ToLower(node.Value)
				for _, bad := range forbiddenLiteralSubstrings {
					if strings.Contains(val, bad) {
						out = append(out, tokenFinding{name, line(node.Pos()), "names a Google credential path or keyring CLI: " + node.Value})
					}
				}
			case *ast.SelectorExpr:
				if id, ok := node.X.(*ast.Ident); ok && forbiddenSelectors[strings.ToLower(id.Name)] {
					out = append(out, tokenFinding{name, line(node.Pos()), "calls a credential-store API: " + id.Name + "." + node.Sel.Name})
				}
			}
			return true
		})
	}
	return out, nil
}

// goSourcesUnder collects every non-test .go file in the package tree. Test files are excluded
// because a test may legitimately SPELL a forbidden path in order to plant it; testdata is not Go
// source at all.
func goSourcesUnder(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		files[path] = b
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(files) == 0 {
		t.Fatalf("no Go sources found under %s - the guard would pass vacuously", root)
	}
	return files
}

// AC5: the package reads no Google credential file and calls no keyring API.
func TestNoTokenHandling(t *testing.T) {
	findings, err := scanForTokenHandling(goSourcesUnder(t, "."))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, f := range findings {
		t.Error(f.String())
	}
	if len(findings) > 0 {
		t.Fatalf("%d credential-handling construct(s) found in internal/antigravity: the adapter must only LAUNCH agy, never read ~/.gemini/** or a keyring", len(findings))
	}
}

// The guard must actually catch the thing it exists for. This is the same source the acceptance
// criteria plant on disk, scanned in memory - so the detection is proven even when the tree is
// clean.
func TestNoTokenHandlingDetectsAPlantedRead(t *testing.T) {
	planted := strings.Join([]string{
		"package antigravity",
		"",
		"import (",
		`	"os"`,
		`	"path/filepath"`,
		")",
		"",
		"func stolen(home string) ([]byte, error) {",
		`	return os.ReadFile(filepath.Join(home, ".gemini", "oauth_creds.json"))`,
		"}",
	}, "\n")
	findings, err := scanForTokenHandling(map[string][]byte{"planted.go": []byte(planted)})
	if err != nil {
		t.Fatalf("scan planted: %v", err)
	}
	if len(findings) == 0 {
		t.Fatal("the guard did not detect a read of a .gemini/oauth_creds.json file")
	}

	// A KEYRING read must be caught too: the personal credential lives there on macOS and Linux.
	keyringSrc := strings.Join([]string{
		"package antigravity",
		"",
		`import "github.com/zalando/go-keyring"`,
		"",
		`func steal() (string, error) { return keyring.Get("gemini", "default") }`,
	}, "\n")
	kf, err := scanForTokenHandling(map[string][]byte{"keyring.go": []byte(keyringSrc)})
	if err != nil {
		t.Fatalf("scan keyring: %v", err)
	}
	if len(kf) == 0 {
		t.Fatal("the guard did not detect a go-keyring import/read")
	}

	// ...and it must NOT flag the strings this package is REQUIRED to contain: the operator-facing
	// refusal says "host keyring", and the ToS notice is legal text.
	legal := strings.Join([]string{
		"package antigravity",
		"",
		`const refusal = "personal Google sign-in lives in the host keyring and cannot be used inside a runtime container"`,
		`const notice = "Antigravity personal sign-in is used through Google's own agy binary."`,
	}, "\n")
	lf, err := scanForTokenHandling(map[string][]byte{"legal.go": []byte(legal)})
	if err != nil {
		t.Fatalf("scan legal: %v", err)
	}
	for _, f := range lf {
		t.Errorf("the guard flagged operator-facing text: %s", f)
	}
}
