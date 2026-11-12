//go:build windows

package main

// setprivSupported is FALSE: Windows has no setpriv and no equivalent, and its residency is never
// the container one.
const setprivSupported = false
