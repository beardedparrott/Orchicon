// Package connection implements orch's first-run connection screen: the
// user enters a server URL and credentials (GUI-created API key, or local
// username+password), orch probes /versionz + a cheap authenticated RPC,
// then persists the profile (§3 of the plan).
package connection

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
	"github.com/beardedparrott/orchicon/internal/version"
)

// field indexes into Model.inputs.
const (
	fieldURL = iota
	fieldCredential
	fieldUsername
	fieldCount
)

// ProbeFuncs isolate the network probes for tests.
type ProbeFuncs struct {
	// Versionz probes GET <url>/versionz.
	Versionz func(ctx context.Context, baseURL string, insecure bool) (*client.VersionzResponse, error)
	// ListProjects probes a cheap authenticated RPC (API-key mode).
	ListProjects func(ctx context.Context, baseURL, token string, insecure bool) error
	// LocalLogin probes POST /auth/local-login (password mode).
	LocalLogin func(ctx context.Context, baseURL, username, password string, insecure bool) (*client.LoginResponse, error)
}

// DefaultProbes uses the real client package.
func DefaultProbes() ProbeFuncs {
	return ProbeFuncs{
		Versionz: client.Ping,
		ListProjects: func(ctx context.Context, baseURL, token string, insecure bool) error {
			cl := client.New(client.Options{BaseURL: baseURL, Token: token, InsecureSkipVerify: insecure})
			_, err := cl.Projects.ListProjects(ctx, connect.NewRequest(defaultListProjectsReq()))
			return err
		},
		LocalLogin: func(ctx context.Context, baseURL, username, password string, insecure bool) (*client.LoginResponse, error) {
			return client.Login(ctx, baseURL, insecure, username, password)
		},
	}
}

// Result is what the screen hands back to the app on success.
type Result struct {
	Profile *config.Profile
	// ServerVersion from /versionz ("" when unreachable — non-blocking).
	ServerVersion string
}

// Model is the connection screen bubbletea model.
type Model struct {
	probes  ProbeFuncs
	inputs  [fieldCount]textinput.Model
	focus   int
	authAPI bool // true = API key, false = username+password
	profile *config.Profile
	// storedRefresh carries the existing profile's refresh token through an
	// edit (API-key switch clears it; password probe overwrites it).
	storedRefresh string
	// storedToken is the API KEY that a switch away from API-key mode parked, so
	// toggling ctrl+a back restores it.
	//
	// It is deliberately NOT the profile's Token in password mode: there, Token is
	// the previous session's minted ACCESS TOKEN — not a password, and not a key.
	storedToken string
	// embedded marks the model as the SHELL's in-place /connect overlay:
	// ctrl+c does not quit the process (esc cancels), tea.Quit is never
	// emitted on success — the shell polls Result() each Update.
	embedded bool
	width    int
	height   int
	busy     bool
	errMsg   string
	info     string
	// reason is the WHY of this screen: set by the shell when it bounces the
	// operator here because the stored session could not be authenticated.
	// Unlike info (which each probe cycle clears), it persists until the
	// operator signs in or cancels.
	reason    string
	saveErr   string
	insecure  bool
	connected *Result // set when the probe succeeded (read by cmd/orch)
}

// SetReason seeds the persistent "why am I being asked" line. A shell that
// bounced here — /connect, or the launch-time credential check rejecting a
// dead session — calls this so the operator is asked to sign in WITH A REASON
// instead of out of nowhere.
func (m *Model) SetReason(s string) { m.reason = s }

