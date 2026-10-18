//go:build windows

package main

// setprivSupported is FALSE: Windows has no setpriv and no equivalent, and its residency is never the
// container one. It is a compile-time constant rather than a runtime probe because a probe that fails would
// have to be handled at every call site, and the answer can never vary on a given platform.
//
// THE CONSEQUENCE IS NAMED, NOT HIDDEN: `asUser` returns the command verbatim, so a child on Windows runs with
// the supervisor's own token. That is the only thing it can do — silently claiming a privilege drop that did
// not happen is worse than not performing one on a platform where it cannot be performed.
const setprivSupported = false
