package instancectl

// spawndir.go — WHICH DIRECTORY A CHILD RUNS IN, which must never be inherited from the app.
//
// REPORTED: "When trying to create a test environment in the desktop in windows (with the desktop launched
// from the installer), it complained about not being able to create it due to the directory the desktop ran
// from. If I closed out of the desktop and launched it from the start menu, then it did allow me to create
// test with no issues."
//
// THAT IS A WORKING-DIRECTORY DIFFERENCE, and it is the whole of it. Nothing in `orchicon install` reads its
// working directory — verified — but every child this package starts INHERITED the app's, because no spawn
// site set cmd.Dir. On Windows a process cannot be created when the inherited working directory no longer
// exists: CreateProcess fails with ERROR_DIRECTORY ("The directory name is invalid") and the child never
// runs at all. A GUI launched by an installer can have exactly such a cwd — the installer's staging area, or
// a directory the installer cleaned up — while the same app launched from the Start menu sits in its own
// install directory, which is stable. Hence "it works if I launch it from the start menu".
//
// THE FIX IS NOT TO PATCH THE INSTALLER, it is to stop depending on an ambient directory at all. A GUI has no
// meaningful current directory; inheriting one makes its behaviour a function of how it happened to be
// launched. So every child gets an EXPLICIT, stable, writable directory, chosen here.
//
// THE USER'S HOME is the right answer for these verbs specifically: install and uninstall read and write
// per-user state under the home directory anyway, so a child that ever does resolve a relative path resolves
// it somewhere it already owns — and unlike a launch directory, the home directory is not a thing an
// installer creates and deletes.

import (
	"os"
	"path/filepath"
)

// spawnDir is the working directory for a child process this package starts, or "" when nothing usable could
// be found (in which case the caller inherits, which is the old behaviour rather than a new failure).
//
// IT CHECKS THAT THE RESULT EXISTS, because a directory that is present in principle and absent in fact is
// precisely the condition that fails a spawn. Falling back to the executable's own directory is not a
// theoretical branch: it is the one directory guaranteed to exist, since it is where this process's image
// lives.
func spawnDir() string {
	if home, err := os.UserHomeDir(); err == nil && isUsableDir(home) {
		return home
	}
	if exe, err := os.Executable(); err == nil {
		if dir := filepath.Dir(exe); isUsableDir(dir) {
			return dir
		}
	}
	return ""
}

// isUsableDir reports whether p is an existing directory.
func isUsableDir(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// unusableCwd explains a failure to start a child when this process's own working directory is the reason.
//
// A DIAGNOSTIC, NOT A PREVENTION — spawnDir already stops this happening. It exists because the Win32 error
// for this case is "The directory name is invalid", which names nothing an operator can act on, and because
// a failure that says only that sent the reporter looking at the INSTANCE rather than at the launch. Returns
// "" when the working directory is fine, so the caller adds nothing and the ordinary error stands.
func unusableCwd() string {
	if _, err := os.Getwd(); err != nil {
		return "this process has no usable working directory (it was started from a directory that no longer " +
			"exists), which is why Windows refused to start the child at all — relaunch Orchicon from the " +
			"Start menu, or from any directory that exists"
	}
	return ""
}