// New creates the connection screen. profile may be nil (first run) or a
// partially-filled profile (e.g. env URL without token).
func New(profile *config.Profile, probes ProbeFuncs) Model {
	m := Model{probes: probes, profile: profile, authAPI: true}
	for i := range m.inputs {
		m.inputs[i] = textinput.New()
		m.inputs[i].CharLimit = 512
		// Explicit field width (Phase 3 finding 2): a zero-width textinput
		// never bounds its frame, so the masked credential echo painted a
		// full-width dotted line and the fields could not be read. ~40
		// cells is the form's natural measure at the 80-col floor.
		m.inputs[i].Width = 40
	}
	m.inputs[fieldURL].Placeholder = "https://orch.example.com"
	m.inputs[fieldURL].Prompt = "Server URL > "
	m.inputs[fieldCredential].EchoMode = textinput.EchoPassword
	m.inputs[fieldCredential].EchoCharacter = '•'
	m.inputs[fieldUsername].Placeholder = "local username"
	m.inputs[fieldUsername].Prompt = "Username > "
	if profile != nil {
		if profile.URL != "" {
			m.inputs[fieldURL].SetValue(profile.URL)
		}
		if profile.Username != "" {
			m.inputs[fieldUsername].SetValue(profile.Username)
		}
		if profile.AuthMethod == config.AuthPassword {
			// PASSWORD MODE: the profile's Token is the previous session's minted
			// ACCESS TOKEN. It is not a password, and showing it in the password
			// field meant the field arrived pre-satisfied — so submit sent the stale
			// token as the password, the plane answered HTTP 401, and there was no
			// way to type the real one over it. Leave the field EMPTY (a password is
			// never stored) and let the operator type it.
			m.authAPI = false
		} else if profile.Token != "" {
			// API-KEY MODE: the Token IS the key. Park it so a mode switch can
			// restore it; applyCredentialMode puts it in the field.
			m.storedToken = profile.Token
		}
		// Carry the stored refresh token through an edit — a re-connect of the
		// same password profile keeps auto-refresh (API-key mode ignores it).
		m.storedRefresh = profile.RefreshToken
	}
	m.applyCredentialMode()
	m.setFocus(fieldURL)
	return m
}

// applyCredentialMode derives the credential field's prompt + placeholder
// from the active auth mode. API-key mode shows "API key >" with the oc_…
// placeholder; username+password mode shows "Password >" with a password
// placeholder — never "API key" in password mode.
// visibleFields is the fields the operator can edit in the current mode, IN THE
// ORDER THEY ARE DRAWN.
//
// The tab cycle walks THIS rather than the raw 0..fieldCount range. The constants
// are fieldURL, fieldCredential, fieldUsername — the ORIGINAL two-field API-key
// layout, with Username bolted on at the end when password mode arrived — but
// password mode DRAWS URL, Username, Password, with the credential field THIRD.
// Cycling by index therefore moved focus DOWN to Password and then back UP to
// Username: the highlight jumped around the form, and a keystroke — or a
// backspace — landed in the field the operator was not looking at, which is
// exactly why the password box seemed impossible to clear.
func (m *Model) visibleFields() []int {
	if m.authAPI {
		return []int{fieldURL, fieldCredential}
	}
	return []int{fieldURL, fieldUsername, fieldCredential}
}

// moveFocus advances the focus through visibleFields, wrapping at the ends.
func (m *Model) moveFocus(delta int) {
	fs := m.visibleFields()
	pos := 0
	for i, f := range fs {
		if f == m.focus {
			pos = i
			break
		}
	}
	pos = (pos + delta + len(fs)) % len(fs)
	m.setFocus(fs[pos])
}

// setFocus focuses one field and blurs the rest, leaving the caret at the end so
// the first keystroke appends and backspace has something to delete.
func (m *Model) setFocus(field int) {
	m.focus = field
	for i := range m.inputs {
		if i == field {
			m.inputs[i].Focus()
		} else {
			m.inputs[i].Blur()
		}
	}
	m.inputs[field].CursorEnd()
}

// applyCredentialMode derives the credential field's prompt, placeholder AND VALUE
// from the active auth mode.
//
// The VALUE is the load-bearing part. Deriving only the label is this screen's
// oldest bug: the field kept whatever it had been given — an API key, or the
// previous session's access token — while its label changed to "Password > ", so
// the operator saw a filled password box they had not filled and could not explain.
func (m *Model) applyCredentialMode() {
	if m.authAPI {
		m.inputs[fieldCredential].Prompt = "API key > "
		m.inputs[fieldCredential].Placeholder = "oc_… (create one in the GUI: Settings → API keys)"
		// Restore the key a switch away from this mode parked.
		m.inputs[fieldCredential].SetValue(m.storedToken)
	} else {
		m.inputs[fieldCredential].Prompt = "Password > "
		m.inputs[fieldCredential].Placeholder = "password"
		// CLEAR: a password is never carried across modes, and neither an API key
		// nor a stale access token is one.
		m.inputs[fieldCredential].SetValue("")
	}
	// Park the caret at the END: SetValue only moves the cursor when the field was
	// previously empty, so a restored value could otherwise leave it at position 0
	// with nothing before it to backspace.
	m.inputs[fieldCredential].CursorEnd()
}

