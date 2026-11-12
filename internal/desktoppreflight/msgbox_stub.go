//go:build !windows

package desktoppreflight

import (
	"fmt"
	"os"
)

// ShowBlocking on every non-Windows platform is a stderr write.
func ShowBlocking(r Report) {
	fmt.Fprintln(os.Stderr, r.String())
}
