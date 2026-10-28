//go:build windows

package adapter

// freshpath_windows.go — THE PERSISTED WINDOWS PATH IS THE SOURCE, NOT THIS PROCESS'S COPY.
//
// A long-lived plane's environment is a SNAPSHOT taken when it started (internal/planecontrol builds the child
// environment from os.Environ()). On Windows the real list lives in the registry and is rebuilt at LOGON, so a
// plane started before an adapter was installed keeps looking in the old places for as long as it lives —
// while the operator's shell, started later, finds it. That is why "I reinstalled and it still says not found"
// and "I can run it from anywhere" are both true at once.
//
// READING THE REGISTRY is reading the source rather than one process's copy of it, which is what makes
// discovery self-healing without a restart.
//
// BEST-EFFORT BY DESIGN: a key that cannot be read (a locked-down machine, a service account) contributes
// nothing, and resolution falls back to exactly the behaviour it had before. Nothing here can make resolution
// worse.

import (
	"os"

	"golang.org/x/sys/windows/registry"
)

func freshPathDirs() []string {
	var dirs []string
	// USER FIRST. On Windows the user's Path is what a package manager installs into without elevation, so it
	// is where a per-user adapter lives — and a user entry overrides a machine one when the command is
	// resolved, so it must be searched in the same order.
	for _, src := range []struct {
		key  registry.Key
		path string
		name string
	}{
		{registry.CURRENT_USER, `Environment`, `Path`},
		{registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`, `Path`},
	} {
		k, err := registry.OpenKey(src.key, src.path, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		val, _, err := k.GetStringValue(src.name)
		k.Close()
		if err != nil {
			continue
		}
		dirs = append(dirs, SplitPersistedPath(val, os.Environ())...)
	}
	return dirs
}