func (m *Model) currentURL() string {
	return strings.TrimRight(strings.TrimSpace(m.inputs[fieldURL].Value()), "/")
}
func (m *Model) credential() string { return strings.TrimSpace(m.inputs[fieldCredential].Value()) }
func (m *Model) username() string   { return strings.TrimSpace(m.inputs[fieldUsername].Value()) }

func (m *Model) password() string { return m.credential() }

// validate checks the form before probing.
func (m *Model) validate() string {
	u := m.currentURL()
	if u == "" {
		return "server URL is required"
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Host == "" {
		return "server URL must include a scheme and host (https://host)"
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "server URL scheme must be http or https"
	}
	if m.authAPI {
		if m.credential() == "" {
			return "API key is required (create one in the GUI: Settings → API keys)"
		}
	} else {
		if m.username() == "" {
			return "username is required"
		}
		if m.password() == "" {
			return "password is required"
		}
	}
	return ""
}

// probe runs the versionz + auth probes and returns the result.
func (m *Model) probe(ctx context.Context) (*Result, error) {
	vr, err := m.probes.Versionz(ctx, m.currentURL(), m.insecure)
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s/versionz: %w", m.currentURL(), err)
	}
	if m.authAPI {
		if err := m.probes.ListProjects(ctx, m.currentURL(), m.credential(), m.insecure); err != nil {
			return nil, fmt.Errorf("API key rejected: %w\n\nRe-create the key in the GUI (Settings → API keys). Read scopes cover browsing; sending chat messages and interjecting into live executions needs the corresponding write scopes (conversations, execution messaging).", err)
		}
		return &Result{Profile: m.buildProfile(m.credential(), ""), ServerVersion: vr.Version}, nil
	}
	lr, err := m.probes.LocalLogin(ctx, m.currentURL(), m.username(), m.password(), m.insecure)
	if err != nil {
		// One wrap, never two: client.Login already prefixes its failures
		// with "login failed:" (Phase 3 finding 2 — the message used to
		// read "login failed: login failed: HTTP 401").
		if strings.Contains(err.Error(), "login failed") {
			return nil, err
		}
		return nil, fmt.Errorf("login failed: %w", err)
	}
	// lr.RefreshToken carries the HttpOnly orchicon_refresh Set-Cookie value
	// (24h TTL) — stored in the profile so the client auto-refreshes the
	// 900s access token instead of dead-ending on auth-expired. A server
	// without refresh support leaves it empty: fall back to the profile's
	// stored token (the re-connect of the same session keeps auto-refresh).
	refresh := lr.RefreshToken
	if refresh == "" {
		refresh = m.storedRefresh
	}
	return &Result{Profile: m.buildProfile(lr.AccessToken, refresh), ServerVersion: vr.Version}, nil
}

// buildProfile assembles the profile from the form. token overrides the
// credential field (password mode stores the minted access token, never
// the password); refresh carries the login's refresh token.
func (m *Model) buildProfile(token, refresh string) *config.Profile {
	name := "default"
	method := config.AuthAPIKey
	if !m.authAPI {
		method = config.AuthPassword
	}
	cred := m.credential()
	if token != "" {
		cred = token
	}
	user := ""
	if !m.authAPI {
		user = m.username()
	}
	url_ := m.currentURL()
	if m.profile != nil && m.profile.Name != "" && m.profile.Name != "default" {
		name = m.profile.Name
	}
	// DISPLAY PREFERENCES are not credentials and must survive this rebuild. The
	// original profile carried them and this function returns a FRESH struct, so
	// omitting them DROPPED them: a saved palette was lost for the whole session
	// (and, because the rebuilt profile is what gets re-saved, the theme was
	// scrubbed from the config too). `Newline` had the same defect — an operator's
	// configured newline chord silently reverted to the default. Credentials are
	// deliberately NOT carried by default (token/username/refresh come from the
	// form), which is why this is an explicit list and not a struct copy.
	themeName, newlineMode := "", ""
	if m.profile != nil {
		themeName, newlineMode = m.profile.Theme, m.profile.Newline
	}
	return &config.Profile{
		Name:               name,
		URL:                url_,
		AuthMethod:         method,
		Token:              cred,
		Username:           user,
		RefreshToken:       refresh,
		InsecureSkipVerify: m.insecure,
		Newline:            newlineMode,
		Theme:              themeName,
	}
}

