//go:build windows

package mcpclient

import (
	"context"
	"log/slog"
)

// SweepStaleChildren is a NO-OP on Windows, and the build tag is what makes that true rather
// than a comment claiming it.
//
// The Unix implementation scans /proc for children marked ORCHICON_MCP_STDIO=1 whose parent has
// died, and kills their process group. None of that exists here: there is no /proc, and
// `syscall.Kill` / `syscall.ESRCH` are absent from the Windows syscall package — which is why the
// untagged file broke the RELEASE BUILD for windows/amd64 and windows/arm64 rather than merely
// degrading at runtime on those hosts.
//
// The orphan protection on this platform is the OTHER mechanism that was already here and is
// still correct: the job object / process-tree teardown around the child, plus the transport's
// own Close. The sweep is the belt-and-suspenders half (ADR-0008) and its absence costs
// robustness against a control plane killed uncleanly, not correctness.
//
// KEPT AS A REAL FUNCTION WITH THE SAME SIGNATURE rather than deleted, so the call site in
// Server.Run stays platform-free and a future Windows implementation has an obvious home.
func SweepStaleChildren(_ context.Context, log *slog.Logger) {
	_ = log
}
