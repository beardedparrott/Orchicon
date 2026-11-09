// Package config persists orch's per-instance connection profiles in ~/.orchicon/config (TOML,
// 0600) and resolves the effective profile at launch.
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
	// EnvTheme is an explicit palette override.
	EnvTheme = "ORCHICON_THEME"
)

// Profile is one instance connection.
type Profile struct {
	Name       string     `toml:"-"`
	URL        string     // base URL, e.g. https://orch.example.com
	AuthMethod AuthMethod // apikey | password
	Token      string     // API key (oc_…) or access token from local-login
	Username   string     // password mode only; the password is never stored
	// RefreshToken is password mode's refresh token from the HttpOnly orchicon_refresh Set-Cookie on
	// local-login (24h TTL).
	RefreshToken string
	// TokenExpiresIn is the access token's lifetime in seconds, as the server reported it
	// (LoginResponse/RefreshResponse expires_in).
	TokenExpiresIn     int64
	InsecureSkipVerify bool // TLS skip-verify for self-signed dev instances
	// Newline is the chat dock's newline-insertion chord: ctrl+j | alt+enter | backslash-enter | both.
	Newline string
	// Theme selects the TUI palette.
	Theme string
}

// Config is the on-disk document.
type Config struct {
	Active   string              // name of the profile used at launch
	Profiles map[string]*Profile // keyed by profile name
	// Theme is the TUI palette preference, stored at the TOP LEVEL so it survives independently of any
	// profile.
	Theme string
	// CollapsedGroups is the set of COLLAPSED category folders across the TUI's grouped lists, keyed
	// "<page>:<category-id>" (e.g. "conversations:cat-1").
	CollapsedGroups []string
	// DiffRailWidth is the operator's width for the TUI's left DIFF RAIL, in cells (0 =
	// auto/proportional).
	DiffRailWidth int
	// This is a CLIENT-SIDE, TUI-ONLY preference, by design, not an oversight: a `theme` field on the
	// project row was considered and rejected.
	ProjectThemes map[string]string
	// ListSharePct is the operator's width preference for the TREE/LIST pane of every master-detail
	// screen (work items, executions, workers …).
	ListSharePct int
}

// FileName / DirName are the fixed locations under the user's home dir.
const (
	DirName  = ".orchicon"
	FileName = "config"
	// EnvConfigDir overrides the config DIRECTORY.
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

// Save writes the config as TOML with 0600 perms (0700 dir).
func Save(path string, cfg *Config) error {
	if cfg == nil {
		return errors.New("config: nil")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	// The temp file goes in the SAME directory on purpose: a rename is only atomic within one
	// filesystem, and /tmp is commonly a different one (a tmpfs).
	tmp, err := os.CreateTemp(dir, FileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("create config temp: %w", err)
	}
	tmpName := tmp.Name()
	// A no-op after a successful rename (the source is gone) and the cleanup for every error path
	// below, so a failed save cannot leave credential-bearing droppings in the operator's config
	// directory.
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

// PreserveUnreadable moves an unreadable config aside and returns the path it was preserved at, so
// a caller that MUST start from an empty config can do so without destroying what it could not.
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

// Resolve picks the effective profile: env overrides first (ephemeral, never persisted), then the
// active profile from the file.
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
		// AN ABSENT NAMED PROFILE RESOLVES TO NOTHING — it does NOT fall back to `active`.
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
	// The collapsed folders, SORTED so a rewrite of unchanged state is byte-identical.
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
		// MANDATORY, not optional: render() above writes this section whenever a binding exists, so without
		// this case the operator's OWN config (the moment they bind one palette) would fail to load.
		if line == "[project_themes]" {
			cur = nil
			inProjectThemes = true
			continue
		}
		if inProjectThemes {
			id, value, ok := strings.Cut(line, "=")
			if !ok {
				// A malformed entry DEGRADES to "no binding for that project" rather than failing the whole load —
				// same rule parseList follows below.
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
			// MANDATORY, not optional: the `default` below REJECTS an unknown key, so without this case a file
			// render() wrote would fail to load — a hard startup error, not a silent default.
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

// parseList reads back a TOML array element list written by quoteList.
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
func InstanceURL(instance string) string {
	port, ok := controlport.Defaults()[instance]
	if !ok {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// ProfileForInstance returns the SAVED profile whose URL points at that instance's control port, or
// nil.
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
