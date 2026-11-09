package runtime

// adapter_source_witness_test.go — THE ADAPTER'S argv[0] AND THE CONTAINER'S PATH PREFIX MUST AGREE.
//
// THE FAILURE THIS PREVENTS, which has now happened once. #898 changed the adapter to send the bare name
// `claude` into every container and left the daemon suppressing the host bin dir from the container's PATH,
// because "the image supplies it". Both halves were internally reasonable; together they removed every route
// to a working CLI on a host whose image lacked one, and the container died with
//
//	exec: "claude": executable file not found in $PATH
//
// The two answers are now derived from ONE gate, and this asserts the wiring rather than the behaviour: the
// behaviour is pinned by the tests either side, but nothing there would notice if one side stopped CONSULTING
// the gate and re-derived its own rule — which is exactly how the disagreement arrived.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stripGoComments removes line comments so an assertion cannot be satisfied by prose — the comment above this
// file names the same function, and a source gate that its own documentation satisfies proves nothing.
func stripGoComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// TestBothSidesReadTheSameHostPreferenceGate is the anti-drift witness.
func TestBothSidesReadTheSameHostPreferenceGate(t *testing.T) {
	cases := []struct {
		path string
		why  string
	}{
		{"../claude/binary.go", "the claude adapter decides the container's argv[0]"},
		{"bootprofile.go", "the daemon decides the container's PATH prefix"},
	}
	for _, c := range cases {
		body, err := os.ReadFile(filepath.FromSlash(c.path))
		if err != nil {
			t.Fatalf("read %s (%s): %v", c.path, c.why, err)
		}
		src := stripGoComments(string(body))
		if !strings.Contains(src, "adapter.PreferHostAdapterInContainer") {
			t.Errorf("%s no longer consults adapter.PreferHostAdapterInContainer, and it is where %s.\n\n"+
				"Both sides MUST read that one gate: when each derived its own rule they disagreed, and the "+
				"container was told to exec a name nothing on its PATH provided — the 0-token failure with no "+
				"stderr. If the decision moved, move BOTH callers with it.", c.path, c.why)
		}
	}
}

// AND NEITHER SIDE MAY REINTRODUCE A HARD-CODED BARE NAME FOR THE ADAPTER'S CONTAINER ROUTE, which is how the
// two drifted apart: argv() went to the image's name unconditionally while the prefix kept assuming the image.
func TestTheAdapterDoesNotHardCodeTheImageNameAsItsOnlyContainerAnswer(t *testing.T) {
	body, err := os.ReadFile(filepath.FromSlash("../claude/binary.go"))
	if err != nil {
		t.Fatalf("read ../claude/binary.go: %v", err)
	}
	src := stripGoComments(string(body))
	// The bare name is legitimate INSIDE the decision's fallback; it must not be the whole answer.
	i := strings.Index(src, "func ClaudeBinaryFor(")
	if i < 0 {
		t.Fatal("ClaudeBinaryFor is gone — the seam both transports go through")
	}
	fn := src[i:]
	end := strings.Index(fn[1:], "\nfunc ")
	if end > 0 {
		fn = fn[:end+1]
	}
	if !strings.Contains(fn, "claudeContainerArgv0") {
		t.Errorf("ClaudeBinaryFor no longer routes a container through claudeContainerArgv0, so the host\n"+
			"preference can be bypassed by returning the image's name directly:\n%s", fn)
	}
}
