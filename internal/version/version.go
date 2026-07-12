// Package version holds build-time version metadata for the Orchicon
// control plane binary. Values are injected via -ldflags at build time.
package version

import (
	"fmt"
	"runtime"
)

// Build-time variables overridden by the linker (-ldflags "-X ...").
var (
	gitCommit = "none"
	gitTag    = "dev"
	buildDate = "unknown"
)

// Info describes the running binary.
type Info struct {
	Commit    string `json:"commit"`
	Tag       string `json:"tag"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// Current returns version metadata for this build.
func Current() Info {
	return Info{
		Commit:    gitCommit,
		Tag:       gitTag,
		BuildDate: buildDate,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
}

// String formats version info for one-line logging.
func (i Info) String() string {
	return fmt.Sprintf("orchicon %s (commit=%s built=%s go=%s %s/%s)",
		i.Tag, i.Commit, i.BuildDate, i.GoVersion, i.OS, i.Arch)
}
