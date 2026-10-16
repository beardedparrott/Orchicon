package main

// shquote_test.go — the POSIX single-quote helper, in a file with NO build tag.
//
// IT LIVES HERE RATHER THAN BESIDE ITS FIRST USER because `binaries_test.go` — which has no build
// tag on purpose, since it drives the RESOLVER through a `goos` argument and therefore constructs
// Windows-shaped paths and `pg_dump.exe` names on every host — also needs it. A symbol defined in a
// `//go:build !windows` file is invisible to a Windows build, so the untagged consumer could not
// compile there:
//
//	GOOS=windows go test -c ./cmd/orchicon
//	cmd/orchicon/binaries_test.go:48:31: undefined: shellQuote
//
// That is the same class of mistake `serve_reap_unix.go` warns about (a symbol that belongs to one
// platform sitting in a file that builds on all of them, or the reverse), and it matters here for
// the same reason it matters in `hostproc_unix.go`: Windows is a first-class host for this
// repository now, so an untagged file that cannot compile there is a build break waiting for the
// next release. Keeping the helper untagged — it is a handful of `strings.ReplaceAll` and touches no
// OS API — is what lets BOTH the Unix-only orphan tests and the platform-neutral resolver tests use
// it.

import "strings"

// shellQuote wraps s in POSIX single quotes, escaping embedded single quotes, so the result can be
// spliced into a `#!/bin/sh` fixture verbatim.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
