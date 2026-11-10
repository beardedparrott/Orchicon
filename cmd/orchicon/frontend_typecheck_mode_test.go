package main

// frontend_typecheck_mode_test.go — `tsc --noEmit` AND `tsc -b` DO NOT AGREE, AND ONLY ONE IS THE BUILD.
//
// THE FAILURE THIS PINS. A change moved a `const liveBlocks` below its first use. `npx tsc --noEmit` reported
// exit 0 and the change was called green — then `make build`'s frontend step died with
//
//	src/routes/ask-orchicon.tsx:689:5 - error TS2448: Block-scoped variable 'liveBlocks' used before its declaration.
//
// `--noEmit` runs a single-file-program check; `-b` runs the project's composite build graph, and it is `-b`
// that the shipped bundle is produced by. A green from the wrong mode is worse than no check, because it is
// reported as evidence.
//
// So the relationship is asserted rather than remembered: the npm script the Makefile runs must be `tsc -b`,
// and `typecheck` must exist so the correct mode is one command away.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheFrontendBuildAndTypecheckUseTheProjectBuild is the guard.
func TestTheFrontendBuildAndTypecheckUseTheProjectBuild(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "frontend", "package.json"))
	if err != nil {
		t.Fatalf("read frontend/package.json: %v", err)
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(body, &pkg); err != nil {
		t.Fatalf("parse frontend/package.json: %v", err)
	}

	build, ok := pkg.Scripts["build"]
	if !ok {
		t.Fatal("frontend/package.json has no `build` script — the Makefile's fe-build runs it")
	}
	// THE SHIPPED BUNDLE IS PRODUCED BY `tsc -b`. If that changes, this gate and every check that trusts it
	// must change with it, and a plain `tsc --noEmit` is NOT an acceptable substitute (it misses TS2448/TS2454
	// — use-before-declaration — because it does not walk the composite graph).
	if !strings.Contains(build, "tsc -b") {
		t.Errorf("the build script no longer runs `tsc -b`:\n\t%s\n\n"+
			"`tsc --noEmit` passes on code `tsc -b` rejects, so whatever replaces it must be a check the "+
			"BUILD actually runs.", build)
	}

	tc, ok := pkg.Scripts["typecheck"]
	if !ok {
		t.Fatal("frontend/package.json has no `typecheck` script — without one, the nearest command to hand " +
			"is `tsc --noEmit`, which does not agree with the build")
	}
	if !strings.Contains(tc, "tsc -b") {
		t.Errorf("`npm run typecheck` must run the SAME mode the build does (`tsc -b`), got:\n\t%s", tc)
	}
}
