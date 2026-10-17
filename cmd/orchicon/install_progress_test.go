package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/containerengine"
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
				"ghcr.io/beardedparrott/orchicon-runtime:latest", "/tmp/bin", ports, false, healthy, containerengine.Status{})
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

// TestPrintInstallInfoUsesTheInstallsEngineDecision pins the SINGLE-DECISION property the install asserts
// everywhere else: the carrier is decided ONCE (install.go's `containersUsed`) and every downstream surface
// reports THAT decision. printInstallInfo used to re-probe with containerEngineAvailable(), a SECOND decision
// that can disagree with the one the install acted on — so the summary could describe a carrier the install
// never created.
//
// The disagreement is made OBSERVABLE by construction: the install's decision is "containers ARE used" while
// the environment has NO engine on PATH, so a re-probe answers the opposite. The summary must follow the
// threaded decision (services in the instance container), never the re-probe (host processes of this user).
func TestPrintInstallInfoUsesTheInstallsEngineDecision(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no engine: a re-probe would answer "no containers"
	if containerEngineAvailable() {
		t.Fatal("the fixture needs an empty PATH to make the re-probe disagree")
	}
	ports := map[string]int{"ORCHICON_CONTROL_PORT": 8080, "ORCHICON_GRAFANA_PORT": 3002}
	out := captureStdout(t, func() {
		// residencyHost with containersUsed=true: a host plane whose services run in the instance container.
		printInstallInfo("dev", residencyHost, "orchicon-cnt-dev", "orchicon-cnt-dev-data",
			"/tmp/orchicon-runtime", "http://localhost:8080/healthz",
			"ghcr.io/beardedparrott/orchicon-runtime:latest", "/tmp/bin", ports, true, true, containerengine.Status{})
	})
	if !strings.Contains(out, "the services run in orchicon-cnt-dev") {
		t.Errorf("the summary must report the install's decision (services in the container); got:\n%s", out)
	}
	if strings.Contains(out, "HOST processes") {
		t.Errorf("the summary must NOT re-probe the engine and contradict the install: got:\n%s", out)
	}
}

// TestPrintInstallInfoIsHonestAboutTheNoEngineShape is the regression for a summary that CONTRADICTED its own
// install. A host with no container engine is a supported shape this change introduced, and the run announces
// it in step 1 ("runtime containers are unavailable (work runs in-process)") — but the summary's tail was
// unconditional, so the SAME run then reported a runtime daemon for per-workflow runtime containers and
// promised "every execution runs in its own short-lived container". On that host no runtime container can
// ever be launched, so the closing advice described a carrier the install never produced — the same class of
// defect as the dead `docker inspect` this item removes, arriving through the install's own output.
//
// BOTH DIRECTIONS ARE ASSERTED, because a summary gutted to the no-engine shape would stop reporting the
// container carrier where it IS the truth.
func TestPrintInstallInfoIsHonestAboutTheNoEngineShape(t *testing.T) {
	// No engine, so printInstallInfo's `containersUsed` is false — the install's threaded decision.
	t.Setenv("PATH", t.TempDir())
	ports := map[string]int{"ORCHICON_CONTROL_PORT": 8092, "ORCHICON_GRAFANA_PORT": 3004}

	noEngine := captureStdout(t, func() {
		printInstallInfo("test", residencyHost, "orchicon-cnt-test", "orchicon-cnt-test-data",
			"/tmp/orchicon-runtime", "http://localhost:8092/healthz",
			"ghcr.io/beardedparrott/orchicon-runtime:latest", "/tmp/bin", ports, false, true, containerengine.Status{})
	})
	if strings.Contains(noEngine, "short-lived container") {
		t.Errorf("a no-engine install must NOT promise a per-workflow runtime container it cannot launch:\n%s", noEngine)
	}
	if strings.Contains(noEngine, "runtime containers): running") {
		t.Errorf("a no-engine install must not report the container runtime daemon as the carrier:\n%s", noEngine)
	}
	if !strings.Contains(noEngine, "IN-PROCESS") {
		t.Errorf("the no-engine summary must say work runs in-process:\n%s", noEngine)
	}
	// The stop recipe must name the state dir: a bare `serve --stop` resolves a RELATIVE `.dev` and answers
	// "serve is not running" over a plane that is up.
	if !strings.Contains(noEngine, "ORCHICON_SERVE_STATE_DIR=") {
		t.Errorf("the host stop recipe must name the instance's serve state dir, or it stops nothing:\n%s", noEngine)
	}

	// The OTHER direction: with an engine the container-carrier lines must still be there.
	withEngine := captureStdout(t, func() {
		printInstallInfo("dev", residencyHost, "orchicon-cnt-dev", "orchicon-cnt-dev-data",
			"/tmp/orchicon-runtime", "http://localhost:8080/healthz",
			"ghcr.io/beardedparrott/orchicon-runtime:latest", "/tmp/bin", ports, true, true, containerengine.Status{})
	})
	if !strings.Contains(withEngine, "short-lived container") {
		t.Errorf("an engine-backed install must still describe the runtime container carrier:\n%s", withEngine)
	}
	if strings.Contains(withEngine, "IN-PROCESS") {
		t.Errorf("an engine-backed install must NOT say work runs in-process:\n%s", withEngine)
	}
}