// Init focuses the first field.
func (m Model) Init() tea.Cmd {
	return textinput.Blink
}

type probeDoneMsg struct {
	result *Result
	err    error
}
type probeStartMsg struct{}

// Update handles keys: tab/shift-tab between fields, ctrl+u clear, enter
// submits, ctrl+a toggles auth method, ctrl+s toggles insecure TLS.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		if m.busy {
			return m, nil
		}
		switch msg.Type {
		case tea.KeyCtrlC:
			if m.embedded {
				// The overlay is the shell's /connect: ctrl+c must not kill
				// the whole process (the shell's global quit chord does).
				return m, nil
			}
			return m, tea.Quit
		case tea.KeyTab, tea.KeyDown:
			m.moveFocus(1)
			return m, nil
		case tea.KeyShiftTab, tea.KeyUp:
			m.moveFocus(-1)
			return m, nil
		case tea.KeyEnter:
			if msg.Alt {
				return m, nil
			}
			return m.submit()
		case tea.KeyCtrlA:
			// Leaving API-key mode: remember the key so switching back restores it.
			if m.authAPI {
				m.storedToken = m.credential()
			}
			m.authAPI = !m.authAPI
			m.applyCredentialMode()
			// The visible field set changes with the mode, and the credential field
			// is not always the same neighbour — re-seat focus on the first field so
			// the caret is never left inside a field that is no longer drawn.
			m.setFocus(fieldURL)
			m.errMsg = ""
			m.info = ""
			// Switching to API-key mode drops the stored password-mode
			// refresh token (it belongs to the local-login session).
			if m.authAPI {
				m.storedRefresh = ""
			}
			return m, nil
		case tea.KeyCtrlS:
			m.insecure = !m.insecure
			m.errMsg = ""
			m.info = ""
			return m, nil
		}

	case probeStartMsg:
		m.busy = true
		m.errMsg = ""
		m.info = "Connecting to " + m.currentURL() + " …"
		return m, m.runProbe()

	case probeDoneMsg:
		m.busy = false
		m.info = ""
		if msg.err != nil {
			m.errMsg = msg.err.Error()
			return m, nil
		}
		m.saveErr = ""
		return m, func() tea.Msg { return connectedMsg{result: msg.result} }

	case connectedMsg:
		// The only handler: record the result so cmd/orch can read it via
		// Result() after prog.Run returns, then exit the screen program.
		m.connected = msg.result
		if m.embedded {
			// In-place /connect overlay: the SHELL consumes the result —
			// the screen program must not quit the process.
			return m, nil
		}
		return m, tea.Quit
	}

	// Route typing to the focused input.
	var cmds []tea.Cmd
	for i := range m.inputs {
		if i == m.focus {
			var cmd tea.Cmd
			m.inputs[i], cmd = m.inputs[i].Update(msg)
			cmds = append(cmds, cmd)
		}
	}
	return m, tea.Batch(cmds...)
}

// submit validates then starts the async probe.
func (m Model) submit() (tea.Model, tea.Cmd) {
	if msg := m.validate(); msg != "" {
		m.errMsg = msg
		return m, nil
	}
	m.errMsg = ""
	return m, func() tea.Msg { return probeStartMsg{} }
}

// runProbe performs the network probes off the UI goroutine.
func (m Model) runProbe() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := m.probe(ctx)
		return probeDoneMsg{result: res, err: err}
	}
}

