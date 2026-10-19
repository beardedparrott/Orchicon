// Package config persists orch's per-instance connection profiles in
// ~/.orchicon/config (TOML, 0600) and resolves the effective profile at
// launch. Environment overrides (ORCHICON_URL / ORCHICON_TOKEN) win over
// the file and are never persisted.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/controlport"
)

// AuthMethod selects how orch authenticates to the plane.
type AuthMethod string

const (
	AuthAPIKey   AuthMethod = "apikey"   // GUI-created identity API key (oc_...)
	AuthPassword AuthMethod = "password" // local username/password (access token, expires)
)

// Env overrides. They win over the config file at resolve time.
const (
	EnvURL   = "ORCHICON_URL"
	EnvToken = "ORCHICON_TOKEN"
	// EnvProfile pins THIS PROCESS to a NAMED profile without touching the file's
	// `active`.
	//
	// THAT SEPARATION IS THE POINT. `active` is a single global slot: it says which
	// profile a bare `orch` uses, and every connect sets it, so it cannot express "this
	// process is the dev one" — two processes reading it are reading the same answer.
	// A named selection is per-process (an environment variable IS process state), so
	// several orch processes can hold several instances at once with no interference:
	//
	//	ORCHICON_PROFILE=localhost-8080 orch   # this shell is dev
	//	ORCHICON_PROFILE=localhost-8091 orch   # this shell is prod
	//
	// An unknown name resolves to NO profile rather than falling back to `active` — a
	// silent fallback is how an operator who asked for one instance ends up talking to
	// another, which is the failure this exists to prevent.
	EnvProfile = "ORCHICON_PROFILE"
	// EnvTheme is an explicit palette override. It wins over the config file so
	// the theme can be pinned in an environment where the config is not
	// writable or not persisted (a launcher with an ephemeral HOME, a container,
	// CI). When it is set, orch reports it rather than silently saving.
	EnvTheme = "ORCHICON_THEME"
)

// Profile is one instance connection.
type Profile struct {
	Name       string     `toml:"-"`
	URL        string     // base URL, e.g. https://orch.example.com
	AuthMethod AuthMethod // apikey | password
	Token      string     // API key (oc_…) or access token from local-login
	Username   string     // password mode only; the password is never stored
	// RefreshToken is password mode's refresh token from the HttpOnly
	// orchicon_refresh Set-Cookie on local-login (24h TTL). Enables the
	// client's auto-refresh of the 900s access token.
	RefreshToken       string
	InsecureSkipVerify bool // TLS skip-verify for self-signed dev instances
	// Newline is the chat dock's newline-insertion chord: alt+enter
	// (default) | backslash-enter | both. bubbletea v1.3.10 has no kitty
	// keyboard protocol support, so Shift+Enter cannot be enabled
	// programmatically; CSI-u shift+enter is accepted when the terminal
	// emits it anyway.
	Newline string
	// Theme selects the TUI palette. The TUI owns its palette set (see
	// internal/tui/theme): the generated families, the hand-written community ports, and the transparent
	// variants of both. These are chosen and validated for TERMINAL contrast — not copied from
	// the GUI's CSS tokens, whose borders are hairlines that vanish on a
	// terminal. /theme lists the installed set.
	//
	// The value is stored VERBATIM and never validated here: the palette set grows (it is 41 palettes as of
	// the community ports), so a ring-fenced list in the config would turn every new theme into a migration,
	// and a name this build does not know is resolved at Use() time where the fallback is the launch default.
	Theme string
}

