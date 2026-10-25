//go:build windows

package opencode

// pathrefresh_windows.go — WHERE A LONG-LIVED PLANE FINDS A PATH IT WAS NOT STARTED WITH.
//
// REPORTED: "I reinstalled on windows and I am still getting this error ... opencode CLI should be
// installed on PATH now. I can run it from anywhere."
//
// Both halves of that report are true at once, and the gap is Windows-specific. The plane's
// environment is a SNAPSHOT taken when it was started:
//
//	cmdEnv := append(EnvWithout(os.Environ(), instanceEnvKeys...), env...)
//	                     ^ internal/planecontrol/planecontrol.go
//
// os.Environ() is the PATH the STARTING process had. On Windows that list lives in the registry
// (HKCU\Environment and the machine-wide equivalent) and is rebuilt at LOGON — so a process started
// before opencode was installed, or started by an installer that carried a pre-install PATH, keeps
// looking in the old places for as long as it lives. The operator's shell was started later and sees
// the new PATH, which is why "I can run it from anywhere" and "the plane cannot find it" are both
// true.
//
// THE FIX IS TO ASK THE REGISTRY, which is the source rather than one process's copy of it. This is
// what makes discovery self-healing WITHOUT a restart — the other half of that being the lazy
// re-resolution in internal/aigateway.
//
// BEST-EFFORT BY DESIGN: a key that cannot be read (a locked-down machine, a service account) yields
// no extra directories, and resolution falls back to exactly the behaviour it had before. Nothing
// here can make resolution WORSE.

import (
	"os"

	"golang.org/x/sys/windows/registry"
)

// registryPathDirs returns the directories named in the PERSISTED user and machine PATH, in that
// order (user first, mirroring how a shell resolves).
//
// IT IS APPENDED TO THE PROCESS PATH, NOT A REPLACEMENT FOR IT: a process whose PATH carries
// something the registry does not (an injected dev shell, a CI harness) keeps working.
func registryPathDirs() []string {
	var dirs []string
	// USER FIRST. On Windows the user's Path is what a package manager installs into without
	// elevation, so it is where a per-user opencode lives.
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
