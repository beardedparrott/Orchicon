package main

// wailsjs_generated_ignored_test.go — WAILS' GENERATED BINDINGS MUST NOT BE LINTED OR COMMITTED.
//
// THE BUG THIS PINS, from the operator's own rebuild:
//
//	$ make rebuild-dev
//	<repo>/frontend/wailsjs/go/models.ts
//	   1:8   error  ES2015 module syntax is preferred over namespaces  @typescript-eslint/no-namespace
//	  11:32  error  Unexpected any. Specify a different type           @typescript-eslint/no-explicit-any
//	  ... (72 problems, 16 errors) ...
//	make[2]: *** [Makefile:715: fe-lint] Error 1
//
// The files were generated minutes earlier by `wails build`, which writes
// <frontend:dir>/wailsjs — our frontend SOURCE tree — and offers no setting to
// put them anywhere else. `npm run lint` is `eslint .`, so the next lint run
// walks Wails' own reflection code and fails on `any` and `namespace`: style
// complaints about generated code nobody here wrote, blocking the whole rebuild.
//
// WHY THIS IS ASSERTED AND NOT JUST FIXED. The fix is two list entries, and
// both are the kind of list a future change edits without noticing the coupling:
//
//   - frontend/eslint.config.js `ignores` decides what `eslint .` reads.
//   - .gitignore decides whether generated code can be committed.
//
// Neither gate runs `wails build` before linting, so nothing in CI reproduces
// the sequence that broke — the failure needs a desktop build to have happened
// first, which is exactly what the operator does by hand and what the release
// workflow does before packaging. A test that reads the two files is the only
// place the invariant can be checked without requiring a Wails toolchain.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wailsGeneratedDir is where the Wails CLI writes its TypeScript bindings,
// relative to the frontend root. Not configurable in wails.json; the CLI always
// uses <frontend:dir>/wailsjs.
const wailsGeneratedDir = "wailsjs"

func readRepoFile(t *testing.T, parts ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(parts...), err)
	}
	return string(b)
}

// TestWailsGeneratedBindingsAreLintIgnored pins the entry that broke fe-lint.
func TestWailsGeneratedBindingsAreLintIgnored(t *testing.T) {
	cfg := readRepoFile(t, "..", "..", "frontend", "eslint.config.js")

	// Locate the ignores array rather than searching the whole file: the path
	// is also named in comments, and a comment does not make eslint skip a file.
	idx := strings.Index(cfg, "ignores:")
	if idx < 0 {
		t.Fatal("frontend/eslint.config.js has no `ignores:` — `eslint .` would lint generated code")
	}
	end := strings.Index(cfg[idx:], "]")
	if end < 0 {
		t.Fatal("frontend/eslint.config.js has an unterminated `ignores` array")
	}
	ignores := cfg[idx : idx+end+1]

	if !strings.Contains(ignores, `"`+wailsGeneratedDir+`"`) &&
		!strings.Contains(ignores, `'`+wailsGeneratedDir+`'`) {
		t.Errorf("frontend/eslint.config.js `ignores` must include %q.\n"+
			"`wails build` writes frontend/%s/*.ts, and `npm run lint` is `eslint .`, so without this entry\n"+
			"`make rebuild-dev` fails in fe-lint on generated code (16 errors: no-namespace, no-explicit-any).\n"+
			"ignores found: %s", wailsGeneratedDir, wailsGeneratedDir, strings.TrimSpace(ignores))
	}
}

// TestWailsGeneratedBindingsAreGitIgnored pins the entry that keeps generated
// code out of the repository. Committing it would freeze a snapshot of the Go
// API that goes stale silently the first time a bound type changes.
func TestWailsGeneratedBindingsAreGitIgnored(t *testing.T) {
	gitignore := readRepoFile(t, "..", "..", ".gitignore")

	want := "frontend/" + wailsGeneratedDir
	found := false
	for _, line := range strings.Split(gitignore, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Accept the directory with or without a trailing slash.
		if strings.TrimSuffix(line, "/") == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf(".gitignore must ignore %q (the Wails CLI's generated TypeScript bindings).\n"+
			"Without it, `git add -A` commits machine-generated reflection of the Go API that drifts\n"+
			"silently whenever a bound type changes.", want)
	}
}

// TestWailsGeneratedBindingsStayOutOfTheTypecheck is the third side of the same
// invariant, and the reason the typecheck did NOT break when lint did: the app
// tsconfig includes only `src`, so frontend/wailsjs is already outside it. If a
// future change widens that include to the frontend root, generated bindings
// enter `tsc -b` and the same class of failure returns through fe-typecheck.
func TestWailsGeneratedBindingsStayOutOfTheTypecheck(t *testing.T) {
	cfg := readRepoFile(t, "..", "..", "frontend", "tsconfig.app.json")

	idx := strings.Index(cfg, `"include"`)
	if idx < 0 {
		t.Fatal("frontend/tsconfig.app.json has no `include` — cannot tell what the typecheck reads")
	}
	end := strings.Index(cfg[idx:], "]")
	if end < 0 {
		t.Fatal("frontend/tsconfig.app.json has an unterminated `include` array")
	}
	include := cfg[idx : idx+end+1]

	// Either it stays scoped to src, or it names the generated dir in exclude.
	scopedToSrc := strings.Contains(include, `"src"`) && !strings.Contains(include, `"."`) &&
		!strings.Contains(include, `"./"`)
	if !scopedToSrc && !strings.Contains(cfg, `"exclude"`) {
		t.Errorf("frontend/tsconfig.app.json `include` is %s, which is no longer scoped to src and names no\n"+
			"`exclude`. frontend/%s is generated by `wails build`; typechecking it reintroduces the\n"+
			"failure that broke fe-lint, through fe-typecheck instead.",
			strings.TrimSpace(include), wailsGeneratedDir)
	}
}
