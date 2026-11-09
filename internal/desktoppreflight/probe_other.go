//go:build !linux && !darwin && !windows

package desktoppreflight

// osProbe on a platform Orchicon does not package a desktop app for.
func osProbe() Probe { return Probe{} }
