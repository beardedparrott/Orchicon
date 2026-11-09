// Package planebind is the Wails-bound face of PLANE CONTROL, and it adds no mechanism of its own.
package planebind

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/instance"
	"github.com/beardedparrott/orchicon/internal/planecontrol"
)

// healthProbeTimeout bounds one /healthz probe. Short: a status call is a UI
// round-trip, not a wait for a boot.
const healthProbeTimeout = 2 * time.Second

// Service is the object Wails binds. Its exported methods become
// window.go.planebind.Service.* in the SPA (resolved by lib/desktop.ts).
type Service struct {
	// Out receives planecontrol's progress text. nil → discarded (Wails needs no
	// console), but tests capture it to prove what the actions reported.
	Out io.Writer
}

// StatusDTO is one instance's connection state, flattened for JSON/Wails.
type StatusDTO struct {
	Instance    string
	ControlPort int
	URL         string
	State       string // connecting | connected | offline | plane-error
	Detail      string // what was FOUND: status code, drift verdict, or "nothing answers"
	Residency   string
	PID         int
	Binary      string
	Drift       string
	Healthy     bool
	LogPath     string
}

// ResolutionDTO answers "which instance does this port mean".
type ResolutionDTO struct {
	Instance    string
	ControlPort int
	Known       bool
	Running     []StatusDTO
}

// ActionDTO is the outcome of a start/stop/restart, as the screen reports it.
type ActionDTO struct {
	Instance string
	Action   string
	Outcome  string // started | already-running | left-alone-unidentified | stopped | already-stopped | failed
	OK       bool
	LogPath  string
	Message  string
}

// THE SEAMS. Production never reassigns these; tests do (and restore them) so the
// binding's DECISIONS are assertable without spawning processes.
var (
	startFn   = planecontrol.Start
	stopFn    = planecontrol.Stop
	restartFn = planecontrol.Restart
	statusFn  = planecontrol.StatusOf
	probeFn   = planecontrol.Probe
)

func (s *Service) out() io.Writer {
	if s.Out == nil {
		return io.Discard
	}
	return s.Out
}

// logPath is where a failure's evidence is, matching the path planecontrol.Start's
// own error names — so the screen and the CLI point at the same file.
func logPath(instanceName string) string {
	return filepath.Join(planecontrol.HostDataDirFor(instanceName), "serve", "logs", "orchicon.log")
}

// statusOf builds one instance's DTO.
func statusOf(instanceName string) StatusDTO {
	dto := StatusDTO{Instance: instanceName, LogPath: logPath(instanceName)}
	st, err := statusFn(planecontrol.Defaults(instanceName, io.Discard))
	if err != nil {
		// The port record is unreadable: report it as offline with the reason, rather
		// than failing the whole screen. The action that can fix it is Start.
		dto.State = StateOffline
		dto.Detail = fmt.Sprintf("no readable port record for %s: %v", instanceName, err)
		return dto
	}
	var h planecontrol.Health
	if st.HealthURL != "" {
		h = probeFn(st.HealthURL, healthProbeTimeout)
	}
	dto.ControlPort = st.ControlPort
	dto.URL = st.HealthURL
	dto.Residency = st.Residency
	dto.PID = st.PID
	dto.Binary = st.Binary
	dto.Drift = st.Drift.String()
	dto.Healthy = h.Healthy()
	dto.State, dto.Detail = Classify(st, h)
	return dto
}

// Instances reports every known instance, with its state.
func (s *Service) Instances() ([]StatusDTO, error) {
	out := make([]StatusDTO, 0, len(instance.Order))
	for _, name := range instance.Order {
		out = append(out, statusOf(name))
	}
	return out, nil
}

// Status reports ONE instance. The name is validated through the shared CLI rule,
// so a name the CLI would reject cannot be used here either.
func (s *Service) Status(instanceName string) (StatusDTO, error) {
	if err := instance.ValidateName(instanceName); err != nil {
		return StatusDTO{}, err
	}
	if !instance.Known(instanceName) {
		return StatusDTO{}, fmt.Errorf("unknown instance %q: this build knows %s — refusing to guess",
			instanceName, strings.Join(instance.Order, ", "))
	}
	return statusOf(instanceName), nil
}

// ResolveControlPort maps the configured plane URL's control port back to an instance.
func (s *Service) ResolveControlPort(port int) (ResolutionDTO, error) {
	res := ResolutionDTO{ControlPort: port}
	for _, name := range instance.Order {
		dto := statusOf(name)
		if dto.PID > 0 || dto.State == StateConnected || dto.State == StatePlaneError {
			res.Running = append(res.Running, dto)
		}
	}
	if name, ok := instance.FromControlPort(port); ok {
		res.Instance, res.Known = name, true
	}
	return res, nil
}

// Start brings the instance's plane up and waits for /healthz.
func (s *Service) Start(instanceName string) (ActionDTO, error) {
	return s.act(instanceName, "start", startFn)
}

// Stop stops the instance's plane. Idempotent: stopping what is not running is not
// a failure (planecontrol.Stop's own contract).
func (s *Service) Stop(instanceName string) (ActionDTO, error) {
	return s.act(instanceName, "stop", stopFn)
}

// Restart stops and starts.
func (s *Service) Restart(instanceName string) (ActionDTO, error) {
	return s.act(instanceName, "restart", restartFn)
}

// act is the one path every mutating method takes: validate, delegate, capture
// planecontrol's own wording, report.
func (s *Service) act(instanceName, action string, fn func(planecontrol.Options) error) (ActionDTO, error) {
	if err := instance.ValidateName(instanceName); err != nil {
		return ActionDTO{}, err
	}
	if !instance.Known(instanceName) {
		return ActionDTO{}, fmt.Errorf("unknown instance %q: this build knows %s — refusing to guess",
			instanceName, strings.Join(instance.Order, ", "))
	}
	buf := &bytes.Buffer{}
	opts := planecontrol.Defaults(instanceName, io.MultiWriter(buf, s.out()))
	err := fn(opts)
	return actionDTO(instanceName, action, buf.String(), err), nil
}

// actionDTO words an outcome from planecontrol's own output.
func actionDTO(instanceName, action, output string, err error) ActionDTO {
	dto := ActionDTO{Instance: instanceName, Action: action, LogPath: logPath(instanceName), Message: strings.TrimSpace(output)}
	if err != nil {
		dto.OK = false
		dto.Outcome = "failed"
		dto.Message = strings.TrimSpace(err.Error() + "\n" + output)
		// The failure path names the evidence; Start's own error already does.
		if !strings.Contains(dto.Message, dto.LogPath) {
			dto.Message += fmt.Sprintf(" (see %s)", dto.LogPath)
		}
		return dto
	}
	dto.OK = true
	// THE WORDING IS SCOPED TO THE ACTION, because planecontrol's phrases are not unique across verbs.
	switch action {
	case "start":
		switch {
		case strings.Contains(output, "leaving it"):
			// planecontrol's fail-safe: an unidentifiable plane is LEFT ALONE.
			dto.Outcome = "left-alone-unidentified"
		case strings.Contains(output, "already serving"):
			dto.Outcome = "already-running"
		default:
			dto.Outcome = "started"
		}
	case "stop":
		if strings.Contains(output, "not running") {
			dto.Outcome = "already-stopped"
		} else {
			dto.Outcome = "stopped"
		}
	default: // restart: Stop + Start, and Start only returns nil once /healthz answered
		dto.Outcome = "started"
	}
	return dto
}