// Config is the on-disk document.
type Config struct {
	Active   string              // name of the profile used at launch
	Profiles map[string]*Profile // keyed by profile name
	// Theme is the TUI palette preference, stored at the TOP LEVEL so it
	// survives independently of any profile. It used to live only inside
	// [profiles.<name>], which meant a theme never persisted when the session
	// was env-driven (ORCHICON_URL/TOKEN resolve to a synthetic "env" profile
	// that is deliberately never written), or on a first run with no config
	// file yet — the operator's "themes are not saving when you exit orch and
	// re-enter". A display preference is not a credential, so it no longer
	// depends on one being saved.
	Theme string
	// CollapsedGroups is the set of COLLAPSED category folders across the TUI's
	// grouped lists, keyed "<page>:<category-id>" (e.g. "conversations:cat-1").
	//
	// TOP LEVEL, for the same reason Theme is: it is a DISPLAY preference, not a
	// credential, and it has to survive a session that was launched from
	// ORCHICON_URL/TOKEN — where the profile is a synthetic "env" that is
	// deliberately never written. The operator: "Conversation categories don't stay
	// collapsed when you leave orch and come back in."
	//
	// IT STORES WHAT IS *CLOSED*, not what is open. That is the cheaper rule to keep
	// correct: a grouping created later is absent from the set and therefore
	// EXPANDED, which is what a new folder should be. Storing the open set would
	// make every new grouping silently collapsed until the operator opened it.
	CollapsedGroups []string
	// DiffRailWidth is the operator's width for the TUI's left DIFF RAIL, in
	// cells (0 = auto/proportional). TOP LEVEL, for the same reason Theme and
	// CollapsedGroups are: it is a DISPLAY preference, not a credential, and it
	// has to survive a session launched from ORCHICON_URL/ORCHICON_TOKEN — where
	// the profile is a synthetic "env" that is deliberately never written.
	//
	// 0 is written back on a reset-to-auto, so the file never pins a width the
	// operator has abandoned.
	DiffRailWidth int
	// ProjectThemes binds a palette to a PROJECT, keyed by project ID (not name or slug — a slug can
	// change and a name would churn this file on rename; the id is the stable key). TOP LEVEL, for the
	// same reason Theme is: a per-operator display preference, not a tenant fact, that has to survive a
	// session launched from ORCHICON_URL/ORCHICON_TOKEN where the profile is a synthetic "env" that is
	// deliberately never written.
	//
	// This is a CLIENT-SIDE, TUI-ONLY preference, by design, not an oversight: a `theme` field on the
	// project row was considered and rejected, because it would put a terminal-validated palette name
	// into platform data the GUI cannot honour — the GUI's theming is CSS tokens with hairline borders
	// (see Theme's doc above), not this palette set. Putting it there would cost a migration plus proto
	// plus both clients' forms for a value only one client can ever use. So it lives here, same as Theme.
	//
	// Values are stored VERBATIM and resolved at Use() time, same as Theme: a project bound to a palette
	// this build does not know (a removed palette, a hand-edit) degrades to the default rather than
	// failing to load or rendering broken.
	ProjectThemes map[string]string
	// ListSharePct is the operator's width preference for the TREE/LIST pane of every master-detail screen
	// (work items, executions, workers …), as a percentage of the content width (0 = the default even split).
	//
	// It is a DIFFERENT preference from DiffRailWidth and deliberately so: that one sizes the diff rail
	// beside the content, this one sizes the list INSIDE a screen. Reading a long work-item name is a
	// standing preference, not a per-session choice, which is why it persists.
	ListSharePct int
}

// FileName / DirName are the fixed locations under the user's home dir.
const (
	DirName  = ".orchicon"
	FileName = "config"
	// EnvConfigDir overrides the config DIRECTORY. It exists because the
	// default is derived from $HOME, and a $HOME that is not writable (a
	// root-owned home, a read-only mount, a launcher with an ephemeral HOME)
	// makes EVERY save fail — profile, token, theme, newline mode. The TUI
	// reports that failure, but reporting is not fixing: the operator needs a
	// way to say where the config may live. Set it to a directory this user
	// owns and it survives rebuilds and container restarts ($HOME/.orchicon
	// may not exist at all, and /tmp is a tmpfs that a restart wipes).
	EnvConfigDir = "ORCHICON_CONFIG_DIR"
)