// ----- the download failure posture (AC#6) -----

// TestDownloadFailureReportsABoundedTail mirrors TestTailWriterKeepsOnlyTheTail for
// the DOWNLOAD path. A failed download must report WHY, and the explanation must
// stay bounded: a proxy's error page or a huge HTML 404 printed whole would bury the
// actual failure in an unattended install's log.
func TestDownloadFailureReportsABoundedTail(t *testing.T) {
	// A body much larger than the tail limit, with a distinctive marker at the END —
	// which is the part a bounded tail must keep, since the last bytes are where a
	// server puts its actual explanation.
	const marker = "THE-ACTUAL-REASON"
	long := strings.Repeat("filler ", 4000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, long+marker)
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "download")
	opt := acquireOptions{Out: io.Discard, HTTPClient: srv.Client()}
	err := downloadToFile(context.Background(), srv.URL+"/x", dst, opt)
	if err == nil {
		t.Fatal("a 500 must be a failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "500") {
		t.Errorf("the error must carry the HTTP status; got:\n%s", msg)
	}
	if !strings.Contains(msg, marker) {
		t.Errorf("the error must carry the TAIL of the server's explanation; got:\n%s", msg)
	}
	if len(msg) > downloadTailLimit+2048 {
		t.Errorf("the reported tail must stay bounded (got %d bytes of error); the whole body must not be echoed", len(msg))
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Error("a failed download must not leave a file behind that a later step could mistake for the artifact")
	}
}

// TestDownloadProgressIsStreamedOncePerChunk pins the other half of AC#6: progress is
// STREAMED (so a working download is distinguishable from a hung one) but not per
// read (so it does not flood the log).
func TestDownloadProgressIsStreamedOncePerChunk(t *testing.T) {
	// 3.5 steps worth of body: enough for a few progress lines, far short of noisy.
	body := bytes.Repeat([]byte("x"), 3*downloadProgressStep+downloadProgressStep/2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	var out bytes.Buffer
	opt := acquireOptions{Out: &out, HTTPClient: srv.Client()}
	if err := downloadToFile(context.Background(), srv.URL+"/y", filepath.Join(t.TempDir(), "d"), opt); err != nil {
		t.Fatalf("download: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "MiB") {
		t.Errorf("progress must be reported as it happens, not only at the end; got:\n%s", got)
	}
	if lines := strings.Count(got, "\n"); lines > 8 {
		t.Errorf("progress must not print a line per read (%d lines for a few MiB); got:\n%s", lines, got)
	}
}
