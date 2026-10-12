package opencode

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestV2RegistersOneAgentPerWorker pins the mechanism the per-execution prompt
// now depends on: each step worker becomes an agent whose `system` is that
// worker's REAL prompt, while the constant tool shell remains the default.
//
// The default matters as much as the per-worker entries: a session that never
// gets switched must still land on a worker agent, because the alternative is
// opencode's own `build` agent and its large default prompt.
func TestV2RegistersOneAgentPerWorker(t *testing.T) {
	withShape(t, ConfigShapeV2, func() {
		out := BuildConfigContent(ConfigOptions{
			AgentName:    workerAgent,
			AgentPrompt:  sessionToolShell,
			DefaultAgent: workerAgent,
			Workers: []WorkerAgent{
				{Name: WorkerAgentName("01M1ERH9921YS9S1QWGZV8D1VM"), Prompt: "REAL WORKER PROMPT A"},
				{Name: WorkerAgentName("01M13DYHKHEF71MVGY07GMGMJ6"), Prompt: "REAL WORKER PROMPT B", Model: "opencode/some-model"},
			},
		})
		doc := decodeDoc(t, out)
		agents, ok := doc["agents"].(map[string]any)
		if !ok {
			t.Fatalf("no agents block: %v", doc)
		}
		for _, want := range []string{
			"orchicon-worker-01m1erh9921ys9s1qwgzv8d1vm",
			"orchicon-worker-01m13dyhkhef71mvgy07gmgmj6",
		} {
			e, ok := agents[want].(map[string]any)
			if !ok {
				t.Fatalf("agent %q missing from %v", want, agents)
			}
			if sys, _ := e["system"].(string); !strings.HasPrefix(sys, "REAL WORKER PROMPT") {
				t.Errorf("agent %q system = %q, want the worker's real prompt", want, sys)
			}
			if e["mode"] != "primary" {
				t.Errorf("agent %q mode = %v, want primary", want, e["mode"])
			}
		}
		shell, ok := agents[workerAgent].(map[string]any)
		if !ok {
			t.Fatalf("the tool shell agent must still be registered: %v", agents)
		}
		if sys, _ := shell["system"].(string); sys != sessionToolShell {
			t.Error("the shell agent must keep the constant tool shell")
		}
		if got, _ := doc["default_agent"].(string); got != workerAgent {
			t.Errorf("default_agent = %q, want the shell %q — a session that is never switched must not fall to `build`", got, workerAgent)
		}
	})
}

// TestV2WorkerAgentsAreV2Only pins that the per-worker agents do NOT leak into a
// v1 document. On 1.x the same prompt rides the prompt body's `system` field per
// message, so emitting agents there would be redundant config 1.x has no use for
// — and the 1.x document must stay byte-identical.
func TestV2WorkerAgentsAreV2Only(t *testing.T) {
	doc := decodeDoc(t, BuildConfigContent(ConfigOptions{
		AgentName:    workerAgent,
		AgentPrompt:  sessionToolShell,
		DefaultAgent: workerAgent,
		Workers:      []WorkerAgent{{Name: WorkerAgentName("01M1ERH9921YS9S1QWGZV8D1VM"), Prompt: "REAL WORKER PROMPT A"}},
	}))
	if _, ok := doc["agents"]; ok {
		t.Error("a v1 document must not carry the `agents` block")
	}
	if _, ok := doc["agent"]; !ok {
		t.Error("a v1 document must carry the v1 `agent` block")
	}
}

// TestWorkerAgentNameIsDerivedAndStable pins the contract the plane and the
// execution both depend on: the name is COMPUTED from the worker id, so the
// writer and the selector agree without transmitting it.
func TestWorkerAgentNameIsDerivedAndStable(t *testing.T) {
	const id = "01M1ERH9921YS9S1QWGZV8D1VM"
	got := WorkerAgentName(id)
	if got != WorkerAgentName(id) {
		t.Error("WorkerAgentName must be deterministic")
	}
	if got != "orchicon-worker-01m1erh9921ys9s1qwgzv8d1vm" {
		t.Errorf("WorkerAgentName(%q) = %q", id, got)
	}
	if r := WorkerAgentName("Weird/ID With Spaces"); strings.ContainsAny(r, "/ ") {
		t.Errorf("an agent id must survive a lookup key: %q", r)
	}
	if r := WorkerAgentName(""); r != "orchicon-worker-" {
		t.Logf("empty worker id yields %q (the caller skips an empty name)", r)
	}
}

// TestV2WorkerAgentOmitsAnEmptyModel pins the schema constraint: an empty model
// is a validation failure as a string and must be omitted, not emitted as "".
func TestV2WorkerAgentOmitsAnEmptyModel(t *testing.T) {
	entry := v2AgentEntry("a prompt", "")
	if _, ok := entry["model"]; ok {
		t.Error("an empty model must be omitted")
	}
	b, _ := json.Marshal(entry)
	if strings.Contains(string(b), "\"model\"") {
		t.Errorf("marshalled entry still carries a model key: %s", b)
	}
}
