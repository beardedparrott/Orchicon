package main

// tool_dirs_makefile_test.go — every $(DEV_TOOLS) directory the Makefile exports must exist before a
// recipe runs: go creates GOCACHE and GOPATH itself, but not GOTMPDIR, and names it only as a path.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryExportedToolDirIsCreated derives the directories from the exports rather than naming them, so a
// future GOMODCACHE is covered without editing this file.
func TestEveryExportedToolDirIsCreated(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	src := string(body)

	decl := regexp.MustCompile(`(?m)^([A-Z][A-Z0-9_]*)[ \t]*\?=[ \t]*\$\(DEV_TOOLS\)/\S+`)
	exported := regexp.MustCompile(`(?m)^export ([A-Z][A-Z0-9_]*)$`)
	mkdir := regexp.MustCompile(`(?m)^\$\(shell mkdir -p (.*)\)$`)

	isExported := make(map[string]bool)
	for _, m := range exported.FindAllStringSubmatch(src, -1) {
		isExported[m[1]] = true
	}
	created := make(map[string]bool)
	for _, m := range mkdir.FindAllStringSubmatch(src, -1) {
		for _, f := range strings.Fields(m[1]) {
			created[strings.TrimSuffix(strings.TrimPrefix(f, "$("), ")")] = true
		}
	}

	var want []string
	for _, m := range decl.FindAllStringSubmatch(src, -1) {
		if isExported[m[1]] {
			want = append(want, m[1])
		}
	}
	// A parser-shaped gate that sees nothing must fail: a vacuous pass is its characteristic bug.
	if len(want) == 0 {
		t.Fatal("the Makefile exports no $(DEV_TOOLS)/… directory any more, or the parse no longer sees the " +
			"block — either the exports moved (update this gate with them) or the tool dirs are gone")
	}
	for _, name := range want {
		if !created[name] {
			t.Errorf("%s points into $(DEV_TOOLS) and is exported, but no `$(shell mkdir -p …)` creates it — "+
				"after the tool dirs are cleaned, the first $(GO) call dies with `creating work dir: stat …`, "+
				"which names a path instead of the cleanup that removed it", name)
		}
	}
}
