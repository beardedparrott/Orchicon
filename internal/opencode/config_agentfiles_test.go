package opencode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
)

// THE SERVE CONFIG IS HANDED TO opencode IN ONE ENVIRONMENT STRING, and Linux refuses to exec a process
// whose single env string exceeds 128 KiB: five full worker prompts made the config 146,269 bytes and
// every runtime run died with "fork/exec …/opencode: argument list too long". These tests pin that the
// prompts travel as FILES and the config stays small however many workers there are.

func bigPrompt(tag string, n int) string {
	return strings.Repeat(tag+` line with "quotes", back\slash, {json: [1,2]} and unicode ✓é`+"\n", n)
}

func fiveWorkers() []adapter.WorkerAgent {
	var ws []adapter.WorkerAgent
	for _, id := range []string{"01WORKERAAA", "01WORKERBBB", "01WORKERCCC", "01WORKERDDD", "01WORKEREEE"} {
		ws = append(ws, adapter.WorkerAgent{Name: adapter.WorkerAgentName(id), Prompt: bigPrompt(id, 300)})
	}
	return ws
}

func TestWorkerPromptsTravelAsFilesAndConfigStaysSmall(t *testing.T) {
	withShape(t, ConfigShapeV2, func() {
		dir := filepath.Join(t.TempDir(), ".orchicon", "run1", "agents")
		ws := fiveWorkers()

		inline := BuildConfigContent(ConfigOptions{AgentName: workerAgent, AgentPrompt: sessionToolShell, DefaultAgent: workerAgent, Workers: ws})
		out := BuildConfigContent(ConfigOptions{AgentName: workerAgent, AgentPrompt: sessionToolShell, DefaultAgent: workerAgent, Workers: ws, AgentFilesDir: dir})

		if len(inline) < 100000 {
			t.Fatalf("test premise broken: inline config is only %d bytes — the prompts are too small to model the overflow", len(inline))
		}
		if len(out) > 20000 {
			t.Fatalf("config with AgentFilesDir is %d bytes; the prompts must not ride the config text", len(out))
		}

		agents := decodeDoc(t, out)["agents"].(map[string]any)
		for _, w := range ws {
			sys, _ := agents[w.Name].(map[string]any)["system"].(string)
			if !strings.HasPrefix(sys, "{file:"+dir+string(filepath.Separator)) || !strings.HasSuffix(sys, ".md}") {
				t.Fatalf("agent %q system = %q, want a {file:%s/…md} reference", w.Name, sys, dir)
			}
			path := strings.TrimSuffix(strings.TrimPrefix(sys, "{file:"), "}")
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("agent %q: referenced file not written: %v", w.Name, err)
			}
			if string(got) != w.Prompt {
				t.Fatalf("agent %q: file content differs from the worker's prompt", w.Name)
			}
		}
	})
}

// An edited prompt MUST change the reference text: the runtime pool keys a warm container on the config
// text, so an unchanged reference would reuse a container whose opencode loaded the OLD prompt.
func TestAgentPromptRefChangesWithContent(t *testing.T) {
	dir := t.TempDir()
	a := agentPromptRef(dir, adapter.WorkerAgent{Name: "orchicon-worker-x", Prompt: "one"})
	b := agentPromptRef(dir, adapter.WorkerAgent{Name: "orchicon-worker-x", Prompt: "two"})
	c := agentPromptRef(dir, adapter.WorkerAgent{Name: "orchicon-worker-x", Prompt: "one"})
	if a == b {
		t.Fatalf("different prompts produced the same reference %q", a)
	}
	if a != c {
		t.Fatalf("identical prompts produced different references: %q vs %q", a, c)
	}
}

// opencode expands {env:X} / {file:X} in INLINE text (a dangling {file:} fails the whole config), but
// never in a referenced file's contents — so a prompt that merely mentions the tokens must arrive
// byte-for-byte through the file.
func TestPromptWithSubstitutionTokensIsStoredVerbatim(t *testing.T) {
	dir := t.TempDir()
	p := "use {env:HOME} and {file:nope} literally\n"
	ref := agentPromptRef(dir, adapter.WorkerAgent{Name: "orchicon-worker-y", Prompt: p})
	path := strings.TrimSuffix(strings.TrimPrefix(ref, "{file:"), "}")
	got, err := os.ReadFile(path)
	if err != nil || string(got) != p {
		t.Fatalf("file = %q, %v; want the prompt verbatim", got, err)
	}
}

// With no directory the behaviour is the old one (host serve, Ask, tests): the prompt is inline.
func TestAgentPromptRefInlineWithoutDir(t *testing.T) {
	if got := agentPromptRef("", adapter.WorkerAgent{Name: "n", Prompt: "P"}); got != "P" {
		t.Fatalf("got %q, want inline prompt", got)
	}
}

// A directory that cannot be created must not stop the worker: fall back to inline text.
func TestAgentPromptRefFallsBackInlineWhenDirUnwritable(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A path UNDER a regular file can never be created.
	got := agentPromptRef(filepath.Join(blocker, "agents"), adapter.WorkerAgent{Name: "n", Prompt: "P"})
	if got != "P" {
		t.Fatalf("got %q, want the inline fallback", got)
	}
}

// The production entry point sets the directory: <project root>/.orchicon/<run>/agents, resolved to the
// project root when the serve is built for a run worktree.
func TestRuntimeServeConfigPutsPromptsUnderRunStateDir(t *testing.T) {
	withShape(t, ConfigShapeV2, func() {
		root := t.TempDir()
		wt := filepath.Join(root, ".orchicon-worktrees", "run42")
		out := RuntimeServeConfig("orchicon-runtime:dev", wt, "run42", nil, mcpclient.Resolution{}, nil, fiveWorkers())
		if len(out) > 20000 {
			t.Fatalf("RuntimeServeConfig is %d bytes with five large worker prompts; want it small", len(out))
		}
		entries, err := os.ReadDir(filepath.Join(root, ".orchicon", "run42", "agents"))
		if err != nil || len(entries) != 5 {
			t.Fatalf("agents dir = %d entries, %v; want 5 prompt files", len(entries), err)
		}
	})
}
