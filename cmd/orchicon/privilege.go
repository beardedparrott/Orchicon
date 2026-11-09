package main

// This is criterion 2 of the work item: on Windows, launching from an elevated shell must produce a
// clear, actionable error naming the cause and the remedy.

import (
	"errors"
	"fmt"
	"os"
)

// hostDataDirWritable proves the invoking user can actually WRITE the Postgres data dir before
// initdb is allowed to try, and returns an actionable error when it cannot.
func hostDataDirWritable(dataDir string) error {
	f, err := os.CreateTemp(dataDir, ".orchicon-write-probe-*")
	if err != nil {
		return fmt.Errorf(
			"the Postgres data dir %q exists but the invoking user cannot write it, "+
				"so initdb would fail partway and could leave a half-initialized data dir: %w\n"+
				"  The data dir belongs to a DIFFERENT user (an earlier elevated run, a root-owned "+
				"leftover, or a shared path).\n"+
				"  Fix it one of two ways, then run the command again:\n"+
				"    - point ORCHICON_DATA_DIR at a directory your own account owns (e.g. the default "+
				"%s), or\n"+
				"    - hand the existing directory to your account: %s",
			dataDir, err, defaultDataDirHint(), chownHint(dataDir))
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

// defaultDataDirHint is the platform's default data dir, for the error's
// "use a dir you own instead" remedy.
func defaultDataDirHint() string { return defaultDataDirFor() }

// chownHint is the platform-appropriate way to hand a directory to the invoking account.
func chownHint(dir string) string {
	if windowsDirHint != "" {
		return fmt.Sprintf("take ownership of %q from an elevated prompt (Properties → Security → Advanced)", dir)
	}
	return fmt.Sprintf("sudo chown -R \"$(id -u):$(id -g)\" %q", dir)
}

// windowsDirHint is "" on Unix and non-empty on Windows, so chownHint can pick the platform's
// remedy without a build tag on this file (privilege.go is platform-neutral by design).
var windowsDirHint = platformDirHint()

// elevated is the DETECTION SEAM, and it exists so the guard has coverage on the Linux CI host
// rather than only on a Windows machine: the test suite replaces it and exercises both branch
// outcomes.
var elevated = runningElevated

// ensureHostPostgresPrivileges refuses to start HOST-RESIDENT Postgres services
// from an elevated Windows shell.
//
// WHY IT EXISTS AT ALL. Postgres upstream refuses to run under an Administrators
// token:
//
//	"Execution of PostgreSQL by a user with administrative permissions is not permitted."
//
// and it says that from inside initdb/postgres, after the supervisor has already
// created dirs and resolved binaries. Letting it surface there is the worst class
// of failure this task is about: an install that APPEARS to work — the binary
// unpacked, the dirs written, the resolver reported success — and then cannot
// start the database, with nothing naming the actual cause. So the check runs
// BEFORE anything is created, and it names both the cause and the remedy.
//
// IT RUNS UNCONDITIONALLY, because there is only one service shape now: host
// processes of the invoking user. (The retired container residency ran the
// services as uid 70 inside a container, where the invoking shell's elevation was
// irrelevant; that branch is gone with the single-container deployment.)
//
// WHEN IT FIRES the fix is genuinely actionable and local: the operator opens a
// NORMAL terminal. The second half of the message matters as much as the first —
// an elevated shell would also leave the data dir owned by the elevating account,
// so even a hand-run initdb would produce a data dir the operator's own account
// could not read afterwards.
func ensureHostPostgresPrivileges() error {
	if !elevated() {
		return nil
	}
	return errors.New(
		"Postgres refuses to run as Administrator, and this shell is elevated.\n" +
			"  Open a NON-elevated terminal (PowerShell or cmd — do not use \"Run as administrator\",\n" +
			"  and do not start this from an elevated session) and run the command again.\n" +
			"  Use a normal user data dir rather than a protected one, e.g. the default\n" +
			"  %LOCALAPPDATA%\\orchicon — an elevated shell would also make the data dir\n" +
			"  unreadable to your own account afterwards.")
}
