//go:build !windows

package main

// setprivSupported gates the privilege drop at COMPILE time (setpriv_windows.go says why). On every non-Windows
// platform setpriv is either present or the residency branch already declines it, so the answer is true.
const setprivSupported = true
