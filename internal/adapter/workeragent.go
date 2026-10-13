package adapter

import "strings"

// workeragent.go — the per-WORKER agent definition an opencode serve carries.
//
// IT LIVES IN THIS PACKAGE FOR THE IMPORT GRAPH, not because it is an adapter
// concern. Both consumers need it and neither can import the other: the opencode
// config builder writes the agents into the serve document, and `runtime` (the
// plane's lifecycle) is what resolves the run's step workers and hands them over
// — but internal/opencode imports internal/runtime, so runtime cannot name a
// type declared there. internal/adapter imports neither, and both import it, so
// it is the only package where the shared declaration can sit.

// WorkerAgent is one step worker's agent definition: the agent a session for
// that worker is switched to, carrying the worker's REAL system prompt.
//
// WHY IT EXISTS AT ALL. On 1.x the worker's system prompt rode every turn in
// the prompt body's `system` field. 2.x has no such field, so the prompt has to
// arrive through a channel opencode actually treats as a system prompt — and
// measurement says there is exactly one: the agent's `system`. Measured against
// a live 2.x serve, with an instruction forcing a distinctive reply:
//
//	agents.<n>.system          -> the forced reply   (the prompt IS in force)
//	session.instructions.entry -> ignored, on turn 1 AND after a step boundary
//	session.synthetic          -> admitted to the transcript, then ignored
//	prepended to the user text -> obeyed, but it is a USER message: it competes
//	                             with the user's own text instead of governing it
//
// An agent's `system` is baked per SERVE, while a prompt is per WORKER, and one
// serve hosts a whole run — so the serve carries one agent PER DISTINCT STEP
// WORKER and the execution selects its own by name.
//
// WHAT IT CAN HOLD IS BOUNDED BY THAT. Only the RUN-LEVEL layers of a worker's
// prompt are identical for every execution — the shared stable prefix and the
// worker's identity block — so only those may go in here. The task, the context
// files and the execution history vary per execution and must stay on the
// per-execution message.
//
// The constant `orchicon-worker` tool shell is ALWAYS emitted alongside these
// and remains `default_agent`: a session whose selection did not happen runs a
// worker with a thin prompt — never opencode's own `build` agent and its large
// default prompt.
type WorkerAgent struct {
	// Name is the agent id to select (WorkerAgentName).
	Name string
	// Prompt is the worker's real system prompt — the run-level part only.
	Prompt string
	// Model optionally pins the agent's model. Empty omits it: the v2 schema
	// validates the reference pattern, so "" is a validation failure.
	Model string
}

// WorkerAgentName derives the agent id for a step worker.
//
// DETERMINISTIC AND DERIVED, never transmitted: the plane writes the agents into
// the serve config and the execution selects one at start, and the two only
// agree if both compute the same name from the same worker id. Lowercased
// because an agent id is a lookup key (opencode derives ids from file names) and
// our worker ids are upper-case ULIDs.
func WorkerAgentName(workerID string) string {
	id := strings.ToLower(strings.TrimSpace(workerID))
	id = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		default:
			return '-'
		}
	}, id)
	return "orchicon-worker-" + id
}