// DefaultPath returns $ORCHICON_CONFIG_DIR/config when that variable is set,
// otherwise ~/.orchicon/config.
func DefaultPath() (string, error) {
	if dir := strings.TrimSpace(os.Getenv(EnvConfigDir)); dir != "" {
		return filepath.Join(dir, FileName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, DirName, FileName), nil
}

// Load reads the config at path. A missing file yields an empty config and
// no error — first run.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{Profiles: map[string]*Profile{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	return parse(string(data))
}

// Save writes the config as TOML with 0600 perms (0700 dir). The file
// holds bearer credentials, so perms are a security requirement, not style.
//
// THE WRITE IS ATOMIC — a temp file in the SAME directory, fsync, rename — and that is
// a MULTI-PROCESS requirement, not fastidiousness. orch is meant to run more than once
// at a time (one process per instance; see ORCHICON_PROFILE), and every one of those
// processes READS and WRITES this single file: the display preferences on each toggle,
// the profile on a connect. os.WriteFile truncates IN PLACE, so a second process
// loading the config during a write could read a HALF-WRITTEN document — and a parse
// failure is not harmless here, because the launch path's recovery from an unreadable
// config is to start from an EMPTY one and then save it, destroying the other
// instance's profile and credentials. A rename replaces the whole file in one step, so
// a reader sees either the old document or the new one and never a partial one.
//
// fsync BEFORE the rename, so a crash cannot leave a renamed-but-empty file — the same
// truncation reached by another route.
func Save(path string, cfg *Config) error {
	if cfg == nil {
		return errors.New("config: nil")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	// The temp file goes in the SAME directory on purpose: a rename is only atomic within
	// one filesystem, and /tmp is commonly a different one (a tmpfs), where it would
	// silently degrade to copy-then-unlink and reopen the window this closes.
	tmp, err := os.CreateTemp(dir, FileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("create config temp: %w", err)
	}
	tmpName := tmp.Name()
	// A no-op after a successful rename (the source is gone) and the cleanup for every
	// error path below, so a failed save cannot leave credential-bearing droppings in
	// the operator's config directory.
	defer func() { _ = os.Remove(tmpName) }()
	// CreateTemp already uses 0600, and this is stated anyway: these are the perms the
	// file's own contract depends on, so they are not to be inherited from a helper.
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config temp perms: %w", err)
	}
	if _, err := tmp.WriteString(render(cfg)); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write config temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync config temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close config temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

// PreservedSuffix marks the path an unreadable config is moved aside to.
const PreservedSuffix = ".unreadable"

// PreserveUnreadable moves an unreadable config aside and returns the path it was
// preserved at, so a caller that MUST start from an empty config can do so without
// destroying what it could not read.
//
// WHY THIS EXISTS. The launch path's recovery from a load error is "start fresh, then
// the connection screen saves a new config" — and that save OVERWRITES the file. Against
// a genuinely corrupt file those bytes are already worthless; against a file this build
// merely cannot PARSE it is destruction, and that is reachable in ordinary operation.
// The parser deliberately rejects an unknown key (TestUnknownKeyStillRejected), and this
// repo deliberately keeps TWO orch vintages sharing ONE config file — the dev-tracked
// `orch` symlink and the frozen prod snapshot (see the Makefile's orch-launchers, whose
// whole point is that the snapshot "moves only on prod rebuild"). So the newer client
// writing a key the older one does not know makes every launch of the older one look
// exactly like corruption. Preserving the file means the wipe is never silent and never
// final — and proves what was there, which an overwrite cannot.
//
// RENAME, not copy: it is atomic, and it guarantees the next Save cannot clobber the
// original, which is the entire point.
func PreserveUnreadable(path string) (string, error) {
	stamp := time.Now().UTC().Format("20060102T150405")
	backup := path + PreservedSuffix + "-" + stamp
	// A second unreadable config inside the same second must not overwrite the first
	// backup — the one that already holds an operator's live credentials.
	for n := 1; ; n++ {
		if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
			break
		}
		if n > 1000 {
			return "", fmt.Errorf("config: no free name to preserve %s", path)
		}
		backup = fmt.Sprintf("%s%s-%s-%d", path, PreservedSuffix, stamp, n)
	}
	if err := os.Rename(path, backup); err != nil {
		return "", fmt.Errorf("preserve unreadable config: %w", err)
	}
	return backup, nil
}

