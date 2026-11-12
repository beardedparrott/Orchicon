//go:build !windows

package adapter

// freshpath_other.go — THERE IS NO SINGLE AUTHORITATIVE PATH STORE ON POSIX, and this file says so
// rather than pretending otherwise.

import (
	"os"
	"strings"
)

func freshPathDirs() []string {
	var dirs []string
	dirs = append(dirs, readEtcPaths()...)
	dirs = append(dirs, readEtcEnvironmentPath()...)
	return dirs
}

// readEtcPaths reads macOS's ordered system list: one absolute directory per line, '#' comments.
func readEtcPaths() []string {
	b, err := os.ReadFile("/etc/paths")
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// readEtcEnvironmentPath pulls PATH= out of /etc/environment, which several distros use for a system-wide
// default. Only that one key is read: the file can hold credentials, and a probe has no business loading them.
func readEtcEnvironmentPath() []string {
	b, err := os.ReadFile("/etc/environment")
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != "PATH" {
			continue
		}
		v = strings.TrimSpace(v)
		// The file may quote the value, and the quotes are not part of it.
		v = strings.Trim(v, `"'`)
		var out []string
		for _, d := range strings.Split(v, ":") {
			if d = strings.TrimSpace(d); d != "" {
				out = append(out, d)
			}
		}
		return out
	}
	return nil
}
