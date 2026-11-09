package runtime

// guard_reap_race_test.go — REAPING MUST NOT DISCARD A REFUSAL.
//
// cmd.Wait() closes the pipe READ ends. A child that exits fast leaves its output in the
// kernel buffer until a pump reads it, so reaping before the pumps have read THROWS THAT
// OUTPUT AWAY. An execution-guard refusal exits in about a millisecond, which makes it the
// worst case: the command reports a non-zero exit and did nothing, and the REASON is gone.
// That is a failed command with no explanation — a model cannot course-correct from it and
// an operator cannot see it.
//
// THE LOSS IS A SCHEDULING RACE, and how it behaves under test is MEASURED rather than
// assumed — because the obvious way to test it does not work. On this repo's own shape (a
// child that writes one line then exits 1), with the pump scheduled:
//
//	0ms after Wait  -> the message survived 50/50 runs
//	1ms after Wait  -> LOST in 26/50 runs
//	5ms after Wait  -> LOST in 50/50 runs
//
// So the loss needs about a millisecond of scheduling gap, and an IDLE machine does not
// produce one. NEGATIVE CONTROL, run against this file: with the fix's gate removed so the
// reaper runs first again, BOTH LOOPS BELOW STILL PASSED. They are therefore a
// load-dependent backstop, NOT the detector — under load (a full `go test ./...`, many
// packages in parallel) the gap exceeds a millisecond and they do bite, but on a quiet
// developer machine they cannot.
//
// THE DETECTOR IS TestTheReaperIsStartedAfterThePumps, which asserts the ORDER and fails
// immediately when it regresses (verified by the same negative control). That is why the
// wiring assertion exists alongside the behavioural ones rather than instead of them: a
// behavioural test for a race that a fast machine cannot lose is a detector that has stopped
// detecting, which is worse than no detector because it is trusted.
//
// The loops drive the REAL handlers (runStdio, runExec), with the child's PATH pinned
// shim-free by shimFreePath, so the shim that refuses is the one the handler under test
// installed. They are kept for the loaded-machine case and to exercise the end-to-end path.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// reapRaceIterations is the repeat count that makes a scheduling race observable. See the
// file comment for why one iteration is not enough.
const reapRaceIterations = 40

// execEvents drives runExec for one argv and returns the decoded event stream.
func execEvents(t *testing.T, req AgentRequest) []AgentEvent {
	t.Helper()
	req.Env = append(append([]string(nil), req.Env...), "PATH="+shimFreePath(t))
	h := newChildRegistry(slog.Default())
	var buf bytes.Buffer
	h.runExec(json.NewEncoder(&buf), req)

	var events []AgentEvent
	d := json.NewDecoder(&buf)
	for {
		var ev AgentEvent
		if err := d.Decode(&ev); err != nil {
			break
		}
		events = append(events, ev)
	}
	if len(events) == 0 {
		t.Fatalf("runExec produced no events for argv %v", req.Argv)
	}
	return events
}

// assertEveryRefusalNamesItself runs the same destructive command through one driver,
// reapRaceIterations times, and requires EVERY run to report the refusal AND the reason.
func assertEveryRefusalNamesItself(
	t *testing.T,
	cmd string,
	drive func(*testing.T, AgentRequest) []AgentEvent,
) {
	t.Helper()
	project := t.TempDir()
	outside := t.TempDir() // temp space ONLY

	for i := 0; i < reapRaceIterations; i++ {
		events := drive(t, AgentRequest{
			Cmd:        cmd,
			ExecID:     "exec-reap-race",
			Argv:       []string{"sh", "-c", "rm -rf " + outside},
			Cwd:        project,
			ProjectDir: project,
		})

		if code := stdioExitCode(t, events); code == 0 {
			t.Fatalf("iteration %d: the %s path RAN `rm -rf %s` (exit %d) — the guard is not applied",
				i, cmd, outside, code)
		}
		if _, err := os.Stat(outside); os.IsNotExist(err) {
			t.Fatalf("iteration %d: the destructive command DELETED %s — the guard did not refuse",
				i, outside)
		}
		// THE ASSERTION THIS FILE EXISTS FOR.
		if out := stdioStreamText(events); !strings.Contains(strings.ToUpper(out), "ORCHICON GUARD") {
			t.Fatalf("iteration %d: the %s path refused `rm -rf %s` but the reason was LOST (out=%q) — "+
				"the child's output was discarded by reaping before the pumps drained; see "+
				"watchExec/runExec in agent.go", i, cmd, outside, out)
		}
	}
}

func TestStdioGuardRefusalSurvivesTheReapRace(t *testing.T) {
	assertEveryRefusalNamesItself(t, "stdio", stdioEvents)
}

func TestExecGuardRefusalSurvivesTheReapRace(t *testing.T) {
	assertEveryRefusalNamesItself(t, "exec", execEvents)
}

// funcBody returns the source of one top-level function, so an ordering assertion can be scoped to
// it.
func funcBody(t *testing.T, src, decl string) string {
	t.Helper()
	i := strings.Index(src, decl)
	if i < 0 {
		t.Fatalf("agent.go no longer declares %q — re-establish the ordering assertion or "+
			"explain the replacement", decl)
	}
	rest := src[i+len(decl):]
	if j := strings.Index(rest, "\nfunc "); j >= 0 {
		return rest[:j]
	}
	return rest
}

// mustBeOrdered fails when the first needle does not appear before the second.
func mustBeOrdered(t *testing.T, body, first, second, why string) {
	t.Helper()
	i, j := strings.Index(body, first), strings.Index(body, second)
	if i < 0 || j < 0 {
		t.Fatalf("expected both %q (%d) and %q (%d) in the function body — if the shape was "+
			"rewritten, re-establish that the pipes are drained BEFORE reaping", first, i, second, j)
	}
	if i > j {
		t.Errorf("%s", why)
	}
}

// TestTheReaperIsStartedAfterThePumps pins the ORDER as a wiring property, in addition to the
// behaviour asserted above.
func TestTheReaperIsStartedAfterThePumps(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatalf("read agent.go: %v", err)
	}
	s := string(src)

	// runExec: the pumps must be drained (the gate armed) BEFORE cmd.Wait().
	execBody := funcBody(t, s, "func (h *childRegistry) runExec(")
	mustBeOrdered(t, execBody,
		"go func() { wg.Wait(); close(drained) }()",
		"werr := cmd.Wait()",
		"runExec reaps before the pumps have drained: cmd.Wait() closes the pipe read ends, "+
			"so a fast child's output — a guard refusal is ~1ms — is discarded")

	// runStdio: the pumps must be running (and the gate armed) BEFORE the reaper starts.
	stdioBody := funcBody(t, s, "func (h *childRegistry) runStdio(")
	mustBeOrdered(t, stdioBody,
		"go func() { wg.Wait(); close(pumpsDone) }()",
		"go h.watchExec(s)",
		"runStdio starts the reaper before the pumps gate is armed: watchExec's cmd.Wait() "+
			"can then close the pipes while the pumps are still only SCHEDULED")
	if !strings.Contains(stdioBody, "s.pumpsDone = pumpsDone") {
		t.Error("runStdio no longer attaches the pumps gate to the session, so watchExec " +
			"cannot wait for the pumps before reaping")
	}
}
