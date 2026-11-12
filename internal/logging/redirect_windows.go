//go:build windows

package logging

import "os"

// RedirectStdStreams is a no-op on native Windows.
func RedirectStdStreams(f *os.File) {}
