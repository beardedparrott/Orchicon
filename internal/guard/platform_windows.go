//go:build windows

package guard

// platform_windows.go — THE GUARD DOES NOT WORK ON WINDOWS, and it never did.
//
// REPORTED: every Ask turn through the opencode adapter failed with
//
//	create conversation session: host opencode serve unavailable — Ask chat transport is disabled:
//	host serve guard: guard: symlink sudo: symlink guard
//	C:\Users\operator\AppData\Local\Temp\orchicon-guard-706398431\sudo: A required privilege is not held
//	by the client.
//
// "Why is there a sudo in windows?" — there is no sudo; that is a SYMLINK NAMED `sudo`, dangling by
// design, because the guard intercepts guarded names by shadowing them on PATH. On POSIX that is a
// per-execution temp directory of symlinks pointing at one bash script.
//
// THREE SEPARATE REASONS THAT CANNOT WORK HERE, any one of them fatal:
//
//  1. CREATING A SYMLINK NEEDS A PRIVILEGE THE PLANE DOES NOT HAVE. Windows requires
//     SeCreateSymbolicLinkPrivilege (an elevated token, or Developer Mode). The plane runs as an
//     ordinary detached process from the MSI, with a FILTERED token: the privilege is not present.
//     Measured on the reporting machine — an ELEVATED ssh session created the symlink happily, which
//     is exactly why this was not caught earlier: it works when tested from an admin shell and fails
//     for the process that actually needs it. The OS error is ERROR_PRIVILEGE_NOT_HELD (1314).
//
//  2. THE SHIM IS A BASH SCRIPT. guardScriptTemplate starts `#!/bin/bash`. Windows has no /bin/bash,
//     so even a successfully created link would exec nothing.
//
//  3. PATH DOES NOT RESOLVE AN EXTENSIONLESS NAME. Windows appends PATHEXT (.COM;.EXE;.BAT;.CMD;…)
//     when searching PATH, so a file named `sudo` with no extension is not a candidate at all — the
//     shim would be invisible even if it were runnable.
//
// SO THE GUARD IS UNSUPPORTED HERE, and that is a fact to REPORT rather than a failure to propagate.
// The mistake was the severity: this error was returned from the host serve's start path, so a
// defense-in-depth backstop being unavailable DISABLED ASK CHAT ENTIRELY. Every other caller of this
// package already warns and continues (internal/runtime's supervisor, internal/askorchicon's bash
// path); the host serve was the one place that treated it as fatal.
//
// WHAT IS LOST, STATED PLAINLY: without the shim, a command issued from INSIDE a subprocess — a python
// script calling subprocess.run(["rm","-rf","/"]) — is not blocked at the OS layer. The layer above it
// is unaffected and platform-independent: opencode's own permission rules are generated from the same
// neverallow declaration (internal/opencode/interactivePermissionRules) and are injected into every
// serve's config, including on Windows. So the never-allow class is still denied and unmatched
// commands still default to `ask`. What is missing is the second line of defence.
//
// A REAL WINDOWS GUARD IS A SEPARATE PIECE OF WORK, not a port: the interception mechanism would have
// to be .cmd/.exe shims (PATHEXT-visible), the shim would have to be a compiled helper rather than
// bash, and the dangerous-binary list is different (del, rmdir, format, diskpart, rd, Remove-Item).
// Doing it badly would be worse than not doing it: a guard that silently matches nothing is the
// failure this whole component exists to prevent.

// supported reports whether the guard can be BUILT and FUNCTION on this platform.
func supported() bool { return false }

// unsupportedReason is the platform-specific explanation, empty when supported.
func unsupportedReason() string {
	return "the execution guard needs a bash shim plus symlinks, and Windows has neither: symlink " +
		"creation requires SeCreateSymbolicLinkPrivilege (an elevated token or Developer Mode), the " +
		"shim is a #!/bin/bash script, and PATH does not resolve an extensionless name. Commands are " +
		"still checked by opencode's own permission rules; only the OS-level subprocess backstop is absent"
}
