package api

// lifecycle_surface_test.go — AC2: NO lifecycle RPC was added. This test is the tripwire: the generated
// Connect surface is enumerated and its SIZE is pinned, so adding an RPC is a deliberate act (bump the number
// below) rather than an accident, and no procedure may be named like a plane lifecycle control.
//
// IT IS ALSO THE REASON THE MECHANISM IS A GO PACKAGE (internal/planecontrol) AND NOT AN RPC. The API path
// existed for the web app — a browser cannot spawn a process — and the web app is out of scope for lifecycle
// control, so there is deliberately nothing here to call.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// generatedProcedures is the number of Connect procedures the generated clients declare. BUMPING IT IS THE
// DELIBERATE ACT OF ADDING AN RPC; a lifecycle control must not be one of them.
//
// 238 -> 239: develop added one procedure to RuntimeImageService (FetchRuntimeImage) while this
// branch was open, and it is the deliberate act this pin demands — a runtime-image query, not a lifecycle
// control. The name tripwire below re-checks that on every run, so a lifecycle RPC cannot ride in on a bump.
const generatedProcedures = 239

// lifecycleName matches a procedure or RPC that would put plane lifecycle control on the API.
var lifecycleName = regexp.MustCompile(`(?i)(start|stop|restart|shutdown|lifecycle|status).*(plane|instance)|(plane|instance).*(start|stop|restart|shutdown|lifecycle)`)

func TestAPISurfaceDidNotGrow(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "api", "gen", "go", "orchicon", "api", "v1", "apiv1connect", "*.connect.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob generated connect clients: %v (%d files)", err, len(files))
	}
	procRe := regexp.MustCompile(`Procedure = "([^"]+)"`)
	procs := map[string]bool{}
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range procRe.FindAllStringSubmatch(string(b), -1) {
			procs[m[1]] = true
		}
	}
	if len(procs) != generatedProcedures {
		t.Errorf("the Connect API declares %d procedures, want %d — if an RPC was added on purpose, bump "+
			"generatedProcedures; if it is a plane lifecycle control, it does not belong on the API at all",
			len(procs), generatedProcedures)
	}
	names := make([]string, 0, len(procs))
	for p := range procs {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		if lifecycleName.MatchString(p) {
			t.Errorf("procedure %q looks like plane lifecycle control — this task must not add a lifecycle RPC", p)
		}
	}
}

func TestProtoDeclaresNoLifecycleService(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "proto", "orchicon", "api", "v1", "*.proto"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob proto files: %v (%d files)", err, len(files))
	}
	rpcRe := regexp.MustCompile(`(?m)^\s*rpc\s+([A-Za-z0-9_]+)`)
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range rpcRe.FindAllStringSubmatch(string(b), -1) {
			if lifecycleName.MatchString(m[1]) {
				t.Errorf("%s declares rpc %s — plane lifecycle control is LOCAL process invocation only",
					filepath.Base(path), m[1])
			}
		}
		if strings.Contains(string(b), "PlaneLifecycle") {
			t.Errorf("%s mentions PlaneLifecycle — no lifecycle service exists", filepath.Base(path))
		}
	}
}
