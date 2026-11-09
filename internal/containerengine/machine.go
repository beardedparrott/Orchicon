package containerengine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// MachineState is the state of the `podman machine` VM.
type MachineState string

const (
	// MachineNotApplicable is Linux, or an engine that has no machine concept.
	MachineNotApplicable MachineState = "not-applicable"
	// MachineAbsent means podman is installed and no VM has ever been created.
	MachineAbsent MachineState = "absent"
	// MachineStopped means the VM exists and is not running.
	MachineStopped MachineState = "stopped"
	// MachineRunning means the VM reports Running.
	MachineRunning MachineState = "running"
	// MachineFailed means the machine could not be created, started, or reached.
	MachineFailed MachineState = "failed"
)

const (
	defaultMachineTimeout = 60 * time.Second
	defaultPollInterval   = time.Second
)

// machineListEntry is the subset of `podman machine list --format json` we rely on.
type machineListEntry struct {
	Name    string `json:"Name"`
	Running bool   `json:"Running"`
}

// MachineStatus reports the VM state with the reason a state matters, so each of the four states
// can produce its own actionable message (AC3).
func MachineStatus(ctx context.Context, p Probe, e Engine) (MachineState, string, error) {
	if e.Name != EnginePodman {
		return MachineNotApplicable, "", nil
	}
	switch p.goos() {
	case "darwin", "windows":
	default:
		// Linux: containers talk to the kernel and `podman machine` does not exist.
		return MachineNotApplicable, "", nil
	}

	out, err := p.Run(ctx, e.Bin, "machine", "list", "--format", "json")
	if err != nil {
		reason := strings.TrimSpace(out)
		if reason == "" {
			reason = err.Error()
		}
		return MachineFailed, fmt.Sprintf("the podman machine could not be queried: %s", firstLine(reason)), nil
	}

	var entries []machineListEntry
	trimmed := strings.TrimSpace(out)
	if trimmed != "" {
		if jerr := json.Unmarshal([]byte(trimmed), &entries); jerr != nil {
			// 4.x printed "[]" for "no machines"; a bare `null` also means none. Anything
			// else is a shape we do not understand, which is reported rather than guessed.
			return MachineFailed, fmt.Sprintf("unexpected `podman machine list` output: %s", firstLine(trimmed)), nil
		}
	}
	if len(entries) == 0 {
		return MachineAbsent, "no podman machine exists yet", nil
	}
	names := make([]string, 0, len(entries))
	for _, ent := range entries {
		if ent.Running {
			return MachineRunning, fmt.Sprintf("the podman machine %q is running", ent.Name), nil
		}
		names = append(names, ent.Name)
	}
	return MachineStopped, fmt.Sprintf("the podman machine %q exists but is stopped", strings.Join(names, ", ")), nil
}

// EnsureMachine brings the VM to a usable state, with ONE distinct, actionable message per state
// (AC3).
func EnsureMachine(ctx context.Context, p Probe, e Engine, out io.Writer) (MachineState, error) {
	state, reason, _ := MachineStatus(ctx, p, e)
	if out == nil {
		out = io.Discard
	}
	switch state {
	case MachineNotApplicable:
		return state, nil

	case MachineAbsent:
		fmt.Fprintln(out, "orchicon: no podman machine exists yet — creating one (this downloads a one-time VM image, a few hundred MB)")
		fmt.Fprintln(out, "orchicon: running: podman machine init --now")
		if o, err := p.Run(ctx, e.Bin, "machine", "init", "--now"); err != nil {
			return MachineFailed, machineFailure(err, o)
		}

	case MachineStopped:
		fmt.Fprintf(out, "orchicon: %s — starting it\n", reason)
		fmt.Fprintln(out, "orchicon: running: podman machine start")
		if o, err := p.Run(ctx, e.Bin, "machine", "start"); err != nil {
			return MachineFailed, machineFailure(err, o)
		}

	case MachineRunning:
		fmt.Fprintf(out, "orchicon: %s\n", reason)

	case MachineFailed:
		// Already broken before we touched it: report the reason and the two remedies, and do
		// not try to start something we could not even enumerate.
		return MachineFailed, machineFailure(nil, reason)
	}

	if err := waitReachable(ctx, p, e.Bin); err != nil {
		return MachineFailed, machineFailure(err, "the podman machine did not answer within the timeout")
	}
	return MachineRunning, nil
}

// machineFailure is the failed-to-start message: what happened, then what to do about it.
func machineFailure(err error, detail string) error {
	detail = firstLine(strings.TrimSpace(detail))
	if detail == "" && err != nil {
		detail = firstLine(err.Error())
	}
	if detail == "" {
		detail = "no output"
	}
	return fmt.Errorf("the podman machine failed to start: %s — remove it and retry with `podman machine rm` then re-run `orchicon install`, or set ORCHICON_CONTAINER_ENGINE=none to install in local mode", detail)
}

// waitReachable polls `podman info`, which is the cheapest call that proves the ENGINE answers (not
// merely that a VM process exists).
func waitReachable(ctx context.Context, p Probe, bin string) error {
	timeout := p.MachineTimeout
	if timeout <= 0 {
		timeout = defaultMachineTimeout
	}
	interval := p.PollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	sleep := p.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for attempt := 0; ; attempt++ {
		out, err := p.Run(ctx, bin, "info", "--format", "{{.Host.Arch}}")
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("%s", firstLine(strings.TrimSpace(out)))
		if lastErr.Error() == "" {
			lastErr = err
		}
		if !time.Now().Before(deadline) || attempt >= 600 {
			if lastErr == nil {
				lastErr = fmt.Errorf("timed out after %s", timeout)
			}
			return lastErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		sleep(interval)
	}
}

// firstLine keeps a multi-line engine error printable in a one-line install message while
// still carrying the first thing the engine said.
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
