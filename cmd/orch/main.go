// Command orch is the thin Orchicon remote client: a Bubbletea TUI that
// connects to a running Orchicon instance over the network — a client of
// the plane, exactly like the web GUI. It carries zero control-plane
// code; the binary is small by design (the full `orchicon` binary
// embeds the frontend, migrations, and every reconciler and is the wrong
// download for a remote user).
//
// Usage:
//
//	orch                    launch the TUI (connection screen on first run)
//	orch --profile NAME     pin THIS PROCESS to a saved profile
//	orch version            print the client version (same as orchicon version)
//	orch --url URL --token KEY / --insecure   flag overrides (env wins)
//
// Credentials persist in ~/.orchicon/config (0600). ORCHICON_URL and
// ORCHICON_TOKEN override the config file and are never persisted.
//
// ORCHICON_PROFILE selects a NAMED profile for THIS PROCESS without changing which
// profile a bare `orch` uses, so several orch processes can talk to several instances
// at once. Each instance is saved under its own name (its host:port), so connecting to a
// second instance no longer replaces the first one's entry.
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

	"github.com/beardedparrott/orchicon/internal/controlport"
	"github.com/beardedparrott/orchicon/internal/tui"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
	"github.com/beardedparrott/orchicon/internal/tui/connection"
	"github.com/beardedparrott/orchicon/internal/version"
)

// flags carries CLI overrides (env still wins — see resolveProfile).
type flags struct {
	url string
	// profile selects a NAMED profile for this process (one instance per process).
	profile  string
	token    string
	insecure bool
	showVer  bool
	// instance names a LOCAL Orchicon instance (dev|prod|test) to connect to, resolved through the
	// SHARED control-port table — the same one the desktop app and the installer use.
	instance string
}

func main() {
	fl := parseArgs(os.Args[1:])
	// Fullscreen takeover like opencode / Claude Code: the app owns the viewport and paints every cell
	// itself.
	applyWSLTrueColorDefault()
	applyTerminalColorProfile()
	restoreWrap := disableAutoWrap()
	defer restoreWrap()
	if fl.showVer {
		fmt.Println(version.Current())
		return
	}
	if err := run(fl); err != nil {
		fmt.Fprintln(os.Stderr, "orch:", err)
		os.Exit(1)
	}
}

