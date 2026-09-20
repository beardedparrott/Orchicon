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
func Save(path string, cfg *Config) error {
	if cfg == nil {
		return errors.New("config: nil")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(render(cfg)), 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// Resolve picks the effective profile: env overrides first (ephemeral,
// never persisted), then the active profile from the file. Returns nil
// when nothing is configured — the connection screen's cue.
func Resolve(cfg *Config) *Profile {
	envURL := strings.TrimSpace(os.Getenv(EnvURL))
	envToken := os.Getenv(EnvToken)
	if envURL != "" {
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
	if cfg == nil || cfg.Active == "" {
		return nil
	}
	p, ok := cfg.Profiles[cfg.Active]
	if !ok || p == nil {
		return nil
	}
	cp := *p
	if envToken != "" {
		cp.Token = envToken // token override on the active profile, in memory only
	}
	cp.URL = strings.TrimRight(cp.URL, "/")
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
