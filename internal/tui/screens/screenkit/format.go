package screenkit

import (
	"fmt"
	"time"
)

// Shared field formatters for detail panes (all screens import these so
// timestamps and numbers render identically).

// FmtTime renders a protobuf timestamp (nil → "—").
func FmtTime(ts interface{ AsTime() time.Time; IsValid() bool }) string {
	if ts == nil || !ts.IsValid() {
		return "—"
	}
	return ts.AsTime().UTC().Format("2006-01-02 15:04")
}

// FmtInt renders an integer.
func FmtInt(n int) string { return fmt.Sprintf("%d", n) }

// FmtInt64 renders an int64.
func FmtInt64(n int64) string { return fmt.Sprintf("%d", n) }

// FmtBool renders a bool as yes/no.
func FmtBool(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// FmtDuration renders seconds as a compact duration ("90s", "2m30s").
func FmtDuration(seconds int64) string {
	if seconds <= 0 {
		return "—"
	}
	d := time.Duration(seconds) * time.Second
	return d.String()
}
