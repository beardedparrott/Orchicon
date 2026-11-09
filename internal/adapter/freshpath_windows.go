//go:build windows

package adapter

// freshpath_windows.go — THE PERSISTED WINDOWS PATH IS THE SOURCE, NOT THIS PROCESS'S COPY.

import (
	"os"

	"golang.org/x/sys/windows/registry"
)

func freshPathDirs() []string {
	var dirs []string
	// USER FIRST.
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
