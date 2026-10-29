package main

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/containerengine"
)

// TestSummaryNeverNamesAMissingLauncher is the outward-facing claim: the box a person reads must not name a
// launcher that is not there, and must name it when it is.
func TestSummaryNeverNamesAMissingLauncher(t *testing.T) {
	missing := orchLauncher{Path: "/home/u/.local/bin/orch", Problem: "could not place it"}
	out := captureStdout(t, func() {
		printInstallInfo("dev", residencyHost, "n", "v", "/tmp/s", "http://localhost:8080/healthz",
			"img", "/home/u/.local/bin", missing, map[string]int{}, false, true, containerengine.Status{})
	})
	if strings.Contains(out, "orch launcher: /home/u/.local/bin/orch (remote TUI client") {
		t.Fatalf("the summary claimed a launcher that was not installed:\n%s", out)
	}
	if !strings.Contains(out, "NOT installed") {
		t.Fatalf("the summary did not say the launcher is missing:\n%s", out)
	}
	if !strings.Contains(out, "could not place it") {
		t.Fatalf("the summary did not carry the reason:\n%s", out)
	}
}