// disableAutoWrap turns the terminal's own line wrap OFF and returns the restore func.
func disableAutoWrap() func() {
	fi, err := os.Stdout.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return func() {}
	}
	fmt.Fprint(os.Stdout, "\x1b[?7l")
	return func() { fmt.Fprint(os.Stdout, "\x1b[?7h") }
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
		case "--profile", "-p":
			if i+1 < len(args) {
				i++
				fl.profile = args[i]
			}
		case "--instance", "-i":
			if i+1 < len(args) {
				i++
				fl.instance = args[i]
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
  orch                   launch the TUI
  orch --profile NAME    pin this process to a saved profile (one per instance)
  orch --instance dev    connect to a local instance (dev|prod|test)
  orch version           print the client version
  orch --url URL --token oc_... [--insecure]

Credentials persist in ~/.orchicon/config (0600).
ORCHICON_URL / ORCHICON_TOKEN override the config file.
--instance resolves through the shared control-port table, so dev/prod/test mean the
same thing here as in the desktop app and the installer. It also sets
ORCHICON_INSTANCE for this process, so /plane start|stop|restart act on the instance
you are connected to.
ORCHICON_PROFILE selects a named profile for THIS PROCESS without changing which
profile a plain "orch" uses, so several orch processes can talk to several instances
at once. Each instance is saved under its own name (its host:port), so connecting to a
second instance no longer replaces the first one's entry.
Create an API key in the web GUI (Settings → API keys) with read
scopes for the areas orch displays.
`

// run resolves the profile then either launches the connection screen (first run / missing
// credential) or the app shell directly.
func selectInstance(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	if _, ok := controlport.Defaults()[name]; !ok {
		return "", fmt.Errorf("unknown instance %q — expected dev, prod or test (or use --url for an arbitrary plane)", name)
	}
	if err := os.Setenv("ORCHICON_INSTANCE", name); err != nil {
		return "", fmt.Errorf("select instance %q: %w", name, err)
	}
	return config.InstanceURL(name), nil
}

func run(fl *flags) error {
	path, err := config.DefaultPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	// AN UNREADABLE CONFIG IS PRESERVED, NEVER SILENTLY OVERWRITTEN.
	loadWarning := ""
	if err != nil {
		if backup, berr := config.PreserveUnreadable(path); berr == nil {
			loadWarning = fmt.Sprintf("%s could not be read (%v). It has been preserved at %s, and this session starts from an empty config — sign in again, and check that file before the preserved copy is cleaned up.", path, err, backup)
		} else {
			loadWarning = fmt.Sprintf("%s could not be read (%v), and it could not be preserved: %v", path, err, berr)
		}
		cfg = &config.Config{Profiles: map[string]*config.Profile{}}
	}
	// WHICH INSTANCE THIS PROCESS TALKS TO.
	requested := strings.TrimSpace(os.Getenv(config.EnvProfile))
	if requested == "" {
		requested = strings.TrimSpace(fl.profile)
	}
	profile := config.ResolveNamed(cfg, requested)
	if requested != "" && profile == nil {
		// SAY SO, AND DO NOT SUBSTITUTE ANOTHER INSTANCE.
		msg := fmt.Sprintf("no profile named %q in %s — connect to that instance once (it is saved under its host:port), or correct ORCHICON_PROFILE/--profile", requested, path)
		if loadWarning != "" {
			loadWarning += "; also, " + msg
		} else {
			loadWarning = msg
		}
	}
	// WHICH LOCAL INSTANCE.
	target, err := selectInstance(fl.instance)
	if err != nil {
		return err
	}

	if profile == nil && fl.url != "" {
		profile = &config.Profile{URL: strings.TrimRight(fl.url, "/"), AuthMethod: config.AuthAPIKey, Token: fl.token, InsecureSkipVerify: fl.insecure}
	}
	// THE INSTANCE RETARGETS THE CONNECTION, AND REUSES THE CREDENTIAL ALREADY SAVED FOR IT.
	if target != "" && fl.url == "" {
		if profile == nil {
			profile = config.ProfileForInstance(cfg, fl.instance)
		}
		if profile == nil {
			profile = &config.Profile{AuthMethod: config.AuthAPIKey, Token: fl.token, InsecureSkipVerify: fl.insecure}
		} else {
			cp := *profile // never mutate the saved profile
			profile = &cp
		}
		profile.URL = target
	}
	if profile == nil && (fl.token != "" || fl.insecure) {
		return fmt.Errorf("--token/--insecure given but no --url (and no config): pass --url or set ORCHICON_URL")
	}
	if profile != nil {
		applyFlags(profile, fl)
	}

	if profile == nil || profile.Token == "" {
		p, err := runConnection(path, profile, loadWarning)
		if err != nil {
			return err
		}
		profile = p
	}
	// Apply the stored palette preference to the FINAL profile, AFTER the connection branch.
	applyStoredTheme(profile, cfg)
	// The launch DIRECTORY is computed ONCE here and passed to EVERY shell in this session.
	launchDir, _ := os.Getwd()
	firstShell := true
	for {
		opts := shellLaunchOptions(firstShell, launchDir)
		firstShell = false
		reconnect, err := runShell(profile, opts...)
		if !reconnect {
			return err
		}
		// The shell exited for re-auth (/connect) — or the launch-time credential check rejected a stored
		// session.
		p, cerr := runConnection(path, profile, reauthReason)
		if cerr != nil {
			return cerr
		}
		profile = p
	}
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

// runConnection shows the first-run screen; on success it persists the profile (unless env-driven)
// and returns it.
const reauthReason = "the saved session could not be authenticated — sign in again"

func runConnection(path string, existing *config.Profile, reason string) (*config.Profile, error) {
	m := connection.New(existing, connection.DefaultProbes())
	m.SetReason(reason)
	prog := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	final, err := prog.Run()
	if err != nil {
		return nil, fmt.Errorf("connection screen: %w", err)
	}
	cm, ok := final.(connection.Model)
	if !ok {
		return nil, fmt.Errorf("connection screen returned unexpected state")
	}
	res := cm.Result()
	if res == nil {
		return nil, fmt.Errorf("no connection established")
	}
	if os.Getenv(config.EnvURL) == "" || os.Getenv(config.EnvToken) == "" {
		// Env-driven profiles are never persisted (plan §3); file-driven
		// ones save so the second launch connects automatically.
		if os.Getenv(config.EnvURL) == "" {
			if err := connection.SaveProfile(path, res); err != nil {
				return nil, fmt.Errorf("save config: %w", err)
			}
		}
	}
	return res.Profile, nil
}

// applyStoredTheme resolves the TUI palette preference onto the profile that will actually be used:
// the config's top-level theme, overridden by ORCHICON_THEME.
func applyStoredTheme(p *config.Profile, cfg *config.Config) {
	if p == nil {
		return
	}
	if cfg != nil && cfg.Theme != "" {
		p.Theme = cfg.Theme
	}
	if envTheme := strings.TrimSpace(os.Getenv(config.EnvTheme)); envTheme != "" {
		p.Theme = envTheme
	}
}

// shellLaunchOptions is ONE shell's TUI options, and it is where the session's two separate
// decisions are expressed:
func shellLaunchOptions(firstShell bool, launchDir string) []tui.AppOption {
	opts := []tui.AppOption{tui.WithLaunchDir(launchDir)}
	if firstShell {
		opts = append(opts, tui.WithLaunchPrompt())
	}
	return opts
}

// runShell probes /versionz, builds the client set, and runs the app shell until the user quits.
func runShell(profile *config.Profile, opts ...tui.AppOption) (bool, error) {
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
		// Password mode: auto-refresh the 900s access token with the stored 24h refresh token (Phase 2b).
		RefreshToken: profile.RefreshToken,
	})
	// Launch-time credential check.
	identity, probeErr := probeIdentity(cl)
	if probeErr != nil && connect.CodeOf(probeErr) == connect.CodeUnauthenticated {
		return true, nil // main reopens the connection screen, with a reason
	}
	app := tui.NewApp(cl, profile, serverVersion, opts...)
	if identity != "" {
		app.SetIdentity(identity)
	}
	// Open on Ask (the GUI nav's first entry) instead of an empty shell;
	// its streams start on the first WindowSizeMsg.
	app.SwitchTo(tui.TabAsk)
	prog := tea.NewProgram(app, tea.WithAltScreen(), tea.WithMouseCellMotion(),
		tea.WithReportFocus(),
		// THE PROGRAM INPUT IS PLATFORM-SPECIFIC, and on Windows it is not merely decorated stdin:
		// bubbletea picks its whole input PATH by comparing the reader's fd to stdin's.
		tea.WithInput(programInput()))
	_, err = prog.Run()
	if err != nil {
		return false, err
	}
	if app.ReconnectRequested() {
		profile = app.Profile()
		return true, nil
	}
	return false, nil
}

// probeIdentity resolves the footer identity display name (best effort: admin-gated RPC — an empty
// string just hides the identity chip).
func probeIdentity(cl *client.Clients) (string, error) {
	resp, err := cl.Auth.ListIdentities(context.Background(), connect.NewRequest(&apiv1.ListIdentitiesRequest{PageSize: 1}))
	if err != nil {
		return "", err
	}
	if resp == nil || len(resp.Msg.GetIdentities()) == 0 {
		return "", nil
	}
	return resp.Msg.GetIdentities()[0].GetDisplayName(), nil
}
