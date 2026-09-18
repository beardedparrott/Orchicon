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
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

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
	// Fullscreen takeover like opencode / Claude Code: the app owns the
	// viewport and paints every cell itself. Two terminal-level settings
	// complete that ownership:
	//
	//   1. 24-bit color when the terminal advertises it (COLORTERM), so the
	//      theme's hex values render exactly instead of being rounded to
	//      the 256-color cube.
	//   2. Auto-wrap OFF for the app's lifetime (restored on exit): writing
	//      the last cell of the last row otherwise leaves the terminal in
	//      wrap-pending state, and the renderer's next line feed scrolls
	//      the whole frame up by one row — leaving an unpainted hairline
	//      along the bottom (the operator's persistent "sliver").
	if os.Getenv("COLORTERM") == "truecolor" || os.Getenv("COLORTERM") == "24bit" {
		lipgloss.SetColorProfile(termenv.TrueColor)
	}
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

// disableAutoWrap turns the terminal's own line wrap OFF and returns the
// restore func. A fullscreen TUI paints every cell up to the terminal's
// last column; with auto-wrap enabled the cursor is left wrap-pending on
// the final row, and the next line feed scrolls the frame by one row —
// the unpainted hairline that appears along the bottom of the display.
// Non-TTY stdout (pipes, CI) is left untouched.
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
		p, err := runConnection(path, profile, "")
		if err != nil {
			return err
		}
		profile = p
	}
	// Apply the stored palette preference to the FINAL profile, AFTER the
	// connection branch.
	//
	// It used to be applied BEFORE that branch, to the pre-connection profile —
	// which the branch then REPLACED with the profile the connection screen built.
	// Any launch that passed through that screen (first run, or an env-driven
	// launch with no token in the env: notably the orch-dev/orch-prod launchers,
	// which preset ORCHICON_URL) therefore lost its saved theme for the entire
	// session even though the config still held it. Applying it here cannot be
	// undone by anything downstream.
	//
	// The preference lives at the config's TOP LEVEL so it survives without a
	// saved profile (env-driven sessions and first runs never write one).
	// ORCHICON_THEME wins over the file, so a palette can be pinned where the
	// config is not persisted.
	applyStoredTheme(profile, cfg)
	// The launch prompt asks about the directory the operator is sitting in, computed
	// ONCE here: the app takes it as an option, and a later shell in this session is
	// passed "" so a reconnect never re-asks. os.Getwd can fail (a deleted cwd); that
	// is not worth failing a launch over — no directory simply means no question.
	launchDir, _ := os.Getwd()
	firstShell := true
	for {
		dir := ""
		if firstShell {
			dir = launchDir
		}
		firstShell = false
		reconnect, err := runShell(profile, dir)
		if !reconnect {
			return err
		}
		// The shell exited for re-auth (/connect) — or the launch-time credential
		// check rejected a stored session. Reopen the connection screen,
		// pre-filled with the current profile and SAYING WHY, then loop.
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

// runConnection shows the first-run screen; on success it persists the
// profile (unless env-driven) and returns it.
// reauthReason is the WHY shown on the connection screen when the shell
// bounces the operator back here: a stored session that cannot authenticate is
// not a first run, and being asked for credentials with no explanation reads
// as the tool having lost them for no reason.
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

// applyStoredTheme resolves the TUI palette preference onto the profile that will
// actually be used: the config's top-level theme, overridden by ORCHICON_THEME.
// It is applied to the FINAL profile (after any connection-screen rebuild), because
// the connection screen returns a freshly built profile and anything set before it
// is discarded.
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

// runShell probes /versionz, builds the client set, and runs the app
// shell until the user quits. Returns (reconnect=true, nil) when the
// shell exited for re-auth (/connect) — main's loop then re-runs the
// connection screen with the updated profile.
func runShell(profile *config.Profile, launchDir string) (bool, error) {
	// The launch-time project prompt is armed ONLY on the FIRST shell of a launch.
	//
	// A /connect round trip (or a rejected stored session) re-enters this function,
	// and that is a CONTINUATION of the session rather than a new launch: asking the
	// same question again there would be exactly the nagging the feature is built to
	// avoid, and the operator has already given an answer this session.
	launchOption := tui.WithLaunchDir(launchDir)
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
		// Password mode: auto-refresh the 900s access token with the
		// stored 24h refresh token (Phase 2b). Empty in api-key mode —
		// the refresh interceptor is inert.
		RefreshToken: profile.RefreshToken,
	})
	// Launch-time credential check.
	//
	// The operator's rule: user+password against the built-in IdP is the NORMAL
	// way to use orch, so a session that has lost its credentials must ASK for
	// them at launch rather than entering a shell that 401s silently on every
	// send. This probe runs THROUGH the client's refresh interceptor (an expired
	// access token with a working refresh token refreshes and retries here), so
	// it only fires when the session genuinely cannot authenticate.
	//
	// Only Unauthenticated counts as a credential failure. PermissionDenied means
	// the credential IS valid and the entitlement is not; bouncing that back to
	// the connection screen would ask for credentials that are already correct
	// and loop forever.
	identity, probeErr := probeIdentity(cl)
	if probeErr != nil && connect.CodeOf(probeErr) == connect.CodeUnauthenticated {
		return true, nil // main reopens the connection screen, with a reason
	}
	app := tui.NewApp(cl, profile, serverVersion, launchOption)
	if identity != "" {
		app.SetIdentity(identity)
	}
	// Open on Ask (the GUI nav's first entry) instead of an empty shell;
	// its streams start on the first WindowSizeMsg.
	app.SwitchTo(tui.TabAsk)
	prog := tea.NewProgram(app, tea.WithAltScreen(), tea.WithMouseCellMotion(),
		tea.WithReportFocus())
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

// probeIdentity resolves the footer identity display name (best effort:
// admin-gated RPC — an empty string just hides the identity chip).
// probeIdentity makes one cheap AUTHENTICATED call and returns the caller's
// display name. The error is returned rather than swallowed so the caller can
// tell "this credential is dead" from "this instance is unreachable": the
// first must ASK for credentials before entering the shell, the second must
// still open the shell degraded (a stale config is non-blocking by design).
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
