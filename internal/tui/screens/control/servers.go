package control

// servers.go — Control → Servers: the same server management the desktop app offers (design of
// record, phase 2 / D6). IT IS LOCAL-ONLY, AND SAYS SO.

import (
	"context"
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/instancectl"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// serversSourceName is the source's id: stable, because slash commands and tests address it.
const serversSourceName = "servers"

// fetchServers lists every server on this machine.
func (m *Model) fetchServers(_ context.Context, _ string) ([]kit2.Item, string, error) {
	if !m.serversEnabled {
		return nil, "", nil
	}
	list, err := instancectl.List()
	if err != nil {
		return nil, "", fmt.Errorf("read this machine's servers: %w", err)
	}
	items := make([]kit2.Item, 0, len(list))
	for _, s := range list {
		items = append(items, kit2.Item{
			ID:    s.Name,
			Title: s.Name,
			// The Meta column is the ONE thing a glance needs: is it working, and if not, why.
			Meta: serverMeta(s),
		})
	}
	return items, "", nil
}

// serverMeta is one short phrase per server, for the list's right-hand column.
func serverMeta(s instancectl.Instance) string {
	switch s.Setup.EffectiveState() {
	case instancectl.SetupRunning:
		if s.Setup.Percent > 0 {
			return fmt.Sprintf("setting up %d%%", s.Setup.Percent)
		}
		return "setting up"
	case instancectl.SetupFailed:
		return "setup FAILED"
	case instancectl.SetupStale:
		// The state that exists so a killed install cannot render as eternal progress.
		return "setup stopped responding"
	}
	// HEALTH IS THE AUTHORITATIVE SIGNAL, AND A PID IS NOT REQUIRED TO REPORT IT.
	if s.Plane.Healthy {
		// The port is on the Instance AND on the plane DTO, and Status() sets them equal. Read either, so a
		// caller that filled only one (a test, or a future DTO) still gets the port rather than ":0".
		port := s.ControlPort
		if port == 0 {
			port = s.Plane.ControlPort
		}
		if port == 0 {
			// Serving but the port never reached us: say the fact we HAVE rather than invent a ":0".
			return "running"
		}
		return fmt.Sprintf("running :%d", port)
	}
	if s.Plane.PID > 0 {
		// A process that exists but serves nothing is a different failure, and naming it that way is what
		// distinguishes a wedged plane from an absent one.
		return "not answering (no /healthz)"
	}
	return "stopped"
}

// serversDetail is the detail pane for one server: what it is, where it lives, and how to read its log.
func (m *Model) serversDetail(name string) []screenkit.Field {
	s, err := instancectl.Status(name)
	if err != nil {
		return []screenkit.Field{{Key: "error", Value: err.Error()}}
	}
	out := []screenkit.Field{
		{Key: "server", Value: s.Name},
		{Key: "state", Value: serverMeta(s)},
		{Key: "plane", Value: s.Plane.Detail},
		{Key: "web UI", Value: s.URL},
		{Key: "data", Value: s.DataDir},
		{Key: "log", Value: s.Plane.LogPath},
	}
	if s.Setup != nil {
		out = append(out, screenkit.Field{Key: "setup", Value: setupLine(s.Setup)})
	}
	return out
}

// setupLine is the one-line setup summary, matching the desktop page's wording so the two agree.
func setupLine(p *instancectl.SetupProgress) string {
	switch p.EffectiveState() {
	case instancectl.SetupRunning:
		if p.Label != "" {
			return p.Label
		}
		return "setting up"
	case instancectl.SetupDone:
		return "setup complete"
	case instancectl.SetupFailed:
		if p.Error != "" {
			return "setup failed: " + p.Error
		}
		return "setup failed"
	case instancectl.SetupStale:
		return "setup stopped responding — re-apply to retry"
	}
	return p.State
}

// serversActions are the verbs for the selected server.
func (m *Model) serversActions(item kit2.Item) []kit2.Action {
	name := item.ID
	status, err := instancectl.Status(name)
	running := err == nil && status.Plane.PID > 0 && status.Plane.Healthy
	setupActive := err == nil && status.Setup != nil &&
		(status.Setup.EffectiveState() == instancectl.SetupRunning ||
			status.Setup.EffectiveState() == instancectl.SetupStale)

	// While a setup is in flight the verbs would fight it: start/stop/restart against a database being
	// initialised is how a half-created cluster happens.
	if setupActive {
		return []kit2.Action{{
			Label: "re-apply", Key: "i", Source: serversSourceName,
			Do: func(ctx context.Context) error { return m.serversInstall(ctx, name) },
		}}
	}

	acts := []kit2.Action{
		{
			Label: "start", Key: "s", Source: serversSourceName,
			Do: func(ctx context.Context) error { return m.serversVerb(ctx, name, "start") },
		},
		{
			Label: "restart", Key: "R", Source: serversSourceName,
			Do: func(ctx context.Context) error { return m.serversVerb(ctx, name, "restart") },
		},
		{
			Label: "stop", Key: "S", Source: serversSourceName, Danger: running,
			Do: func(ctx context.Context) error { return m.serversVerb(ctx, name, "stop") },
		},
		{
			Label: "re-apply", Key: "i", Source: serversSourceName,
			Do: func(ctx context.Context) error { return m.serversInstall(ctx, name) },
		},
		{
			Label: "remove (keep data)", Key: "x", Danger: true, Source: serversSourceName,
			Confirm: "Remove the server " + name + " and KEEP its data?\n" +
				"Its database, attachments and secrets key stay, so reinstalling this server reuses them.",
			Do: func(ctx context.Context) error { return m.serversRemove(ctx, name, false) },
		},
		{
			Label: "remove (DESTROY data)", Key: "X", Danger: true, Source: serversSourceName,
			Confirm: "Remove the server " + name + " and DESTROY its data?\n" +
				"This deletes the database, your attachments AND the key that decrypts your secrets.\n" +
				"Those secrets can never be decrypted again. There is no undo.",
			Do: func(ctx context.Context) error { return m.serversRemove(ctx, name, true) },
		},
	}
	return acts
}

// serversVerb runs one of the short verbs through the shared contract and refreshes the list.
func (m *Model) serversVerb(_ context.Context, name, verb string) error {
	var (
		res instancectl.Result
		err error
	)
	switch verb {
	case "start":
		res, err = instancectl.Start(name, instancectl.Options{})
	case "stop":
		res, err = instancectl.Stop(name, instancectl.Options{})
	case "restart":
		res, err = instancectl.Restart(name, instancectl.Options{})
	default:
		return fmt.Errorf("unknown server verb %q", verb)
	}
	m.refreshServers()
	if err != nil {
		return fmt.Errorf("%s %s: %w", verb, name, err)
	}
	// An outcome is reported even on success, because "already running" and "started" are different facts
	// and the operator asked for one of them.
	return nilReporter(res)
}

// serversInstall re-applies an install. It is the RETRY for a failed or stale setup, which is why it is
// offered while one is in flight.
func (m *Model) serversInstall(_ context.Context, name string) error {
	res, err := instancectl.Install(name, instancectl.Options{})
	m.refreshServers()
	if err != nil {
		return fmt.Errorf("install %s: %w", name, err)
	}
	return nilReporter(res)
}

// serversRemove uninstalls one server. purge is the DESTROY answer and is already confirmed by the caller's
// prompt; the contract additionally requires it to be stated, so it cannot be reached by accident.
func (m *Model) serversRemove(_ context.Context, name string, purge bool) error {
	res, err := instancectl.Uninstall(name, instancectl.Options{Purge: purge})
	m.refreshServers()
	if err != nil {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return nilReporter(res)
}

// nilReporter turns the contract's outcome into an error ONLY when it failed, so the TUI reports a failure
// it can see and stays quiet about a success the list already shows.
func nilReporter(res instancectl.Result) error {
	if res.OK {
		return nil
	}
	msg := strings.TrimSpace(res.Message)
	if msg == "" {
		msg = res.Outcome
	}
	return fmt.Errorf("%s", msg)
}

// refreshServers re-reads the source, so a server that just changed is not shown stale.
func (m *Model) refreshServers() {
	if m.serversEnabled {
		m.Refresh(serversSourceName)
	}
}
