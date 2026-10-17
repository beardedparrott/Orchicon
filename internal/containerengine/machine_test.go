package containerengine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const fakePodman = "/fake/bin/podman"

// TestMachineNotApplicableOnLinux is the state that must never be reported as a problem:
// Linux talks to the kernel, and the WSL installer path is Linux too. NOTHING may run — not
// even `podman machine list`, which does not exist there.
func TestMachineNotApplicableOnLinux(t *testing.T) {
	p := testProbe("linux", nil, map[string]string{"podman": fakePodman}, func(bin string, args []string) (string, error) {
		t.Fatalf("linux must not invoke podman at all, ran %s %v", bin, args)
		return "", nil
	})
	e := Engine{Name: EnginePodman, Bin: fakePodman, Version: "5.2.1"}
	state, reason, err := MachineStatus(context.Background(), p, e)
	if err != nil || state != MachineNotApplicable || reason != "" {
		t.Fatalf("linux: state=%q reason=%q err=%v", state, reason, err)
	}
	var out bytes.Buffer
	state, err = EnsureMachine(context.Background(), p, e, &out)
	if err != nil || state != MachineNotApplicable {
		t.Fatalf("ensure on linux: state=%q err=%v", state, err)
	}
	if out.Len() != 0 {
		t.Fatalf("linux needs no machine message, printed: %q", out.String())
	}
}

// TestMachineStatusStates pins the four machine states from fixtures: the JSON shapes are
// what `podman machine list --format json` actually prints, and the state decides the
// message the operator sees (AC3).
func TestMachineStatusStates(t *testing.T) {
	for _, c := range []struct {
		name    string
		fixture string
		want    MachineState
	}{
		{"no machine", "machine-list-empty.json", MachineAbsent},
		{"stopped", "machine-list-stopped.json", MachineStopped},
		{"running", "machine-list-running.json", MachineRunning},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := fixture(t, c.fixture)
			p := testProbe("darwin", nil, nil, func(_ string, args []string) (string, error) {
				if args[0] != "machine" || args[1] != "list" || args[3] != "json" {
					t.Fatalf("unexpected call: %v", args)
				}
				return body, nil
			})
			state, _, err := MachineStatus(context.Background(), p, Engine{Name: EnginePodman, Bin: fakePodman})
			if err != nil {
				t.Fatalf("status: %v", err)
			}
			if state != c.want {
				t.Fatalf("want %q, got %q", c.want, state)
			}
		})
	}
}

// machineScript is a fake podman: every `machine list` reports `listBody`, init/start succeed
// unless `failOn` matches, and `info` answers unless `infoFails`.
func machineScript(t *testing.T, listBody, failOn string, infoFails bool, calls *[]string) func(string, []string) (string, error) {
	return func(_ string, args []string) (string, error) {
		switch {
		case args[0] == "machine" && args[1] == "list":
			return listBody, nil
		case args[0] == "machine" && (args[1] == "init" || args[1] == "start"):
			*calls = append(*calls, args[1])
			if failOn == args[1] {
				return fixture(t, "machine-start-fail.txt"), errors.New("exit status 125")
			}
			return "", nil
		case args[0] == "info":
			if infoFails {
				return "Error: cannot connect to Podman", errors.New("exit status 125")
			}
			return "amd64", nil
		}
		t.Fatalf("unexpected call: %v", args)
		return "", nil
	}
}

// TestEnsureMachineAbsent is AC3's first state: no machine → init it, and say so before the
// (slow, downloading) command runs.
func TestEnsureMachineAbsent(t *testing.T) {
	var calls []string
	p := testProbe("darwin", nil, nil, machineScript(t, fixture(t, "machine-list-empty.json"), "", false, &calls))
	var out bytes.Buffer
	state, err := EnsureMachine(context.Background(), p, Engine{Name: EnginePodman, Bin: fakePodman}, &out)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if state != MachineRunning {
		t.Fatalf("want running after init, got %q", state)
	}
	if len(calls) != 1 || calls[0] != "init" {
		t.Fatalf("want exactly one `machine init`, got %v", calls)
	}
	msg := out.String()
	if !strings.Contains(msg, "no podman machine exists yet") {
		t.Fatalf("absent must have its own message:\n%s", msg)
	}
	if !strings.Contains(msg, "podman machine init --now") {
		t.Fatalf("absent must show the command it runs:\n%s", msg)
	}
}