// Resolve picks the effective profile: env overrides first (ephemeral,
// never persisted), then the active profile from the file. Returns nil
// when nothing is configured — the connection screen's cue.
func Resolve(cfg *Config) *Profile { return ResolveNamed(cfg, "") }

// ResolveNamed is Resolve with an EXPLICIT profile selection (ORCHICON_PROFILE, or the
// --profile flag), which is what makes more than one orch process possible at once.
// name is trimmed; empty means "no explicit selection", in which case ORCHICON_PROFILE
// is consulted and then the file's `active` is used, exactly as before.
//
// PRECEDENCE, and the one deliberate difference from the URL/token overrides:
//
//   - No selection + ORCHICON_URL  → the EPHEMERAL "env" profile, never persisted. This
//     is the ad-hoc "just point me at this host" form and it is unchanged.
//   - A selection + ORCHICON_URL   → the NAMED profile's credentials against the URL the
//     environment asked for (a tunnel, a remote host, a moved port). Stated because it is
//     the only combination that mixes the two, and it is the useful reading: the
//     credential identifies WHO, the URL says WHERE.
//   - A selection + no ORCHICON_URL → that profile, verbatim.
func ResolveNamed(cfg *Config, name string) *Profile {
	name = strings.TrimSpace(name)
	if name == "" {
		name = strings.TrimSpace(os.Getenv(EnvProfile))
	}
	envURL := strings.TrimSpace(os.Getenv(EnvURL))
	envToken := os.Getenv(EnvToken)

	// The ephemeral env profile, still reached ONLY when nothing was named.
	if envURL != "" && name == "" {
		p := &Profile{
			Name:       "env",
			URL:        strings.TrimRight(envURL, "/"),
			AuthMethod: AuthAPIKey,
			Token:      envToken,
		}
		if p.Token == "" {
			p.AuthMethod = "" // connection screen must still collect a credential
		}
		return p
	}

	if cfg == nil {
		return nil
	}
	slot := name
	if slot == "" {
		slot = cfg.Active
	}
	if slot == "" {
		return nil
	}
	p, ok := cfg.Profiles[slot]
	if !ok || p == nil {
		// AN ABSENT NAMED PROFILE RESOLVES TO NOTHING — it does NOT fall back to
		// `active`. Falling back would silently connect an operator who asked for one
		// instance to a different one, which is precisely the confusion the explicit
		// selection exists to remove (and the caller reports the name it could not find).
		return nil
	}
	cp := *p
	if envToken != "" {
		cp.Token = envToken // token override, in memory only
	}
	if envURL != "" {
		cp.URL = strings.TrimRight(envURL, "/")
	} else {
		cp.URL = strings.TrimRight(cp.URL, "/")
	}
	return &cp
}

