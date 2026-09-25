// Package permpolicy is the ONE accessor for the operator's persistent
// permission policy: a deny/accept list in a YAML file.
//
// Why it is a package and not a field: the policy is durable operator
// policy written once, and it must be consulted by BOTH the consent core
// (before it raises an ask) and the guard shim's interactive profile
// (before a path-scoped command). A second copy of the precedence rules is
// how enforcement and prompting drift apart, so there is exactly one
// function — Decide — that implements the order, and one store that reads
// the file.
//
// PRECEDENCE (settled; the deny list sits ABOVE the session grant on
// purpose: a grant suppresses prompts, but cannot open a path the operator
// excluded):
//
//	never-allow binary class (guard shim case block + askmode mode gate)
//	  > deny list
//	  > session grant
//	  > accept list
//	  > conversation's project (default scope)
//	  > otherwise (ask)
//
// The never-allow binary class is enforced above this package (the guard
// shim's absolute `case` block and the ask mode gate); everything from the
// deny list down is Decide.
//
// RELOAD SEMANTICS: the file is read on EVERY consult. A gated decision is
// a write, an execution, or a read of a denied path — never a hot loop —
// and the file is a few hundred bytes, so reading it per decision buys
// "an edit takes effect immediately" with no watcher and no staleness
// window. Do not cache it; do not add a watcher.
package permpolicy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"go.yaml.in/yaml/v3"
)

// PolicyEnv is the env var naming the policy file.
const PolicyEnv = "ORCHICON_PERMISSION_POLICY"

// dataDirEnv mirrors internal/config's DataDir resolution.
const dataDirEnv = "ORCHICON_DATA_DIR"

// defaultDataDir mirrors internal/config.Default()'s DataDir fallback.
const defaultDataDir = "/var/lib/orchicon"

// PolicyFileName is the default file name under the data dir.
const PolicyFileName = "permission-policy.yaml"

// List names which half of the policy an entry lives in.
type List int

const (
	ListDeny List = iota
	ListAccept
)

func (l List) String() string {
	if l == ListAccept {
		return "accept"
	}
	return "deny"
}

// ParseList maps a wire/UI list name to a List. Unknown names are an
// error — a silent default would write the wrong half of the policy.
func ParseList(s string) (List, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "deny":
		return ListDeny, nil
	case "accept":
		return ListAccept, nil
	default:
		return ListDeny, fmt.Errorf("permission policy: unknown list %q (want \"deny\" or \"accept\")", s)
	}
}

// Verdict is the outcome of one gated decision.
type Verdict int

const (
	// VerdictNone is the zero value: no rule matched (never returned by
	// Decide, which always ends at Ask).
	VerdictNone Verdict = iota
	// VerdictDeny — refused; a session grant cannot override it.
	VerdictDeny
	// VerdictGrant — the conversation's session grant covers this path.
	VerdictGrant
	// VerdictAccept — an accept entry covers this path; never prompts.
	VerdictAccept
	// VerdictProject — the conversation's own project dir (default scope).
	VerdictProject
	// VerdictAsk — nothing covers it; raise a consent ask.
	VerdictAsk
)

func (v Verdict) String() string {
	switch v {
	case VerdictDeny:
		return "deny"
	case VerdictGrant:
		return "session_grant"
	case VerdictAccept:
		return "accept"
	case VerdictProject:
		return "project"
	case VerdictAsk:
		return "ask"
	default:
		return "none"
	}
}

// Proceed reports whether this verdict means "proceed silently".
func (v Verdict) Proceed() bool {
	switch v {
	case VerdictDeny, VerdictAsk, VerdictNone:
		return false
	default:
		return true
	}
}

// Policy is the parsed file: two lists of doublestar patterns. Both empty
// means "no policy" — nothing denied, nothing pre-accepted — never "deny
// everything".
type Policy struct {
	Deny   []string `yaml:"deny"`
	Accept []string `yaml:"accept"`
}

// Empty reports whether the policy holds no entries at all.
func (p Policy) Empty() bool { return len(p.Deny) == 0 && len(p.Accept) == 0 }

// Decision is one consult's answer, including the entry that decided it
// (so a refusal can NAME the entry — an unexplained refusal is unfixable).
type Decision struct {
	Verdict Verdict
	Entry   string
	List    List
}

