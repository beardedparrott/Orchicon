package runtime

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

// TestLiveClaudeMountAuth is the ONE env-gated live smoke for the claude
// adapter's host-credential path.
//
// TEST BUDGET: a real Claude session costs money (the operator's plan), so
// this NEVER runs in default CI. It is doubly gated — it requires
// ORCHICON_TEST_LIVE_CLAUDE=1 AND a runtime image whose claude CLI is on the
// mounted PATH (ORCHICON_TEST_LIVE_CLAUDE_IMAGE) AND a real host claude.ai
// login — and it is SKIPPED (never failed) when any prerequisite is absent.
//
// It exercises exactly the production wiring: the mounts + PATH from
// standardHostMountArgs for a claude-demanding run, mounted against the REAL
// HostHome, so a non-bare session authenticates with the operator's own
// mounted host login. It runs ONE prompt on the cheapest model with JSON
// output so the billed cost is captured in the test log, and removes the
// container in a cleanup.
func TestLiveClaudeMountAuth(t *testing.T) {
	if os.Getenv("ORCHICON_TEST_LIVE_CLAUDE") != "1" {
		t.Skip("set ORCHICON_TEST_LIVE_CLAUDE=1 to run the live claude auth smoke (a real, billed session)")
	}
	home := os.Getenv("HOME")
	if home == "" {
		t.Skip("no HOME to mount")
	}
	if st, err := os.Stat(filepath.Join(home, ".claude", ".credentials.json")); err != nil || st.IsDir() {
		t.Skip("host has no persisted claude.ai login (~/.claude/.credentials.json) to mount")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not available on this host")
	}
	image := os.Getenv("ORCHICON_TEST_LIVE_CLAUDE_IMAGE")
	if image == "" {
		t.Skip("set ORCHICON_TEST_LIVE_CLAUDE_IMAGE to a runtime image whose claude CLI is on the mounted PATH")
	}

	d := &Daemon{HostHome: home}
	name := "orchicon-live-claude-smoke"
	args := []string{"run", "--name", name}
	args = append(args, d.standardHostMountArgs(CreateRequest{AdapterKinds: []string{"claude"}, GitStrategy: "none"})...)
	args = append(args, image,
		"claude", "-p", "reply with the single word OK",
		"--model", "claude-haiku-4-5",
		"--output-format", "json",
	)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	out, err := exec.Command("docker", args...).CombinedOutput()
	body := strings.TrimSpace(string(out))
	if err != nil {
		if msg, ok := adapter.ClaudeAuthFailure(body); ok {
			t.Fatalf("live claude smoke reported unauthenticated — %s\noutput:\n%s", msg, body)
		}
		t.Fatalf("live claude smoke failed: %v\noutput:\n%s", err, body)
	}

	var res struct {
		Result       string  `json:"result"`
		TotalCostUSD float64 `json:"total_cost_usd"`
		IsError      bool    `json:"is_error"`
	}
	if jerr := json.Unmarshal([]byte(body), &res); jerr != nil {
		if msg, ok := adapter.ClaudeAuthFailure(body); ok {
			t.Fatalf("live claude smoke reported unauthenticated — %s\noutput:\n%s", msg, body)
		}
		t.Fatalf("live claude smoke produced unparseable output: %v\noutput:\n%s", jerr, body)
	}
	if res.IsError {
		t.Fatalf("live claude session reported an error: %s", body)
	}
	if !strings.Contains(strings.ToUpper(res.Result), "OK") {
		t.Fatalf("live claude session reply %q does not contain OK", res.Result)
	}
	t.Logf("LIVE CLAUDE MOUNT/AUTH SMOKE OK — one prompt on claude-haiku-4-5, billed cost %f USD", res.TotalCostUSD)
}
