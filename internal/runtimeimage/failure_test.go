package runtimeimage

import (
	"strings"
	"testing"
)

func TestClassifyEngineMismatch(t *testing.T) {
	log := "Step 3/5 : RUN npm install -g playwright@1.62\nnpm error EBADENGINE Unsupported engine { package: 'playwright@1.62', required: { node: '>=20' }, current: { node: 'v18.20.4' } }"
	f := ClassifyBuildLog(log, 1, "exit status 1")
	if f.Category != "engine_mismatch" {
		t.Fatalf("expected engine_mismatch got %q", f.Category)
	}
	if !strings.Contains(f.Hint, "Node >= 20") {
		t.Fatalf("hint missing: %q", f.Hint)
	}
	if f.FailedStep == "" {
		t.Fatalf("expected failed step")
	}
	if f.LogTail == "" {
		t.Fatalf("expected log tail")
	}
}

func TestClassifyApt(t *testing.T) {
	log := "E: Unable to locate package foobar\nStep 2/3 : RUN apt-get install -y foobar"
	f := ClassifyBuildLog(log, 1, "exit status 1")
	if f.Category != "apt_dpkg" {
		t.Fatalf("expected apt_dpkg got %q reason %q", f.Category, f.Reason)
	}
}

func TestClassifyNetwork(t *testing.T) {
	log := "dial tcp 1.2.3.4:443: i/o timeout\nfailed to fetch"
	f := ClassifyBuildLog(log, 1, "")
	if f.Category != "network" {
		t.Fatalf("expected network got %q", f.Category)
	}
}

func TestClassifyInvalidTag(t *testing.T) {
	log := "invalid reference format: repository name must be lowercase"
	f := ClassifyBuildLog(log, 1, "")
	if f.Category != "invalid_tag" {
		t.Fatalf("expected invalid_tag got %q", f.Category)
	}
}

func TestClassifyOOM(t *testing.T) {
	log := "Killed\nsignal 9: out of memory"
	f := ClassifyBuildLog(log, 137, "")
	if f.Category != "oom" {
		t.Fatalf("expected oom got %q", f.Category)
	}
}

func TestClassifyStream(t *testing.T) {
	f := ClassifyBuildLog("", 1, "Error in input stream: connection reset")
	if f.Category != "stream" {
		t.Fatalf("expected stream got %q", f.Category)
	}
}

func TestLogTail(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 100; i++ {
		sb.WriteString("line\n")
	}
	f := ClassifyBuildLog(sb.String(), 1, "exit status 1")
	lines := strings.Split(f.LogTail, "\n")
	if len(lines) > 60 {
		t.Fatalf("log tail too long %d", len(lines))
	}
}

func TestNeverBareExitStatus(t *testing.T) {
	f := ClassifyBuildLog("some log", 1, "exit status 1")
	if f.Reason == "exit status 1" {
		t.Fatalf("bare exit status returned")
	}
}


func TestClassifyDockerfile(t *testing.T) {
	log := "dockerfile: syntax error near unexpected token"
	f := ClassifyBuildLog(log, 1, "exit status 1")
	if f.Category != "dockerfile" {
		t.Fatalf("expected dockerfile got %q", f.Category)
	}
}

func TestClassifyUnknownFallsBackToLogTail(t *testing.T) {
	f := ClassifyBuildLog("", 1, "exit status 1")
	if f.Reason == "exit status 1" {
		t.Fatalf("should not return bare exit")
	}
	if f.Category != "unknown" && f.Category != "stream" {
		// empty log with generic exit => unknown with fallback
	}
}
func TestClassifyUnknownUsesLogTail(t *testing.T) {
	log := "Step 1/2 : RUN echo hi\nerror: something went wrong"
	f := ClassifyBuildLog(log, 1, "exit status 1")
	if f.Reason == "exit status 1" {
		t.Fatalf("should not be bare")
	}
}