// Inputs are the facts ABOVE the policy in the precedence chain that the
// caller owns. Deny is evaluated before either field is read, which is what
// makes "deny outranks a session grant" true by construction.
type Inputs struct {
	// SessionGranted: this conversation's in-memory grant covers the path.
	SessionGranted bool
	// ProjectDefault: the path is inside the conversation's own project
	// (AskFileScope.PreApprovedPath).
	ProjectDefault bool
}

// DefaultPath resolves the policy file: ORCHICON_PERMISSION_POLICY when
// set, else <ORCHICON_DATA_DIR>/permission-policy.yaml. The data-dir half
// is computed here from the same env var internal/config reads, so the two
// cannot drift and this package never imports config.
func DefaultPath() string {
	if v := strings.TrimSpace(os.Getenv(PolicyEnv)); v != "" {
		return v
	}
	return filepath.Join(dataDir(), PolicyFileName)
}

func dataDir() string {
	if v := strings.TrimSpace(os.Getenv(dataDirEnv)); v != "" {
		return v
	}
	return defaultDataDir
}

// ExplicitPath reports whether the operator set ORCHICON_PERMISSION_POLICY
// (which is what gates preset installation: an explicitly pointed-at
// nonexistent path is never created).
func ExplicitPath() bool { return strings.TrimSpace(os.Getenv(PolicyEnv)) != "" }

// Load reads and strictly parses the policy file.
//
// ABSENT FILE (and empty file) => the zero Policy and a nil error: "no
// policy" is a legitimate state, not a failure, and must never degrade to
// "deny everything".
//
// MALFORMED => an error naming the file and the parse error. A policy file
// that silently parses to nothing is worse than no file at all, because the
// operator believes exclusions are in force. Unknown keys are malformed
// too (KnownFields): an unrecognised key means the operator's intent
// changed shape, and guessing which half it meant is how a deny becomes an
// accept.
func Load(path string) (Policy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Policy{}, nil
		}
		return Policy{}, fmt.Errorf("permission policy: read %s: %w", path, err)
	}
	return Parse(path, b)
}

// Parse strictly parses policy bytes that were read from path (path is used
// for error messages only). Exported so a caller that already holds the
// bytes — tests, and the boot check that wants path + parse error — can use
// the same parser.
func Parse(path string, b []byte) (Policy, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return Policy{}, nil
	}
	var p Policy
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		if errors.Is(err, io.EOF) {
			return Policy{}, nil
		}
		return Policy{}, fmt.Errorf("permission policy %s: %w", path, err)
	}
	for _, half := range []struct {
		name    string
		entries []string
	}{{"deny", p.Deny}, {"accept", p.Accept}} {
		for _, e := range half.entries {
			if strings.TrimSpace(e) == "" {
				return Policy{}, fmt.Errorf("permission policy %s: empty %s entry", path, half.name)
			}
			if !doublestar.ValidatePattern(expandHome(e)) {
				return Policy{}, fmt.Errorf("permission policy %s: malformed %s entry %q", path, half.name, e)
			}
		}
	}
	return p, nil
}

// --- matching ---------------------------------------------------------

func home() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return os.Getenv("HOME")
}

// expandHome expands a leading ~ (the operator writes ~/.ssh/**, not the
// absolute home) and resolves $HOME for the ~-prefixed spellings a shell
// would have expanded before the shim saw them.
func expandHome(p string) string {
	s := strings.TrimSpace(p)
	switch {
	case s == "~":
		return home()
	case strings.HasPrefix(s, "~/"):
		return filepath.Join(home(), s[2:])
	case s == "$HOME" || s == "${HOME}":
		return home()
	case strings.HasPrefix(s, "$HOME/"):
		return filepath.Join(home(), s[len("$HOME/"):])
	case strings.HasPrefix(s, "${HOME}/"):
		return filepath.Join(home(), s[len("${HOME}/"):])
	}
	return s
}

// matchPattern reports whether target is covered by entry (D7): expand a
// leading ~, then doublestar PathMatch so `**` crosses directories. A
// pattern ending in `/**` also covers the path itself (`~/.ssh/**` matching
// `~/.ssh`) — doublestar does this already, but the explicit check keeps the
// intent readable and independent of the matcher's zero-segment rules.
func matchPattern(entry, target string) bool {
	pat := expandHome(entry)
	if pat == "" || target == "" {
		return false
	}
	if pat == target {
		return true
	}
	if strings.HasSuffix(pat, "/**") && strings.TrimSuffix(pat, "/**") == target {
		return true
	}
	if ok, err := doublestar.PathMatch(pat, target); err == nil && ok {
		return true
	}
	return false
}

