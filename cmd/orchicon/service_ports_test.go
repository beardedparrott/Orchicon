package main

// service_ports_test.go — the crash-loop diagnosis must name the port holder, and must never act on
// it. The Windows netstat parser has its own test (zz_netstat_probe_test.go, windows-tagged).

import (
	"log/slog"
	"strings"
	"testing"
)

// componentPorts MUST READ THE SAME TABLE THE CHILD BINDS.
//
// A diagnosis that names the wrong port is worse than none: it sends the operator to inspect a
// process that is innocent while the real conflict keeps crashing the child. So every component's
// port set is compared with the supervisor's own resolved table rather than hand-written here.
func TestComponentPortsComeFromTheResolvedTable(t *testing.T) {
	s := &supervisor{ports: servicePorts{
		postgres: 5432, nats: 4222, natsMonitor: 8222,
		tempo: 3200, tempoGRPC: 9095, tempoOTLPGRPC: 14317, tempoOTLPHTTP: 14318,
		loki: 3100, vm: 8428,
		collectorOTLPGRPC: 4317, collectorOTLPHTTP: 4318,
		collectorHealth: 13133, collectorSelfMetrics: 8888,
	}}
	cases := map[string][]int{
		"postgres":        {5432},
		"nats":            {4222, 8222},
		"tempo":           {3200, 9095, 14317, 14318},
		"loki":            {3100}, // grpc is 0, so there is no fixed port to own
		"victoriametrics": {8428},
		"otel-collector":  {4317, 4318, 13133, 8888},
	}
	for comp, want := range cases {
		got := s.componentPorts(comp)
		if len(got) != len(want) {
			t.Errorf("%s: ports = %v, want %v", comp, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: ports = %v, want %v", comp, got, want)
				break
			}
		}
	}
	if got := s.componentPorts("not-a-component"); got != nil {
		t.Errorf("an unknown component has no ports, got %v", got)
	}
}

// A LOOKUP THAT FINDS NOTHING MUST SAY NOTHING.
//
// The caller's whole reason for existing is "a bind conflict and a real bug look identical from
// inside the retry loop". When the port is FREE, they are not identical — it is a real bug, and the
// child's own message is the whole story. Emitting "something holds this" (or worse, a fabricated
// owner) on an empty result would be a false diagnosis, which is the one thing worse than silence.
func TestReportPortOwnerStaysQuietWhenThePortIsFree(t *testing.T) {
	t.Setenv(portOwnerEnv, "") // do not disable, but nothing is listening on a made-up port
	s := &supervisor{log: testLogger(), ports: servicePorts{loki: closedPort(t)}}
	s.reportPortOwner("loki")
	if s.portOwnerReported["loki"] {
		t.Error("reportPortOwner marked the component as reported when it had nothing to report")
	}
}

// The advice must name the port, the holder, AND state that Orchicon will not kill it — the last
// part because the holder is very often a SECOND INSTANCE's services, and an operator who assumes
// the tool will clean up after itself waits for a retry that can never succeed.
func TestPortOwnerAdviceNamesTheHolderAndRefusesToAct(t *testing.T) {
	var buf strings.Builder
	s := &supervisor{log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))}
	s.logPortOwnerAdvice("nats", []portOwner{{Port: 8222, PID: 26792, Name: "nats-server.exe"}})

	out := buf.String()
	for _, want := range []string{"nats", "8222", "26792", "nats-server.exe", "will not kill"} {
		if !strings.Contains(out, want) {
			t.Errorf("the advice does not mention %q:\n%s", want, out)
		}
	}
}

// THE LOOKUP MUST BE DISABLEABLE. A test whose fake child "cannot bind" a made-up port would
// otherwise ask the OS about it on every restart, which is slow and meaningless.
func TestPortOwnersRespectsTheDisableKnob(t *testing.T) {
	t.Setenv(portOwnerEnv, "1")
	if got := portOwners([]int{closedPort(t)}); got != nil {
		t.Errorf("the disable knob must suppress the lookup entirely, got %v", got)
	}
}
