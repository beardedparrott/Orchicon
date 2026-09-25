package permpolicy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// PresetYAML is the operator-confirmed preset shipped by default.
//
// Credential stores are the reason for the deny list: READS never ask, so
// without these the agent could read a private key or a stored token
// without the operator noticing a prompt at all. Nothing is pre-accepted
// beyond project directories, which are already the default scope.
//
// The header is written into the file too (fileHeader) so a hand-edit
// finds the rules documented where it is editing.
const PresetYAML = `deny:
  - ~/.ssh/**
  - ~/.gnupg/**
  - ~/.aws/**
  - ~/.config/gh/**
  - ~/.git-credentials
  - ~/.netrc
  - ~/.docker/config.json
accept: []
`

// fileHeader is the comment block written at the top of the file (and of
// every rewrite). It documents the precedence in the file the operator
// edits — and it is the reason a UI write can regenerate the whole file
// without losing the rules' explanation.
const fileHeader = `# Orchicon permission policy — read on every gated decision; edits take effect immediately.
# deny  : always refused. A session grant CANNOT override an entry here.
# accept: never prompts. Precedence: never-allow binaries > deny > session grant > accept > project dir > ask.
# Patterns are doublestar globs; a leading ~ is the operator's home.
`

// WriteFile atomically writes p to path with the documented header block.
//
// ATOMIC: temp file + rename in the same directory, so a concurrent
// per-consult read sees either the old file or the new one, never a
// half-written policy. The mode is 0600 — the file lists credential stores.
//
// NOTE ON HAND COMMENTS: a write regenerates the file from the parsed
// struct and re-emits the top header block. Entry-level comments a human
// added inline are NOT preserved (the top block documents the file). This
// is a deliberate trade: surgical YAML comment preservation is a large
// amount of code for a file whose rules fit on one screen.
func WriteFile(path string, p Policy) error {
	body, err := yaml.Marshal(p)
	if err != nil {
		return fmt.Errorf("permission policy: marshal: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("permission policy: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".permission-policy-*.tmp")
	if err != nil {
		return fmt.Errorf("permission policy: temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append([]byte(fileHeader), body...)); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("permission policy: write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("permission policy: chmod %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("permission policy: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("permission policy: rename to %s: %w", path, err)
	}
	return nil
}

// EnsureDefault installs the preset at path, but ONLY when the operator did
// not explicitly point at a file (ORCHICON_PERMISSION_POLICY unset) and no
// file exists yet.
//
// Both halves are load-bearing: "ship the preset by default" and
// "an absent file means no policy" must hold at once, so an explicitly
// pointed-at nonexistent path is never created — otherwise the acceptance
// test for "absent file ⇒ no policy" could never be set up.
func EnsureDefault(path string) error {
	if ExplicitPath() {
		return nil
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("permission policy: stat %s: %w", path, err)
	}
	if err := WriteFile(path, MustParsePreset()); err != nil {
		return err
	}
	return nil
}

// MustParsePreset parses the embedded preset (it is a compile-time constant;
// a parse failure is a build-time bug, not an operator error).
func MustParsePreset() Policy {
	p, err := Parse("<preset>", []byte(PresetYAML))
	if err != nil {
		panic("permpolicy: embedded preset does not parse: " + err.Error())
	}
	return p
}

// Boot is the serve-time check: install the preset when that is the rule,
// then strictly load the file so a malformed policy fails LOUDLY at boot
// with the path and the parse error.
//
// Installing before loading is deliberate: on a fresh instance the preset
// must be in force immediately, and a preset that failed to parse must stop
// the boot rather than leave the operator believing credential stores are
// excluded.
func Boot(path string) error {
	if err := EnsureDefault(path); err != nil {
		return err
	}
	_, err := Load(path)
	return err
}
