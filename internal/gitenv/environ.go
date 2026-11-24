package gitenv

import "os"

// defaultEnviron is the real process environment; separated so the var seam lives in one visible file
// rather than an unexported assignment buried among the logic.
func defaultEnviron() []string { return os.Environ() }
