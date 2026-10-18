//go:build !linux && !darwin && !windows

package desktoppreflight

// osProbe on a platform Orchicon does not package a desktop app for. Evaluate
// fails closed for these GOOS values, so an empty capability set is correct: there
// is nothing here that could honestly answer "is a webview present".
func osProbe() Probe { return Probe{} }