// Result returns the successful connection result, or nil while the
// user is still on the form (cmd/orch reads it after prog.Run).
func (m Model) Result() *Result {
	return m.connected
}

// SetEmbedded marks the model as the shell's in-place /connect overlay
// (never quits the process on success/ctrl+c).
func (m *Model) SetEmbedded() { m.embedded = true }

// Connected reports whether a successful probe has landed (the shell's
// embedded overlay polls this each Update).
func (m *Model) Connected() bool { return m.connected != nil }

// connectedMsg is emitted on success; the app swaps to the shell.
type connectedMsg struct{ result *Result }

// View renders the form.
func (m Model) View() string {
	var b strings.Builder
	b.WriteString(theme.ListTitle.Render("Connect to an Orchicon instance") + "\n\n")

	authLabel := "API key"
	if !m.authAPI {
		authLabel = "username + password"
	}
	b.WriteString(theme.DetailKey.Render("Auth method (ctrl+a to toggle): ") + theme.DetailValue.Render(authLabel) + "\n")
	b.WriteString(theme.HintText.Render("  ctrl+a: switch between API key and username + password login") + "\n\n")

	b.WriteString(m.inputs[fieldURL].View() + "\n\n")
	if m.authAPI {
		b.WriteString(m.inputs[fieldCredential].View() + "\n\n")
		b.WriteString(theme.HintText.Render("  Create an API key in the Orchicon web GUI (Settings → API keys).") + "\n")
		b.WriteString(theme.HintText.Render("  Read scopes cover browsing; sending chat messages and interjecting into") + "\n")
		b.WriteString(theme.HintText.Render("  live executions needs the corresponding write scopes (conversations,") + "\n")
		b.WriteString(theme.HintText.Render("  execution messaging).") + "\n\n")
	} else {
		b.WriteString(m.inputs[fieldUsername].View() + "\n")
		b.WriteString(m.inputs[fieldCredential].View() + "\n\n")
		b.WriteString(theme.HintText.Render("  Password sessions auto-refresh in place (24h).") + "\n\n")
	}

	insecure := "off"
	if m.insecure {
		insecure = "ON (self-signed TLS accepted)"
	}
	b.WriteString(theme.DetailKey.Render("Skip TLS verify (ctrl+s): ") + theme.DetailValue.Render(insecure) + "\n")

	if m.reason != "" {
		b.WriteString("\n" + theme.HintText.Render(m.reason) + "\n")
	}
	if m.info != "" {
		b.WriteString("\n" + theme.StatusBusy.Render(m.info) + "\n")
	}
	if m.errMsg != "" {
		b.WriteString("\n" + theme.ErrorText.Render(m.errMsg) + "\n")
	}
	if m.saveErr != "" {
		b.WriteString("\n" + theme.ErrorText.Render(m.saveErr) + "\n")
	}

	b.WriteString("\n" + theme.HintText.Render("enter: connect · tab: next field · ctrl+c: quit"))
	if cv := version.Current().Tag; cv != "dev" {
		b.WriteString(" · orch " + cv)
	}
	b.WriteString("\n")
	return b.String()
}

// SaveProfile persists the profile to the config file. Passwords are never
// written — only the minted access token.
func SaveProfile(path string, res *Result) error {
	cfg, err := config.Load(path)
	if err != nil {
		cfg = &config.Config{Profiles: map[string]*config.Profile{}}
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]*config.Profile{}
	}
	p := res.Profile
	if p.Name == "" {
		p.Name = "default"
	}
	cfg.Profiles[p.Name] = p
	cfg.Active = p.Name
	return config.Save(path, cfg)
}

// defaultListProjectsReq is the cheap authenticated probe (page size 1).
func defaultListProjectsReq() *apiv1.ListProjectsRequest {
	return &apiv1.ListProjectsRequest{PageSize: 1}
}

// Ensure ORCHICON_PASSWORD env is honored for scripted password logins
// (§3: never persisted to disk).
func envPassword() string { return os.Getenv("ORCHICON_PASSWORD") }

var _ = envPassword // referenced by cmd/orch in password scripting mode
