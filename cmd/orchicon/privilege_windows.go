//go:build windows

package main

// privilege_windows.go — the Windows half of the process's own privilege identity,
// and the ONE platform where "am I elevated?" is a question the services care about.
//
// POSTGRES REFUSES TO RUN UNDER AN ADMINISTRATORS TOKEN. Its own startup check says
// so: "Execution of PostgreSQL by a user with administrative permissions is not
// permitted." There is no uid to drop to — Windows has no unix model — so the only
// correct behaviour is to REFUSE TO START EARLY and say why, which is what
// ensureHostPostgresPrivileges does with runningElevated below. If we did not, the
// failure would surface three steps later as an initdb/postgres error with no cause
// named, which is the "install that appears to work and then cannot start the
// database" class this check exists to prevent.

import "golang.org/x/sys/windows"

// runningElevated reports whether the current process token is elevated.
//
// IsElevated is the documented UAC check (it reads TokenElevation), and it is the
// ONE-SYSCALL answer. The alternative — scraping `whoami /groups` for the
// Administrators SID, or probing `net session` — needs a subprocess and a string
// match, which is slower and can be wrong under a localized or filtered token. It
// does NOT tell us whether the account is AN ADMINISTRATOR; it tells us whether
// this TOKEN is elevated, which is exactly the condition Postgres objects to (an
// administrator running from a non-elevated shell is fine, and Postgres runs for
// them).
//
// The token API is already a direct dependency: golang.org/x/sys is required in
// go.mod for exactly this kind of platform call, so this adds no module.
func runningElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// invokingUID/invokingGID report the invoking user's ids.
//
// os.Getuid()/os.Getgid() return -1 on Windows (os/syscall_windows.go), and -1
// is a LIE that only stays harmless because asUser returns the command verbatim in
// host residency. Make the sentinel explicit instead: 0 is "no uid to drop to",
// which is what asUser's `uid != 0` test means, so the test keeps its meaning on a
// platform that has no uids. Nothing chowns with these values — host residency
// never calls os.Chown.
func invokingUID() int { return 0 }
func invokingGID() int { return 0 }

// unixSocketsSupported reports whether postgres's Unix-domain socket transport
// exists. It does NOT on Windows, and the consequence is load-bearing rather than
// cosmetic: postgresSocketDir() must return "" there, because passing
// `-c unix_socket_directories=<dir>` to postgres.exe is at best ignored and at
// worst a startup error.
const unixSocketsSupported = false

// platformDirHint is non-empty on Windows, so the data-dir-ownership remedy names
// the Windows mechanism (take ownership from an elevated prompt) instead of a
// `sudo chown` that does not exist there.
func platformDirHint() string { return "windows" }
