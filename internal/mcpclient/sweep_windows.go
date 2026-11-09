//go:build windows

package mcpclient

import (
	"context"
	"log/slog"
)

// SweepStaleChildren is a NO-OP on Windows, and the build tag is what makes that true rather than a
// comment claiming it.
func SweepStaleChildren(_ context.Context, log *slog.Logger) {
	_ = log
}

// processAlive is the Windows half of the ownership check in plane_identity.go.
func processAlive(_ int) bool { return false }
