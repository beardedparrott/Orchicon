package planebind

// service_test.go — the binding's DECISIONS, without spawning a process.
//
// The behaviours asserted here are the ones the brief calls out as load-bearing:
//   * control goes through internal/planecontrol and NOWHERE else (no second path);
//   * an unidentifiable instance is REFUSED, never guessed;
//   * an unrecognised port resolves to NO instance (the UI must ask);
//   * an action never claims success over a plane it did not verify.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/planecontrol"
)

// withSeams replaces every seam for the duration of one test and restores them
// afterwards, so no test can leak a fake into the suite.
func withSeams(t *testing.T, start, stop, restart func(planecontrol.Options) error,
	status func(planecontrol.Options) (planecontrol.Status, error),
	probe func(string, time.Duration) planecontrol.Health) {
	t.Helper()
	oldStart, oldStop, oldRestart, oldStatus, oldProbe := startFn, stopFn, restartFn, statusFn, probeFn
	t.Cleanup(func() {
		startFn, stopFn, restartFn, statusFn, probeFn = oldStart, oldStop, oldRestart, oldStatus, oldProbe
	})
	if start != nil {
		startFn = start
	}
	if stop != nil {
		stopFn = stop
	}
	if restart != nil {
		restartFn = restart
	}
	if status != nil {
		statusFn = status
	}
	if probe != nil {
		probeFn = probe
	}
}

// TestStartNeverBypassesTheFailSafe proves the binding ADDS NO CONTROL PATH: a
// Start delegates exactly once to planecontrol, with the validated instance, and
// never reaches for Stop/Restart behind the fail-safe's back. The "leave an
// unidentifiable plane alone" behaviour itself is planecontrol's, already proven by
// internal/planecontrol's plane_unknown_test.go — and it can only stay true if this
// layer does not invent an alternative route to a restart.
func TestStartNeverBypassesTheFailSafe(t *testing.T) {
	var startOpts []planecontrol.Options
	stopCalls, restartCalls := 0, 0

	withSeams(t,
		func(o planecontrol.Options) error {
			startOpts = append(startOpts, o)
			_, _ = o.Out.Write([]byte("host plane ready: http://localhost:8080/healthz\n"))
			return nil
		},
		func(planecontrol.Options) error { stopCalls++; return nil },
		func(planecontrol.Options) error { restartCalls++; return nil },
		nil, nil,
	)

	svc := &Service{}
	dto, err := svc.Start("dev")
	if err != nil {
		t.Fatalf("Start(dev) returned error: %v", err)
	}
	if len(startOpts) != 1 {
		t.Fatalf("planecontrol.Start called %d times, want exactly 1", len(startOpts))
	}
	if startOpts[0].Instance != "dev" {
		t.Errorf("Start delegated for instance %q, want %q", startOpts[0].Instance, "dev")
	}
	if stopCalls != 0 || restartCalls != 0 {
		t.Fatalf("Start reached Stop(%d)/Restart(%d) — the binding must not invent a restart path", stopCalls, restartCalls)
	}
	if !dto.OK || dto.Outcome != "started" {
		t.Errorf("outcome = %+v, want OK started", dto)
	}
	if dto.Instance != "dev" {
		t.Errorf("the confirmation must name the instance, got %q", dto.Instance)
	}
}

// TestUnknownInstanceIsRefused — the name is validated by the shared CLI rule, and an
// unknown one is refused rather than silently defaulted to dev.
func TestUnknownInstanceIsRefused(t *testing.T) {
	statusCalls := 0
	withSeams(t, nil, nil, nil, func(planecontrol.Options) (planecontrol.Status, error) {
		statusCalls++
		return planecontrol.Status{}, nil
	}, nil)

	svc := &Service{}
	for _, name := range []string{"dev/../prod", "../prod", "", "nope"} {
		if _, err := svc.Status(name); err == nil {
			t.Errorf("Status(%q) accepted a name it must refuse", name)
		}
		if _, err := svc.Start(name); err == nil {
			t.Errorf("Start(%q) accepted a name it must refuse", name)
		}
	}
	if statusCalls != 0 {
		t.Errorf("a refused instance name still reached planecontrol.StatusOf %d times", statusCalls)
	}
}

