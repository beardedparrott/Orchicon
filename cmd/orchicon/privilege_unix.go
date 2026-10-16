//go:build !windows

package main

// privilege_unix.go — the Unix half of the process's own privilege identity.
//
// Nothing here ever ELEVATES; the question is only whether we are in a shape the
// services can run in. On Linux and macOS that question has the same answer, so
// this one file serves both.

import "os"

// runningElevated reports whether this process holds a privileged token.
//
// LINUX AND macOS HAVE NO "ADMINISTRATOR TOKEN" to refuse. Postgres's refusal to
// run as Administrator is a WINDOWS rule (the upstream check reads the token
// group membership; there is even a `pg_ctl` refusal inspired by it), and it has
// no Unix counterpart that this task needs to detect: on Unix the equivalent
// hazard is running as uid 0, which the supervisor already handles by handing the
// child to uid 70 via setpriv in container residency and by simply BEING the
// invoking (non-root) user in host residency. `orchicon serve` as root is not a
// supported shape but it is also not silently broken the way Postgres-as-
// Administrator is — and inventing a Unix refusal here would change behaviour for
// every existing install, which this task must not do.
//
// SO THIS IS `false`, AND THAT IS THE DECISION rather than a stub: the elevated-
// shell check exists for exactly one platform, and pretending otherwise on Unix
// would make the check fire where nothing is wrong.
func runningElevated() bool { return false }

// invokingUID is the uid the supervisor's own process runs as.
func invokingUID() int { return os.Getuid() }

// invokingGID is the gid the supervisor's own process runs as.
func invokingGID() int { return os.Getgid() }

// unixSocketsSupported reports whether postgres's Unix-domain socket transport
// exists on this platform. It does on Unix.
const unixSocketsSupported = true

// platformDirHint is "" on Unix: the ownership remedy is the `sudo chown` the
// caller builds, so no platform-specific sentence is needed.
func platformDirHint() string { return "" }
