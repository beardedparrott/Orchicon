package permpolicy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// PresetYAML is the operator-confirmed preset shipped by default.
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

// fileHeader is the comment block written at the top of the file (and of every rewrite).
const fileHeader = `# Orchicon permission policy — read on every gated decision; edits take effect immediately.
# deny  : always refused. A session grant CANNOT override an entry here.
# accept: never prompts. Precedence: never-allow binaries > deny > session grant > accept > ask.
# Patterns are doublestar globs; a leading ~ is the operator's home.
`

// WriteFile atomically writes p to path with the documented header block.
func WriteFile(path string, p Policy) error {
	body, err := yaml.Marshal(p)
	if err != nil {
		return fmt.Errorf("permission policy: marshal: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		// NOT "mkdir %s", which read as `permission policy: mkdir /var/lib/orchicon: mkdir
		// /var/lib/orchicon: permission denied` — twice.
		return fmt.Errorf("permission policy: cannot create %s: %w", dir, err)
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

// EnsureDefault installs the preset at path, but ONLY when the operator did not explicitly point at
// a file (ORCHICON_PERMISSION_POLICY unset) and no file exists yet.
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

// Boot is the serve-time check: install the preset when that is the rule, then strictly load the
// file so a malformed policy fails LOUDLY at boot with the path and the parse error.
func Boot(path string) error {
	if err := EnsureDefault(path); err != nil {
		return err
	}
	_, err := Load(path)
	return err
}
