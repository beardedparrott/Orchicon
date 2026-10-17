package runtime

// AC 1 as a SOURCE guard: no `docker` invocation may re-enter the DAEMON tree.
//
// Renaming DockerBin → Engine and d.docker → d.engine is a one-way door only if
// something keeps it shut. The engine binary is resolved in ONE place
// (engine.go, via Engine.binary()) and executed in ONE place (Daemon.engine /
// exec.Command(d.engineBin(), …)); this walks internal/runtime and fails on any
// direct `exec.Command("docker"|"podman" …)` or on the retired identifiers, so a
// later hurried change cannot quietly reintroduce a second engine identity.
//
// cmd/orchicon is walked too, with a SHORT, EXPLICIT allowlist: the host-install
// / control-plane lifecycle commands still drive docker by name. Those are
// ADR-0013 §12 item 13's slice (the install task) — pinning them here as known
// exceptions is deliberate, so the residual is visible rather than forgotten,
// and so any NEW literal in cmd/orchicon still fails this test.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// directEngineExec matches an engine binary named as a string literal to
// exec.Command / exec.CommandContext — the exact shape this task removed.
var directEngineExec = regexp.MustCompile(`exec\.Command(Context)?\(\s*(context\.[A-Za-z]+\([^)]*\),\s*)?"(docker|podman)"`)

// engineGuardBanned are the identifiers/forms the rename retired. They are
// checked literally (no regex) so a match is unambiguous.
var engineGuardBanned = []string{
	"DockerBin",
	"d.docker(",
	".dockerFn",
}

// engineGuardAllowed names the cmd/orchicon files whose docker literals are
// KNOWN and owned by another slice of ADR-0013. internal/runtime is NOT allowed
// any entry — the daemon must go through Engine, full stop.
var engineGuardAllowed = map[string]string{
	"install.go":        "ADR-0013 §12 item 13: the host-install lifecycle still drives docker (containerEngineAvailable must learn podman there)",
	"install_bridge.go": "ADR-0013 §12 item 13: docker bridge-IP discovery moves to the engine probe in the install slice",
	"install_ports.go":  "ADR-0013 §12 item 13: port-conflict inspection on the install path",
	"restart.go":        "ADR-0013 §12 item 13: restarting the existing control-plane container",
}

func TestNoDirectEngineInvocation(t *testing.T) {
	roots := []struct {
		dir     string
		daemon  bool // the daemon tree: no exceptions at all
		checked *int
	}{
		{".", true, new(int)},
		{filepath.Join("..", "..", "cmd", "orchicon"), false, new(int)},
	}
	for _, root := range roots {
		entries, err := os.ReadDir(root.dir)
		if err != nil {
			t.Fatalf("read %s: %v", root.dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			if reason, ok := engineGuardAllowed[e.Name()]; ok && !root.daemon {
				t.Logf("known residual docker usage in %s — %s", filepath.Join(root.dir, e.Name()), reason)
				continue
			}
			path := filepath.Join(root.dir, e.Name())
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			*root.checked++
			if m := directEngineExec.FindString(string(src)); m != "" {
				t.Errorf("%s: engine binary named literally (%s) — resolve it through Engine/engineBin() instead", path, m)
			}
			for _, banned := range engineGuardBanned {
				if strings.Contains(string(src), banned) {
					t.Errorf("%s: retired identifier %q — the engine seam is Engine / d.engine / engineFn", path, banned)
				}
			}
		}
		if *root.checked == 0 {
			t.Fatalf("the guard checked no files under %s — its roots are wrong and it would pass vacuously", root.dir)
		}
	}
}