// render emits minimal TOML. Kept hand-rolled (architect decision §3:
// trivial struct, no new dependency).
func render(cfg *Config) string {
	var b strings.Builder
	b.WriteString("# orch — Orchicon remote client config (0600; holds credentials)\n")
	fmt.Fprintf(&b, "active = %q\n", cfg.Active)
	if cfg.Theme != "" {
		fmt.Fprintf(&b, "theme = %q\n", cfg.Theme)
	}
	// The collapsed folders, SORTED so a rewrite of unchanged state is byte-identical. Without that, the
	// map's iteration order would churn the file on every toggle and the diff would look like a change
	// even when nothing did.
	if len(cfg.CollapsedGroups) > 0 {
		keys := append([]string(nil), cfg.CollapsedGroups...)
		sortStrings(keys)
		fmt.Fprintf(&b, "collapsed_groups = [%s]\n", quoteList(keys))
	}
	// The diff rail's width, OMITTED at 0 so "auto" never writes a key that would
	// pin a value the operator never chose (the same contract collapsed_groups uses).
	if cfg.DiffRailWidth > 0 {
		fmt.Fprintf(&b, "diff_rail_width = %d\n", cfg.DiffRailWidth)
	}
	// The project→palette bindings, SORTED by project id so an unchanged map rewrites byte-identical —
	// the same reason collapsed_groups sorts, above.
	if len(cfg.ProjectThemes) > 0 {
		ids := make([]string, 0, len(cfg.ProjectThemes))
		for id := range cfg.ProjectThemes {
			ids = append(ids, id)
		}
		sortStrings(ids)
		b.WriteString("\n[project_themes]\n")
		for _, id := range ids {
			fmt.Fprintf(&b, "%s = %q\n", id, cfg.ProjectThemes[id])
		}
	}
	// The master-detail split, under the same omit-at-zero contract.
	if cfg.ListSharePct > 0 {
		fmt.Fprintf(&b, "list_share_pct = %d\n", cfg.ListSharePct)
	}
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		p := cfg.Profiles[name]
		if p == nil {
			continue
		}
		fmt.Fprintf(&b, "\n[profiles.%s]\n", name)
		fmt.Fprintf(&b, "url = %q\n", p.URL)
		fmt.Fprintf(&b, "auth_method = %q\n", string(p.AuthMethod))
		fmt.Fprintf(&b, "token = %q\n", p.Token)
		fmt.Fprintf(&b, "username = %q\n", p.Username)
		fmt.Fprintf(&b, "refresh_token = %q\n", p.RefreshToken)
		fmt.Fprintf(&b, "insecure_skip_verify = %t\n", p.InsecureSkipVerify)
		if p.Theme != "" {
			fmt.Fprintf(&b, "theme = %q\n", p.Theme)
		}
	}
	return b.String()
}

// parse reads back exactly what render writes (quoted strings + bools).
func parse(data string) (*Config, error) {
	cfg := &Config{Profiles: map[string]*Profile{}}
	var cur *Profile
	inProjectThemes := false
	for i, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[profiles.") && strings.HasSuffix(line, "]") {
			name := strings.TrimSuffix(strings.TrimPrefix(line, "[profiles."), "]")
			if name == "" {
				return nil, fmt.Errorf("config line %d: empty profile name", i+1)
			}
			cur = &Profile{Name: name}
			cfg.Profiles[name] = cur
			inProjectThemes = false
			continue
		}
		// MANDATORY, not optional: render() above writes this section whenever a binding exists, so
		// without this case the operator's OWN config (the moment they bind one palette) would fail to
		// load outright — the "default" key below rejects any key it does not recognise.
		if line == "[project_themes]" {
			cur = nil
			inProjectThemes = true
			continue
		}
		if inProjectThemes {
			id, value, ok := strings.Cut(line, "=")
			if !ok {
				// A malformed entry DEGRADES to "no binding for that project" rather than failing the
				// whole load — same rule parseList follows below: a display preference must never be
				// able to stop the operator connecting.
				continue
			}
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if cfg.ProjectThemes == nil {
				cfg.ProjectThemes = map[string]string{}
			}
			cfg.ProjectThemes[id] = unquote(strings.TrimSpace(value))
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("config line %d: expected key = value", i+1)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "active":
			cfg.Active = unquote(value)
		case "url":
			if cur != nil {
				cur.URL = unquote(value)
			}
		case "auth_method":
			if cur != nil {
				cur.AuthMethod = AuthMethod(unquote(value))
			}
		case "token":
			if cur != nil {
				cur.Token = unquote(value)
			}
		case "username":
			if cur != nil {
				cur.Username = unquote(value)
			}
		case "refresh_token":
			if cur != nil {
				cur.RefreshToken = unquote(value)
			}
		case "insecure_skip_verify":
			if cur != nil {
				b, err := strconv.ParseBool(value)
				if err != nil {
					return nil, fmt.Errorf("config line %d: %w", i+1, err)
				}
				cur.InsecureSkipVerify = b
			}
		case "theme":
			// Top-level (outside any [profiles.*] section) is the TUI palette
			// preference; inside a profile it is the legacy per-profile value.
			if cur != nil {
				cur.Theme = unquote(value)
			} else {
				cfg.Theme = unquote(value)
			}
		case "collapsed_groups":
			if cur == nil {
				cfg.CollapsedGroups = parseList(value)
			}
		case "diff_rail_width":
			// MANDATORY, not optional: the `default` below REJECTS an unknown key, so
			// without this case a file render() wrote would fail to load — a hard
			// startup error, not a silent default.
			if cur == nil {
				n, err := strconv.Atoi(value)
				if err != nil {
					return nil, fmt.Errorf("config line %d: %w", i+1, err)
				}
				cfg.DiffRailWidth = n
			}
		case "list_share_pct":
			// MANDATORY for the same reason diff_rail_width's case is: render writes it, and the `default`
			// below rejects an unknown key, so a config this program wrote must load.
			if cur == nil {
				n, err := strconv.Atoi(value)
				if err != nil {
					return nil, fmt.Errorf("config line %d: %w", i+1, err)
				}
				cfg.ListSharePct = n
			}
		case "newline":
			if cur != nil {
				cur.Newline = unquote(value)
			}
		default:
			return nil, fmt.Errorf("config line %d: unknown key %q", i+1, key)
		}
	}
	return cfg, nil
}

