package planebind

// wiring_test.go — the binding must actually be BOUND by the desktop shell.
//
// The Wails shell task lands on its own branch, so on a branch without it this test
// SKIPS — a deferral, not a pass: the moment cmd/orchicon-desktop/main.go exists, an
// unbound planebind.Service is a RED test, because a page whose actions cannot reach
// the binding is a page that lies about being able to start a plane.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDesktopShellBindsThePlaneControlService(t *testing.T) {
	const shellMain = "../../cmd/orchicon-desktop/main.go"
	src, err := os.ReadFile(filepath.Join(filepath.FromSlash(shellMain)))
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("the Wails shell task has not landed on this branch yet — the moment " +
				shellMain + " exists, it must bind planebind.Service")
		}
		t.Fatalf("read %s: %v", shellMain, err)
	}

	text := string(src)
	if !strings.Contains(text, "planebind.Service") {
		t.Fatalf("%s exists but does not bind planebind.Service — Manage Server's actions would have no "+
			"binding to call (add &planebind.Service{} to wails.Run's Bind slice)", shellMain)
	}
	if !strings.Contains(text, "planebind") {
		t.Fatalf("%s mentions planebind.Service but does not import the package", shellMain)
	}
}
