package runtime

// adapter_surface_log_test.go — A MISSING MOUNT MUST BE READABLE IN ONE LINE.
//
// REPORTED: "the string .claude appears ZERO times in the entire plane log for this run — no mount attempted, no
// path resolved", offered as proof the claude code path was never reached.
//
// That scan could not prove it, for two reasons:
//
//  1. the mounts are built by the DAEMON, and the operator was reading the PLANE log;
//  2. even in the right log, a SUCCESSFUL create logged only a COUNT ("workflow runtime ensured ... mounts=12"),
//     never a path.
//
// These assertions pin the line that makes the question answerable: the resolved boot profile and adapter data
// dirs, logged at create time.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDescribeAdapterMountsNamesTheClaudeSurface is the reported case.
func TestDescribeAdapterMountsNamesTheClaudeSurface(t *testing.T) {
	home := t.TempDir()
	mustDir := func(parts ...string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(append([]string{home}, parts...)...), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustFile := func(parts ...string) {
		t.Helper()
		p := filepath.Join(append([]string{home}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustDir(".claude")
	mustFile(".claude", ".credentials.json")
	mustFile(".claude.json")
	mustDir(".local", "bin")
	mustFile(".local", "bin", "claude")

	got := describeAdapterMounts(home, "claude")
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		filepath.Join(home, ".claude"),
		filepath.Join(home, ".claude", ".credentials.json"),
		filepath.Join(home, ".claude.json"),
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the logged adapter surface omits %s, so a missing mount cannot be read from the log — which "+
				"is exactly what made the reported round trip necessary:\n%s", want, joined)
		}
	}
}

// TestDescribeAdapterMountsIsEmptyWithoutAHome — no home, no claim. A log line that invented paths would be worse
// than silence.
func TestDescribeAdapterMountsIsEmptyWithoutAHome(t *testing.T) {
	if got := describeAdapterMounts("", "claude"); len(got) != 0 {
		t.Errorf("no host home produced %v", got)
	}
}

// TestDescribeAdapterMountsCarriesNoCredentials — paths only.
//
// The line is logged on every create, so it must never carry file CONTENTS: the credential lives at a path and
// naming the path is the diagnosis; logging the value would be the leak.
func TestDescribeAdapterMountsCarriesNoCredentials(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	secret := `{"access_token":"sk-ant-SECRET-VALUE"}`
	if err := os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(describeAdapterMounts(home, "claude"), "\n")
	if strings.Contains(joined, "sk-ant-SECRET-VALUE") {
		t.Fatalf("the logged adapter surface carries the credential VALUE:\n%s", joined)
	}
}