// TestEnsureMachineStopped is AC3's second state: the machine exists but is down → start it.
func TestEnsureMachineStopped(t *testing.T) {
	var calls []string
	p := testProbe("darwin", nil, nil, machineScript(t, fixture(t, "machine-list-stopped.json"), "", false, &calls))
	var out bytes.Buffer
	state, err := EnsureMachine(context.Background(), p, Engine{Name: EnginePodman, Bin: fakePodman}, &out)
	if err != nil || state != MachineRunning {
		t.Fatalf("ensure: state=%q err=%v", state, err)
	}
	if len(calls) != 1 || calls[0] != "start" {
		t.Fatalf("want exactly one `machine start`, got %v", calls)
	}
	if msg := out.String(); !strings.Contains(msg, "stopped") || !strings.Contains(msg, "podman machine start") {
		t.Fatalf("stopped must have its own message:\n%s", msg)
	}
}

// TestEnsureMachineRunning is AC3's third state: already up → no command at all, and no
// `machine init`/`start`.
func TestEnsureMachineRunning(t *testing.T) {
	var calls []string
	p := testProbe("darwin", nil, nil, machineScript(t, fixture(t, "machine-list-running.json"), "", false, &calls))
	var out bytes.Buffer
	state, err := EnsureMachine(context.Background(), p, Engine{Name: EnginePodman, Bin: fakePodman}, &out)
	if err != nil || state != MachineRunning {
		t.Fatalf("ensure: state=%q err=%v", state, err)
	}
	if len(calls) != 0 {
		t.Fatalf("a running machine needs no command, ran %v", calls)
	}
	if msg := out.String(); !strings.Contains(msg, "is running") {
		t.Fatalf("running must say so:\n%s", msg)
	}
}

// TestEnsureMachineFailsToStart is AC3's fourth state, and AC6's guarantee: the failure
// carries the engine's own error plus two remedies, and it never claims the machine works.
func TestEnsureMachineFailsToStart(t *testing.T) {
	var calls []string
	p := testProbe("darwin", nil, nil, machineScript(t, fixture(t, "machine-list-empty.json"), "init", false, &calls))
	state, err := EnsureMachine(context.Background(), p, Engine{Name: EnginePodman, Bin: fakePodman}, nil)
	if err == nil {
		t.Fatal("a failed init must be an error")
	}
	if state != MachineFailed {
		t.Fatalf("want failed, got %q", state)
	}
	msg := err.Error()
	if !strings.Contains(msg, "failed to start") || !strings.Contains(msg, "qemu exited with status 1") {
		t.Fatalf("the engine's reason must be carried: %v", err)
	}
	if !strings.Contains(msg, "podman machine rm") || !strings.Contains(msg, EnginePinEnv+"=none") {
		t.Fatalf("a failed machine must offer both remedies: %v", err)
	}
}

// TestEnsureMachineStartSucceedsButEngineSilent is the "VM process exists is not a working
// engine" case: start exits 0 and `podman info` never answers, so the state stays failed.
func TestEnsureMachineStartSucceedsButEngineSilent(t *testing.T) {
	var calls []string
	p := testProbe("darwin", nil, nil, machineScript(t, fixture(t, "machine-list-stopped.json"), "", true, &calls))
	p.MachineTimeout = 40 * time.Millisecond
	p.PollInterval = time.Millisecond
	state, err := EnsureMachine(context.Background(), p, Engine{Name: EnginePodman, Bin: fakePodman}, nil)
	if err == nil || state != MachineFailed {
		t.Fatalf("an unreachable engine must be a failure: state=%q err=%v", state, err)
	}
	if !strings.Contains(err.Error(), "failed to start") {
		t.Fatalf("message: %v", err)
	}
}

// TestMachineQueryFailureIsFailed folds "we could not even ask" into the failed state with a
// reason, rather than returning an error that an install might treat as fatal.
func TestMachineQueryFailureIsFailed(t *testing.T) {
	p := testProbe("darwin", nil, nil, func(string, []string) (string, error) {
		return "Error: machine list unavailable", errors.New("exit status 125")
	})
	state, reason, err := MachineStatus(context.Background(), p, Engine{Name: EnginePodman, Bin: fakePodman})
	if err != nil {
		t.Fatalf("a query failure is data, not an error: %v", err)
	}
	if state != MachineFailed || !strings.Contains(reason, "machine list unavailable") {
		t.Fatalf("state=%q reason=%q", state, reason)
	}
}

// TestMachineNotApplicableForDocker: only podman has a machine; docker's engine is a daemon.
func TestMachineNotApplicableForDocker(t *testing.T) {
	p := testProbe("darwin", nil, nil, func(string, []string) (string, error) {
		t.Fatal("docker must not be asked about machines")
		return "", nil
	})
	if state, _, _ := MachineStatus(context.Background(), p, Engine{Name: EngineDocker, Bin: "/fake/docker"}); state != MachineNotApplicable {
		t.Fatalf("want not-applicable, got %q", state)
	}
}
