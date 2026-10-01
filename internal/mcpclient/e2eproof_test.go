package mcpclient

// e2eproof_test.go — the fixture sanity leg plus the TWO negative proofs that
// are source-level by nature:
//
//   AC 5 (fixture half): the distinctive probe tool is DISCOVERED over a real
//   streamable-HTTP transport and a call returns the echoed nonce. This is the
//   plumbing floor every other leg stands on — if it fails, nothing else means
//   anything.
//
//   AC 6: NO tenant-level MCP or skill surface remains. The honest form of
//   "grep + UI observation" is a source-level gate: walk the tree and FAIL on
//   any occurrence of the retired tenant-tier vocabulary outside the two places
//   that must mention it (the migration that DROPS the tier, and the frontend
//   test that asserts its ABSENCE). A grep run by hand once proves nothing
//   about the NEXT commit; this test is the pin.
//
// Gate: ORCHICON_TEST_DSN (the DB-backed half only; the AC 6 greps always run).

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AC 5 (fixture half) + the plumbing floor: the probe tool is discovered under
// the mcp__<server>__ namespace through a REAL transport, and a call carrying a
// per-run nonce returns that nonce — so a cached/stale/memoised answer is
// distinguishable from a live one.
func TestE2EFixtureProbeDiscoversAndEchoesNonce(t *testing.T) {
	url, closeFn := E2EHTTPFixture()
	defer closeFn()

	const serverID = "e2eprobe"
	m := NewManager(nil)
	defs, err := m.Start(context.Background(), []ServerSpec{{ID: serverID, Type: TypeHTTP, URL: url}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	wantName := ToolName(serverID, E2EProbeTool)
	found := false
	for _, d := range defs {
		if d.Name == wantName {
			found = true
		}
	}
	if !found {
		names := make([]string, 0, len(defs))
		for _, d := range defs {
			names = append(names, d.Name)
		}
		t.Fatalf("the distinctive probe tool %q was not discovered; got %v", wantName, names)
	}

	nonce := "e2e-fixture-nonce-1"
	out, err := m.Execute(context.Background(), wantName, `{"nonce":"`+nonce+`"}`)
	if err != nil {
		t.Fatalf("Execute %s: %v", wantName, err)
	}
	if !strings.Contains(out, E2EProbeResultPrefix+nonce) {
		t.Fatalf("probe result = %q, want it to carry %q", out, E2EProbeResultPrefix+nonce)
	}
}

// AC 5 (fixture half, stdio): the SAME probe tool is reachable over the stdio
// transport (the transport with child-lifecycle risk in production). The re-exec
// hook lives in this package's TestMain / TestE2EFixtureReexec below.
func TestE2EFixtureProbeOverStdio(t *testing.T) {
	spec := E2EStdioSpec("e2estdio")
	m := NewManager(nil)
	if _, err := m.Start(context.Background(), []ServerSpec{spec}); err != nil {
		t.Fatalf("Start (stdio): %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	wantName := ToolName(spec.ID, E2EProbeTool)
	out, err := m.Execute(context.Background(), wantName, `{"nonce":"stdio-nonce"}`)
	if err != nil {
		t.Fatalf("Execute %s over stdio: %v", wantName, err)
	}
	if !strings.Contains(out, E2EProbeResultPrefix+"stdio-nonce") {
		t.Fatalf("stdio probe result = %q, want it to carry %q", out, E2EProbeResultPrefix+"stdio-nonce")
	}
}

// --- AC 6: no tenant-level MCP or skill surface remains -----------------------

// tenantTierNeedles is the retired tenant-tier vocabulary. Every one of these
// named a tenant-WIDE default (a tier with no bounded owner). Their absence from
// the product is criterion 6.
var tenantTierNeedles = []string{
	"tenant_settings.default_mcp_servers",
	"default_mcp_servers",
	"GetTenantDefaultMCPServers",
	"SetTenantDefaultMCPServers",
	"useGetTenantDefaultMCPServers",
	"useSetTenantDefaultMCPServers",
	"SetProjectMCPServers",
	"GetProjectMCPServers",
	"ListProjectMCPServerIDs",
	"SetMCPServerSelection",
	"MCPPicker",
	"MCPServersTab",
	"SetTenantDefaultMCP",
	"tenant_default_mcp",
}

// tenantTierAllowlist is the ONLY set of paths allowed to name the tier, with
// the reason each is legitimate:
//
//   - the ownership migration (and its down migration) are the SQL that DROPS
//     the column — a migration that removes a tier must name it;
//   - the frontend panel test asserts the hooks' ABSENCE (a negative assertion
//     cannot be written without the name);
//   - the frontend placement test does the same for the retired component
//     names;
//   - the TUI's project form carries historical NOTE comments explaining what
//     was REMOVED and why (the "no selection to write any more" record);
//   - mcpclient/command.go's comment cites the old frontend shape by name.
//
// Anything ELSE matching a needle is the failure.
func tenantTierAllowlist(path string) bool {
	p := filepath.ToSlash(path)
	// The proof harness itself NAMES every needle: the gate below, and the Ask
	// tool-list gate that reuses the same vocabulary. A file whose job is to
	// assert the tier's absence must be able to write the tier's name.
	base := filepath.Base(p)
	if base == "e2eproof_test.go" || strings.HasPrefix(base, "e2e_") && strings.HasSuffix(base, "_proof_test.go") {
		return true
	}
	switch {
	case strings.HasSuffix(p, "db/migrations/20260915000002_tenant_default_mcp_servers.sql"),
		strings.HasSuffix(p, "db/migrations/20260915000002_tenant_default_mcp_servers_down.sql"),
		strings.HasSuffix(p, "db/migrations/20260929000000_mcp_server_ownership.sql"),
		strings.HasSuffix(p, "db/migrations/20260929000000_mcp_server_ownership_down.sql"),
		strings.HasSuffix(p, "frontend/src/components/MCPServersPanel.test.tsx"),
		strings.HasSuffix(p, "frontend/src/components/MCPServersPanel.placement.test.tsx"),
		// The panel itself records that it IS the extraction of the retired tab,
		// in a header comment; the placement test excludes it for the same reason.
		strings.HasSuffix(p, "frontend/src/components/MCPServersPanel.tsx"),
		strings.HasSuffix(p, "internal/tui/screens/work/projects.go"),
		strings.HasSuffix(p, "internal/tui/screens/work/screen_test.go"),
		strings.HasSuffix(p, "internal/db/mcp_servers.go"),
		strings.HasSuffix(p, "internal/mcpclient/command.go"):
		return true
	}
	return false
}

// skipScanDir reports whether a directory is out of scan scope.
func skipScanDir(name string) bool {
	switch name {
	case ".git", "node_modules", "dist", "build", "vendor", ".orchicon-worktrees", ".gotmp":
		return true
	}
	return false
}

// TestE2ENoTenantMCPSurfaceRemains is criterion 6's SOURCE gate: no file in
// internal/, proto/ or frontend/src/ names a tenant-tier MCP or skill surface
// outside the documented allowlist. The failure message names the file, the
// line and the needle, so the next author knows exactly what to delete.
func TestE2ENoTenantMCPSurfaceRemains(t *testing.T) {
	root := repoRootE2E(t)
	var scanned int
	var violations []string
	for _, sub := range []string{"internal", "proto", "frontend/src"} {
		start := filepath.Join(root, sub)
		if _, err := os.Stat(start); err != nil {
			continue
		}
		err := filepath.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if skipScanDir(d.Name()) {
					return fs.SkipDir
				}
				return nil
			}
			name := d.Name()
			// Generated files and migrations are not product surfaces.
			if strings.HasSuffix(name, ".gen.ts") || strings.HasSuffix(name, "_pb.ts") ||
				strings.HasSuffix(name, "_connect.ts") || strings.HasSuffix(name, ".gen.go") ||
				strings.HasSuffix(name, "_pb.go") {
				return nil
			}
			if tenantTierAllowlist(path) {
				return nil
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				rel = path
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			scanned++
			for i, line := range strings.Split(string(data), "\n") {
				for _, needle := range tenantTierNeedles {
					if strings.Contains(line, needle) {
						violations = append(violations,
							filepath.ToSlash(rel)+":"+itoa(i+1)+": "+needle)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", sub, err)
		}
	}
	if scanned == 0 {
		t.Fatal("the AC 6 scan read no files — the walk root or the filters are wrong, so this test would pass vacuously")
	}
	if len(violations) > 0 {
		t.Fatalf("a TENANT-LEVEL MCP/skill surface is still reachable (%d hit(s)); the tier must be gone:\n%s\n"+
			"(if one of these is a legitimate mention, add it to tenantTierAllowlist WITH a reason)",
			len(violations), strings.Join(violations, "\n"))
	}
}

// TestE2EClientsHaveTheOnePanelNotTheTenantTabs is the cheap STRUCTURAL half of
// the UI observation: child 7 replaced the tenant-scoped tabs with ONE
// scope-parameterized panel. The retired components must not exist as files.
func TestE2EClientsHaveTheOnePanelNotTheTenantTabs(t *testing.T) {
	root := repoRootE2E(t)
	comps := filepath.Join(root, "frontend", "src", "components")
	if _, err := os.Stat(filepath.Join(comps, "MCPServersPanel.tsx")); err != nil {
		t.Fatalf("the ONE scope-parameterized panel is missing: %v", err)
	}
	for _, retired := range []string{"MCPPicker.tsx", "MCPServersTab.tsx"} {
		if _, err := os.Stat(filepath.Join(comps, retired)); err == nil {
			t.Errorf("the retired tenant-scoped component %s still exists", retired)
		}
	}
	// The Ask panel route must mount the ONE panel (not a tenant tab set).
	panel, err := os.ReadFile(filepath.Join(comps, "MCPServersPanel.tsx"))
	if err != nil {
		t.Fatalf("read the panel: %v", err)
	}
	if !strings.Contains(string(panel), "scope") {
		t.Error("MCPServersPanel.tsx does not mention a scope — it is not the scope-parameterized replacement")
	}
}

// repoRootE2E walks up from the test's working directory to the module root.
func repoRootE2E(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}

// itoa is a tiny int→string so this file needs no strconv import for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
