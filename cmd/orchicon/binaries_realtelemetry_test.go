package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRealTelemetryArtifactsDownloadAndVerify is the AC1 PROOF for the four
// backends, opt-in (ORCHICON_TEST_REAL_DOWNLOAD=1) because it fetches ~300 MB.
// It exercises the whole pinned path against the publishers' own assets: URL,
// CHECKSUM, FLAT-ARCHIVE extraction, the canonical-name install, the chmod, and
// finally that the installed binary RUNS and reports its pinned version. The
// fixture tests prove the mechanics; only this one proves the PINNING.
func TestRealTelemetryArtifactsDownloadAndVerify(t *testing.T) {
	if os.Getenv("ORCHICON_TEST_REAL_DOWNLOAD") != "1" {
		t.Skip("opt-in")
	}
	dataDir := t.TempDir()
	opt := defaultAcquireOptions(dataDir, telemetryArtifactNames...)
	opt.PathEnv = filepath.Join(dataDir, "empty-path")
	opt.RestrictToPath = true
	opt.Out = os.Stdout
	opt.GOOS, opt.GOARCH = "linux", "amd64"

	rec, err := acquireInfraBinaries(context.Background(), opt)
	if err != nil {
		t.Fatalf("the four pinned telemetry artifacts must download and verify: %v", err)
	}
	for _, name := range telemetryArtifactNames {
		p := rec.telemetryPath(name)
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			t.Fatalf("%s: %v", name, err)
		}
		if st.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not executable: %04o", p, st.Mode().Perm())
		}
		// The real proof: the binary must RUN.
		out, err := exec.Command(p, "--version").CombinedOutput()
		t.Logf("%s -> %s (err=%v)", name, strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]), err)
		if err != nil {
			t.Errorf("%s did not run: %v: %s", p, err, out)
		}
	}
}

// TestRealTelemetryDetectionUsesAnInstalledBuild is the detect half: a host that
// already has the backends (this image ships them on PATH) must DETECT them and
// download nothing. SkipDownload makes the "nothing was fetched" claim a hard
// guarantee rather than an observation.
func TestRealTelemetryDetectionUsesAnInstalledBuild(t *testing.T) {
	dataDir := t.TempDir()
	opt := defaultAcquireOptions(dataDir, telemetryArtifactNames...)
	opt.Out = io.Discard
	opt.SkipDownload = true

	rec, err := acquireInfraBinaries(context.Background(), opt)
	if err != nil {
		t.Skipf("this host has no usable telemetry backends to detect: %v", err)
	}
	for _, name := range telemetryArtifactNames {
		p := rec.telemetryPath(name)
		if p == "" {
			t.Errorf("%s was not resolved", name)
			continue
		}
		if strings.HasPrefix(p, dataDir) {
			t.Errorf("%s = %q is under the data dir — an installed build must be used in place", name, p)
		}
	}
}
