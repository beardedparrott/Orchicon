// Command orch is the thin Orchicon remote client: a Bubbletea TUI that
// connects to a running Orchicon instance over the network — a client of
// the plane, exactly like the web GUI. It carries zero control-plane
// code; the binary is small by design (the full `orchicon` binary
// embeds the frontend, migrations, and every reconciler and is the wrong
// download for a remote user).
//
// Usage:
//
//	orch                 launch the TUI (connection screen on first run)
//	orch version         print the client version (same as orchicon version)
//	orch --url URL --token KEY / --insecure   flag overrides (env wins)
//
// Credentials persist in ~/.orchicon/config (0600). ORCHICON_URL and
// ORCHICON_TOKEN override the config file and are never persisted.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"

	"github.com/beardedparrott/orchicon/internal/tui"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
	"github.com/beardedparrott/orchicon/internal/tui/connection"
	"github.com/beardedparrott/orchicon/internal/version"
)

// flags carries CLI overrides (env still wins — see resolveProfile).
type flags struct {
	url      string
	token    string
	insecure bool
	showVer  bool
}

func main() {
	fl := parseArgs(os.Args[1:])
	if fl.showVer {
		fmt.Println(version.Current())
		return
	}
	if err := run(fl); err != nil {
		fmt.Fprintln(os.Stderr, "orch:", err)
		os.Exit(1)
	}
}

// parseArgs keeps the CLI intentionally tiny (plan step 10: no other
// subcommands — thin by design).
func parseArgs(args []string) *flags {
	fl := &flags{}
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "version":
			fl.showVer = true
		case "--url", "-u":
			if i+1 < len(args) {
				i++
				fl.url = args[i]
			}
		case "--token", "-t":
			if i+1 < len(args) {
				i++
				fl.token = args[i]
			}
		case "--insecure", "-k":
			fl.insecure = true
		case "--help", "-h":
			fmt.Print(usage)
			os.Exit(0)
		default:
			// Unknown args are ignored: flags may arrive in any order and
			// the TUI is the only surface.
		}
	}
	return fl
}

const usage = `orch — Orchicon remote client (TUI)

Usage:
  orch                 launch the TUI
  orch version         print the client version
  orch --url URL --token oc_... [--insecure]

Credentials persist in ~/.orchicon/config (0600).
ORCHICON_URL / ORCHICON_TOKEN override the config file.
Create an API key in the web GUI (Settings → API keys) with read
scopes for the areas orch displays.
`

// run resolves the profile then either launches the connection screen
// (first run / missing credential) or the app shell directly.
func run(fl *flags) error {
	path, err := config.DefaultPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		// A corrupt config file must not wedge the client: start fresh
		// (the connection screen re-creates it on save).
		cfg = &config.Config{Profiles: map[string]*config.Profile{}}
	}
	profile := config.Resolve(cfg)
	if profile == nil && fl.url != "" {
		profile = &config.Profile{URL: strings.TrimRight(fl.url, "/"), AuthMethod: config.AuthAPIKey, Token: fl.token, InsecureSkipVerify: fl.insecure}
	}
	if profile == nil && (fl.token != "" || fl.insecure) {
		return fmt.Errorf("--token/--insecure given but no --url (and no config): pass --url or set ORCHICON_URL")
	}
	if profile != nil {
		applyFlags(profile, fl)
	}

	if profile == nil || profile.Token == "" {
		return runConnection(path, profile)
	}
	return runShell(profile)
}

// applyFlags layers CLI flags under env (config.Resolve already applied
// env): env wins, then flags, then file.
func applyFlags(p *config.Profile, fl *flags) {
	if os.Getenv(config.EnvURL) == "" && fl.url != "" {
		p.URL = strings.TrimRight(fl.url, "/")
	}
	if os.Getenv(config.EnvToken) == "" && fl.token != "" {
		p.Token = fl.token
		if p.AuthMethod == "" {
			p.AuthMethod = config.AuthAPIKey
		}
	}
	if fl.insecure {
		p.InsecureSkipVerify = true
	}
}

// runConnection shows the first-run screen; on success it persists the
// profile (unless env-driven) and hands off to the shell.
func runConnection(path string, existing *config.Profile) error {
	m := connection.New(existing, connection.DefaultProbes())
	prog := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	final, err := prog.Run()
	if err != nil {
		return fmt.Errorf("connection screen: %w", err)
	}
	cm, ok := final.(connection.Model)
	if !ok {
		return fmt.Errorf("connection screen returned unexpected state")
	}
	res := cm.Result()
	if res == nil {
		return fmt.Errorf("no connection established")
	}
	if os.Getenv(config.EnvURL) == "" || os.Getenv(config.EnvToken) == "" {
		// Env-driven profiles are never persisted (plan §3); file-driven
		// ones save so the second launch connects automatically.
		if os.Getenv(config.EnvURL) == "" {
			if err := connection.SaveProfile(path, res); err != nil {
				return fmt.Errorf("save config: %w", err)
			}
		}
	}
	return runShell(res.Profile)
}

// runShell probes /versionz, builds the client set, and runs the app
// shell until the user quits.
func runShell(profile *config.Profile) error {
	vr, err := client.Ping(context.Background(), profile.URL, profile.InsecureSkipVerify)
	if err != nil {
		// Non-blocking per the plan: stale config still opens the shell;
		// the footer shows the degraded state (empty server version).
		vr = &client.VersionzResponse{}
	}
	serverVersion := ""
	if vr != nil {
		serverVersion = vr.Version
	}
	cl := client.New(client.Options{
		BaseURL:            profile.URL,
		Token:              profile.Token,
		InsecureSkipVerify: profile.InsecureSkipVerify,
		Timeout:            30 * time.Second,
	})
	app := tui.NewApp(cl, profile, serverVersion)
	if identity := probeIdentity(cl); identity != "" {
		app.SetIdentity(identity)
	}
	// Open on Ask (the GUI nav's first entry) instead of an empty shell;
	// its streams start on the first WindowSizeMsg.
	app.SwitchTo(tui.TabAsk)
	prog := tea.NewProgram(app, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err = prog.Run()
	return err
}

// probeIdentity resolves the footer identity display name (best effort:
// admin-gated RPC — an empty string just hides the identity chip).
func probeIdentity(cl *client.Clients) string {
	resp, err := cl.Auth.ListIdentities(context.Background(), connect.NewRequest(&apiv1.ListIdentitiesRequest{PageSize: 1}))
	if err != nil || resp == nil || len(resp.Msg.GetIdentities()) == 0 {
		return ""
	}
	return resp.Msg.GetIdentities()[0].GetDisplayName()
}
