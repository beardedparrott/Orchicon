package planecontrol

// plane_unknown_test.go — AC4: the fail-safe survives the move. A plane that cannot be POSITIVELY identified
// is left alone; only a plane this layer can prove is running a superseded binary is restarted.

import (
	"bytes"
	"strings"
	"testing"
)

func TestStartLeavesAnUnidentifiablePlaneAlone(t *testing.T) {
	f := newFakeSeams(t)
	f.healthy = true
	f.drift = Unknown
	ports := map[string]int{"ORCHICON_CONTROL_PORT": 18111}
	var out bytes.Buffer

	if err := Start(Options{Instance: "dev", Out: &out, Ports: ports, HostDataDir: t.TempDir()}); err != nil {
		t.Fatalf("Start on an unidentifiable plane must SUCCEED (leave it alone), got: %v", err)
	}
	if len(f.runs) != 0 {
		t.Fatalf("no stop and no re-exec may happen for a plane we cannot identify; ran %v", f.runs)
	}
	if f.aliveCall != 0 {
		t.Errorf("no stop verification may run for a plane we cannot identify (planeAlive called %d times)", f.aliveCall)
	}
	if !strings.Contains(out.String(), "leaving it") {
		t.Errorf("the output must say the plane is being left alone; got:\n%s", out.String())
	}
}

func TestStartRestartsADriftedPlane(t *testing.T) {
	f := newFakeSeams(t)
	f.healthy = true
	f.drift = Drifted
	ports := map[string]int{"ORCHICON_CONTROL_PORT": 18112}

	if err := Start(Options{Instance: "dev", Out: nil, Ports: ports, HostDataDir: t.TempDir()}); err != nil {
		t.Fatalf("Start on a drifted plane: %v", err)
	}
	if len(f.runs) != 2 {
		t.Fatalf("a drifted plane must be stopped and then started; got runs: %v", f.runs)
	}
	if got := strings.Join(f.runs[0].args, " "); got != "serve --stop" {
		t.Errorf("the first command must be the graceful stop, got %q", got)
	}
	if got := strings.Join(f.runs[1].args, " "); got != "serve --detach" {
		t.Errorf("the second command must be the detached start, got %q", got)
	}
}

func TestStartIsANoOpOnACurrentPlane(t *testing.T) {
	f := newFakeSeams(t)
	f.healthy = true
	f.drift = Current
	ports := map[string]int{"ORCHICON_CONTROL_PORT": 18113}

	if err := Start(Options{Instance: "dev", Out: nil, Ports: ports, HostDataDir: t.TempDir()}); err != nil {
		t.Fatalf("Start on a current plane: %v", err)
	}
	if len(f.runs) != 0 || f.aliveCall != 0 {
		t.Errorf("a healthy plane on the installed binary must be left completely alone; runs=%v aliveCalls=%d", f.runs, f.aliveCall)
	}
}
