package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// These tests cover the two changes that make `orchicon install` legible while it
// runs. They need no Docker daemon, which is why they can exist here at all: the
// progress and the honesty of the summary are properties of the output, and the
// output is what the operator has during a multi-minute unattended install.

// captureStdout collects everything fn prints to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

// TestTailWriterKeepsOnlyTheTail pins the bounded tail that lets a failed
// `docker pull` still report WHY without holding an entire progress stream in
// memory. Streaming the pull is what makes a working install distinguishable from
// a hung one; the tail is what keeps the failure message useful.
func TestTailWriterKeepsOnlyTheTail(t *testing.T) {
	tw := &tailWriter{limit: 16}
	if n, err := tw.Write([]byte("0123456789")); n != 10 || err != nil {
		t.Fatalf("Write = (%d, %v), want (10, nil)", n, err)
	}
	if got := tw.String(); got != "0123456789" {
		t.Fatalf("below the limit the writer must keep everything, got %q", got)
	}
	if _, err := tw.Write([]byte("abcdefghij")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, want := tw.String(), "456789abcdefghij"; got != want {
		t.Fatalf("tail = %q, want the LAST 16 bytes (%q)", got, want)
	}
	if n := len(tw.String()); n != 16 {
		t.Fatalf("the tail must stay bounded at 16 bytes, got %d", n)
	}
}

// TestPrintInstallInfoIsHonestAboutAnUnconfirmedPlane closes the "green but
// broken" case in the summary. An install whose control plane never answered used
// to print "Orchicon is installed and running." and nothing else, so the operator
// opened a URL that did not respond with nothing to go on. The install IS real, so
// the URLs stay; the claim about the plane does not.
func TestPrintInstallInfoIsHonestAboutAnUnconfirmedPlane(t *testing.T) {
	ports := map[string]int{"ORCHICON_CONTROL_PORT": 8080, "ORCHICON_GRAFANA_PORT": 3002}
	call := func(healthy bool) string {
		return captureStdout(t, func() {
			printInstallInfo("dev", residencyContainer, "orchicon-cnt-dev", "orchicon-cnt-dev-data",
				"/tmp/orchicon-runtime", "http://localhost:8080/healthz",
				"ghcr.io/beardedparrott/orchicon-runtime:latest", "/tmp/bin", ports, healthy)
		})
	}

	up := call(true)
	if !strings.Contains(up, "installed and running") {
		t.Errorf("a confirmed install must still say it is running:\n%s", up)
	}
	down := call(false)
	if strings.Contains(down, "installed and running") {
		t.Errorf("an unconfirmed plane must NOT be reported as running:\n%s", down)
	}
	if !strings.Contains(down, "control plane is not") {
		t.Errorf("an unconfirmed plane must say so:\n%s", down)
	}
	if !strings.Contains(down, "http://localhost:8080") {
		t.Errorf("the URLs must still be printed — the instance is real, the plane is not up:\n%s", down)
	}

	// The box is hand-built padding, so keep it rectangular: a line one character
	// too long is invisible in a diff and obvious on screen.
	for name, out := range map[string]string{"healthy": up, "unconfirmed": down} {
		widths := map[int]int{}
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "┌") || strings.HasPrefix(line, "│") || strings.HasPrefix(line, "└") {
				widths[utf8.RuneCountInString(line)]++
			}
		}
		if len(widths) != 1 {
			var got []string
			for n, count := range widths {
				got = append(got, fmt.Sprintf("%d chars x%d", n, count))
			}
			t.Errorf("%s: the summary box is not rectangular (%s):\n%s", name, strings.Join(got, ", "), out)
		}
	}
}