func unquote(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		out, err := strconv.Unquote(v)
		if err == nil {
			return out
		}
		return v[1 : len(v)-1]
	}
	return v
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// quoteList renders strings as a TOML array element list: "a", "b".
func quoteList(ss []string) string {
	parts := make([]string, 0, len(ss))
	for _, s := range ss {
		parts = append(parts, strconv.Quote(s))
	}
	return strings.Join(parts, ", ")
}

// parseList reads back a TOML array element list written by quoteList. It is deliberately tolerant:
// a malformed entry is DROPPED rather than failing the whole config load, because a display preference
// must never be able to stop the operator connecting.
func parseList(v string) []string {
	v = strings.TrimSpace(v)
	v = strings.TrimSuffix(strings.TrimPrefix(v, "["), "]")
	if strings.TrimSpace(v) == "" {
		return nil
	}
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '"':
			inQuote = !inQuote
			if !inQuote {
				out = append(out, cur.String())
				cur.Reset()
			}
		case inQuote:
			// An escaped quote inside the string (\") stays part of it.
			if c == '\\' && i+1 < len(v) {
				i++
				cur.WriteByte(v[i])
				continue
			}
			cur.WriteByte(c)
		}
	}
	return out
}

// InstanceURL is the control URL of a named LOCAL instance.
//
// IT DELEGATES TO internal/controlport, WHICH IS THE POINT: the installer owns the
// per-instance control ports (cmd/orchicon/install_ports.go), the desktop app already
// consumes that table, and a third copy here would be a third thing to keep equal. An
// unknown name yields "" — the caller must refuse rather than substitute another
// instance, so "prod" can never quietly resolve to dev.
func InstanceURL(instance string) string {
	port, ok := controlport.Defaults()[instance]
	if !ok {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// ProfileForInstance returns the SAVED profile whose URL points at that instance's
// control port, or nil.
//
// WHY MATCH ON THE PORT RATHER THAN THE NAME: profiles are keyed by their host:port
// (config.go's package note), not by instance name, because a profile can outlive a
// moved port or describe a remote host. controlport.InstanceForURL is the one place
// that mapping is defined, so this reuses it instead of re-deriving "which instance is
// this URL".
//
// The returned profile is the SAVED one (not a copy) because the caller copies it before
// retargeting — and that caller-side copy is deliberate: a switch must never rewrite the
// operator's stored credentials.
func ProfileForInstance(cfg *Config, instance string) *Profile {
	if cfg == nil || instance == "" {
		return nil
	}
	for _, p := range cfg.Profiles {
		if p == nil {
			continue
		}
		if got, ok := controlport.InstanceForURL(p.URL); ok && got == instance {
			return p
		}
	}
	return nil
}