// matchAny returns the first entry covering target (the literal text, so a
// refusal can name it) and whether one matched. Deny is consulted first by
// Decide; within a list, order is the operator's file order.
func matchAny(entries []string, target string) (string, bool) {
	// Both spellings are candidates: the literal target as the caller wrote
	// it (`~/.ssh/id_rsa` from a model or a TUI row) and its ~-expanded
	// form, so an entry written either way matches either way.
	cands := []string{target}
	if exp := expandHome(target); exp != target {
		cands = append(cands, exp)
	}
	for _, e := range entries {
		for _, c := range cands {
			if matchPattern(e, c) {
				return e, true
			}
		}
	}
	return "", false
}

// --- the store (reader + the plane's single writer) --------------------

// Store is the plane's single writer and the per-consult reader for one
// policy file. It holds no state beyond the path: every read hits the disk
// (D3), so a hand-edit and a UI change are the same change.
type Store struct{ Path string }

// NewStore builds a store for path.
func NewStore(path string) *Store { return &Store{Path: path} }

// Read is a fresh strict read; never cached.
func (s *Store) Read() (Policy, error) { return Load(s.Path) }

// Write replaces the file atomically (temp file + rename in the same
// directory), so a crash or a concurrent consult never observes a
// half-written policy.
func (s *Store) Write(p Policy) error { return WriteFile(s.Path, p) }

// Consult is the READ-side query used for UI listings: does any entry
// cover target? Deny wins over accept.
func (s *Store) Consult(target string) (Decision, error) {
	return s.Decide(target, Inputs{})
}

// Decide implements the ENTIRE precedence chain below the never-allow
// binary class for one path, in order:
//
//  1. deny match  — evaluated BEFORE in.SessionGranted is read at all
//  2. session grant
//  3. accept match
//  4. project default
//  5. otherwise   — ask
//
// This is the one copy of the rules. A caller that needs a "should I ask?"
// answer calls this and not a re-implementation.
func (s *Store) Decide(target string, in Inputs) (Decision, error) {
	p, err := s.Read()
	if err != nil {
		return Decision{}, err
	}
	// 1. DENY FIRST. The grant is not consulted above this line, which is
	// what makes "a session grant cannot override a deny entry" true by
	// construction rather than by remembering to check.
	if entry, ok := matchAny(p.Deny, target); ok {
		return Decision{Verdict: VerdictDeny, Entry: entry, List: ListDeny}, nil
	}
	// 2. Session grant (in-memory, per conversation).
	if in.SessionGranted {
		return Decision{Verdict: VerdictGrant}, nil
	}
	// 3. Accept list: never prompts.
	if entry, ok := matchAny(p.Accept, target); ok {
		return Decision{Verdict: VerdictAccept, Entry: entry, List: ListAccept}, nil
	}
	// 4. The conversation's own project is the default scope.
	if in.ProjectDefault {
		return Decision{Verdict: VerdictProject}, nil
	}
	// 5. Otherwise ask.
	return Decision{Verdict: VerdictAsk}, nil
}

// Refusal renders the refusal for a denied target. The denied entry's
// literal text is in the message: a refusal that does not say WHICH entry
// refused is unfixable by the operator.
func (s *Store) Refusal(target, entry string) error {
	return fmt.Errorf("permission policy: %s is denied by entry %q in %s — a session grant cannot override it", target, entry, s.Path)
}

// HostSuiteGuard is the shared accessor handed to the host tool suite
// (the file/shell choke point). It consults the SAME store, so the suite
// and the consent layer cannot disagree: a denied path is refused HERE, and
// a refusal is returned verbatim so the model relays the entry name.
//
// A read failure (a malformed file at runtime) is returned as an error —
// fail closed. It cannot happen after a successful boot, which is exactly
// why it must not silently proceed if it does.
func (s *Store) HostSuiteGuard() func(paths []string) error {
	return func(paths []string) error {
		for _, target := range paths {
			if strings.TrimSpace(target) == "" {
				continue
			}
			d, err := s.Decide(target, Inputs{})
			if err != nil {
				return err
			}
			if d.Verdict == VerdictDeny {
				return s.Refusal(target, d.Entry)
			}
		}
		return nil
	}
}