// TestResolveControlPortNeverGuesses — an unrecognised port resolves to NO instance
// (the page must ask), and the known ports resolve to their own instance.
func TestResolveControlPortNeverGuesses(t *testing.T) {
	withSeams(t, nil, nil, nil, func(o planecontrol.Options) (planecontrol.Status, error) {
		return planecontrol.Status{Instance: o.Instance, ControlPort: 0}, nil
	}, nil)

	svc := &Service{}
	res, err := svc.ResolveControlPort(9999)
	if err != nil {
		t.Fatalf("ResolveControlPort(9999): %v", err)
	}
	if res.Known || res.Instance != "" {
		t.Fatalf("ResolveControlPort(9999) guessed %q (Known=%v) — an unknown port must prompt", res.Instance, res.Known)
	}

	for port, want := range map[int]string{8080: "dev", 8091: "prod", 8092: "test"} {
		got, err := svc.ResolveControlPort(port)
		if err != nil {
			t.Fatalf("ResolveControlPort(%d): %v", port, err)
		}
		if !got.Known || got.Instance != want {
			t.Errorf("ResolveControlPort(%d) = (%q, Known=%v), want (%q, true)", port, got.Instance, got.Known, want)
		}
	}
}

// TestResolveControlPortOffersThePickerWhenSeveralRun — with more than one plane up
// the operator chooses; the data lists each instance with its own state.
func TestResolveControlPortOffersThePickerWhenSeveralRun(t *testing.T) {
	withSeams(t, nil, nil, nil, func(o planecontrol.Options) (planecontrol.Status, error) {
		return planecontrol.Status{Instance: o.Instance, PID: 100, Drift: planecontrol.Current}, nil
	}, func(string, time.Duration) planecontrol.Health {
		return planecontrol.Health{Reachable: true, StatusCode: 200}
	})

	res, err := (&Service{}).ResolveControlPort(9999)
	if err != nil {
		t.Fatalf("ResolveControlPort: %v", err)
	}
	if len(res.Running) < 2 {
		t.Fatalf("Running lists %d instances, want at least 2 so a picker is offered", len(res.Running))
	}
}

// TestStatusReportsWhatItFound — connected vs offline, from the real classifier, with
// the probe seam standing in for the network.
func TestStatusReportsWhatItFound(t *testing.T) {
	withSeams(t, nil, nil, nil,
		func(o planecontrol.Options) (planecontrol.Status, error) {
			return planecontrol.Status{Instance: o.Instance, ControlPort: 8080, HealthURL: "http://localhost:8080/healthz", PID: 77, Drift: planecontrol.Current}, nil
		},
		func(string, time.Duration) planecontrol.Health { return planecontrol.Health{Reachable: false} },
	)

	dto, err := (&Service{}).Status("dev")
	if err != nil {
		t.Fatalf("Status(dev): %v", err)
	}
	if dto.State != StateOffline {
		t.Fatalf("state = %q, want %q", dto.State, StateOffline)
	}
	if !strings.Contains(dto.Detail, "http://localhost:8080/healthz") {
		t.Errorf("offline detail %q does not show the URL it tried", dto.Detail)
	}
	if dto.ControlPort != 8080 || dto.Instance != "dev" {
		t.Errorf("the DTO must name the instance and port, got %+v", dto)
	}
	if dto.LogPath == "" {
		t.Error("the DTO carries no log path — a failure must be able to name its evidence")
	}
}

// TestActionOutcomesAreWordedFromPlanecontrolsOwnReporting — the page reports
// started / already-running / left-alone-unidentified / failed, and never invents
// "started" for a plane it did not verify.
func TestActionOutcomesAreWordedFromPlanecontrolsOwnReporting(t *testing.T) {
	tests := []struct {
		name    string
		action  string
		output  string
		err     error
		want    string
		wantOK  bool
		wantLog bool
	}{
		{"started after a verified start", "start", "host plane ready: http://localhost:8080/healthz\n", nil, "started", true, false},
		{"already serving", "start", "host plane already serving http://localhost:8080/healthz (running the installed binary)\n", nil, "already-running", true, false},
		{"unidentifiable plane left alone", "start", "host plane already serving http://localhost:8080/healthz (could not identify the process; leaving it)\n", nil, "left-alone-unidentified", true, false},
		{"stop of a stopped plane", "stop", "serve is not running\n", nil, "already-stopped", true, false},
		{"stopped", "stop", "", nil, "stopped", true, false},
		{"start failed", "start", "", errors.New("the host plane did not answer http://localhost:8080/healthz within 60s"), "failed", false, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dto := actionDTO("dev", tc.action, tc.output, tc.err)
			if dto.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q", dto.Outcome, tc.want)
			}
			if dto.OK != tc.wantOK {
				t.Fatalf("OK = %v, want %v — success is claimed only over a verified plane", dto.OK, tc.wantOK)
			}
			if dto.Instance != "dev" {
				t.Errorf("the confirmation must name the instance, got %q", dto.Instance)
			}
			if tc.wantLog && !strings.Contains(dto.Message, dto.LogPath) {
				t.Errorf("a failure must name the log path; message %q does not contain %q", dto.Message, dto.LogPath)
			}
			if !tc.wantOK && dto.LogPath == "" {
				t.Error("a failure carries no log path")
			}
		})
	}
}
