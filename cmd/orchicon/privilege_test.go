package main

// privilege_test.go — the elevated-shell guard, criterion 2 and the "elevated-shell
// detection" half of criterion 8.
//
// WHY THIS IS TESTABLE ON LINUX. The detection itself is a Windows syscall, so a test
// that called it would only ever exercise one platform. `elevated` is the seam
// (privilege.go), so the table below drives BOTH branch outcomes on the Linux gate —
// which is what makes "host + elevated produces an actionable error" a pinned
// behaviour rather than a claim in a comment.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useElevated pins the detection seam for one test and restores it.
func useElevated(t *testing.T, v bool) {
	t.Helper()
	old := elevated
	elevated = func() bool { return v }
	t.Cleanup(func() { elevated = old })
}

// TestEnsureHostPostgresPrivileges is the elevation table. There is ONE service
// shape now (host processes of the invoking user), so residency is no longer an
// axis: an elevated shell is the one state that must refuse.
func TestEnsureHostPostgresPrivileges(t *testing.T) {
	cases := []struct {
		name     string
		elevated bool
		wantErr  bool
	}{
		// ELEVATED IS THE ONE STATE THAT MUST REFUSE. Postgres itself will not run
		// under an Administrators token, so starting the services anyway means an
		// install that reports success and then cannot start the database.
		{name: "elevated", elevated: true, wantErr: true},
		// A normal shell: the supported shape, and it must not be disturbed.
		{name: "normal", elevated: false, wantErr: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			useElevated(t, c.elevated)
			err := ensureHostPostgresPrivileges()
			if c.wantErr && err == nil {
				t.Fatal("an elevated host-residency shell must be refused")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("this state must pass: %v", err)
			}
			if !c.wantErr {
				return
			}
			// CRITERION 2 IS ABOUT THE MESSAGE, not just the refusal: it must name
			// the CAUSE and the REMEDY, or the operator is back to decoding a cryptic
			// failure three steps later.
			msg := err.Error()
			// Case-insensitively, because "NON-elevated" and "non-elevated" are the
			// same instruction and the assertion is about the CONTENT, not the casing.
			lower := strings.ToLower(msg)
			if !strings.Contains(msg, "Administrator") {
				t.Errorf("the refusal must name the cause (Postgres refuses to run as Administrator); got: %s", msg)
			}
			if !strings.Contains(lower, "non-elevated") {
				t.Errorf("the refusal must name the remedy (open a non-elevated terminal); got: %s", msg)
			}
			// …and it must not be a bare Postgres error echoed back.
			if !strings.Contains(msg, "Postgres") {
				t.Errorf("the refusal must say WHICH program refuses; got: %s", msg)
			}
		})
	}
}

// runningElevated is false on this (Unix) platform by construction, and pinning it
// keeps the Unix decision from silently becoming something else.
func TestRunningElevatedIsFalseOnUnix(t *testing.T) {
	if runningElevated() {
		t.Fatal("runningElevated must be false on Unix: there is no Administrator token to hold")
	}
}

// The Unix uid/gid seam is the invoking process's own ids — the values host residency
// hands to postgresUIDGID. Windows reports 0 instead (privilege_windows.go); this pins
// the Unix half so a later edit cannot make the two disagree silently.
func TestInvokingIDsAreThisProcessesIDs(t *testing.T) {
	if got, want := invokingUID(), os.Getuid(); got != want {
		t.Errorf("invokingUID = %d, want os.Getuid() = %d", got, want)
	}
	if got, want := invokingGID(), os.Getgid(); got != want {
		t.Errorf("invokingGID = %d, want os.Getgid() = %d", got, want)
	}
}

// THE POSTGRES IDENTITY NEVER YIELDS A NEGATIVE UID.
//
// The bug this pins: os.Getuid()/os.Getgid() return -1 on Windows. The seam returns 0
// there instead, and this asserts the shape from the Unix side (the Windows side is the
// same property, checked by the compiler).
func TestPostgresUIDGIDNeverUsesTheWindowsSentinel(t *testing.T) {
	host := &supervisor{}
	uid, gid := host.postgresUIDGID()
	if uid < 0 || gid < 0 {
		t.Fatalf("host residency produced a negative uid/gid (%d,%d) — the Windows sentinel", uid, gid)
	}
	if uid != invokingUID() || gid != invokingGID() {
		t.Errorf("the identity must come from the invoking-identity seam: got (%d,%d), want (%d,%d)",
			uid, gid, invokingUID(), invokingGID())
	}
}

