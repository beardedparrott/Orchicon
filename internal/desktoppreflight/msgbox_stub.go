//go:build !windows

package desktoppreflight

import (
	"fmt"
	"os"
)

// ShowBlocking on every non-Windows platform is a stderr write. Status quo for the
// terminal-launched case, which is how the Linux and macOS builds are run out of
// the package: stderr already goes to the terminal the user launched from, and the
// launcher wrapper covers the double-click case where nothing would.
func ShowBlocking(r Report) {
	fmt.Fprintln(os.Stderr, r.String())
}