// unixSocketsSupported is the compile-time platform fact postgresSocketDir gates on, and
// the gate is the fix for the Windows argv: a Unix socket directory must NOT be passed
// to postgres.exe. Asserted here so the constant cannot be flipped without the socket
// test noticing.
func TestUnixSocketsSupportedMatchesTheSocketDir(t *testing.T) {
	if !unixSocketsSupported {
		t.Skip("windows-only property; the compiler checks it there")
	}
	host := &supervisor{log: testLogger(), dataDir: t.TempDir(), infra: testInfra()}
	if got := host.postgresSocketDir(); got != host.dataDir {
		t.Fatalf("unixSocketsSupported is true, so a socket dir must be named: got %q", got)
	}
}

// ---------------------------------------------------------------------------
// CRITERION 3, THE HOST HALF: a data dir owned by ANOTHER user must produce an
// ACTIONABLE error, not a partial initdb.
// ---------------------------------------------------------------------------

// A writable data dir must pass the probe — and the probe must leave nothing
// behind, because it runs on EVERY host boot whose dir is not yet initialized.
func TestHostDataDirWritablePassesAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	if err := hostDataDirWritable(dir); err != nil {
		t.Fatalf("a writable dir must pass: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the write probe left files behind (%d) — it must remove its own scratch file", len(entries))
	}
}

// An unwritable data dir must be refused BEFORE initdb, and the refusal must name
// the cause (the dir belongs to a different user) and a remedy (repoint the data
// dir, or hand this one to the account) — criterion 3's "actionable error rather
// than a corruption risk".
func TestHostDataDirWritableRefusesForeignOwnedDirWithARemedy(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: no directory is unwritable, so the refusal is unreachable")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil { // r-x: cannot create a file in it
		t.Fatal(err)
	}
	// t.TempDir's cleanup needs to remove it; restore write permission first.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := hostDataDirWritable(dir)
	if err == nil {
		t.Fatal("an unwritable data dir must be refused before initdb runs")
	}
	msg := err.Error()
	for _, want := range []string{dir, "cannot write", "ORCHICON_DATA_DIR"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must contain %q; got:\n%s", want, msg)
		}
	}
}

// initPostgres must CONSULT the probe in host residency, so the guard is wired
// rather than merely present. The seeded PG_VERSION makes initPostgres return
// early, which is the idempotency path — the probe lives on the other branch, so
// this pins the wiring by asserting an uninitialized foreign-owned dir is refused
// BEFORE any initdb binary is needed.
func TestInitPostgresRefusesForeignOwnedDirBeforeInitdb(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: no directory is unwritable, so the refusal is unreachable")
	}
	// HERMETIC ABOUT THE CONTAINER-ERA VOLUME, because this test is about the
	// WRITABILITY guard. On a host that still has a legacy postgres volume (the
	// operator's own machine has two), the container-era guard refuses first and
	// this assertion would fail for a reason that has nothing to do with what it
	// pins — a test that only passes on a host with no history. Saying "no engine
	// is installed here" removes the volume from the picture without touching the
	// probe this test is actually exercising.
	withEngineCandidates(t)
	s := &supervisor{log: testLogger(), dataDir: t.TempDir()}
	dir := filepath.Join(s.dataDir, "postgres")
	if err := os.MkdirAll(dir, 0o500); err != nil { // unwritable, and NO PG_VERSION
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := s.initPostgres()
	if err == nil {
		t.Fatal("an unwritable data dir must be refused, not handed to initdb")
	}
	// It must be OUR actionable error, not initdb's — the supervisor can name the
	// cause and the remedy, initdb can only report the operation it failed.
	if !strings.Contains(err.Error(), "cannot write") {
		t.Fatalf("the error must be the actionable host guard, got: %v", err)
	}
}
