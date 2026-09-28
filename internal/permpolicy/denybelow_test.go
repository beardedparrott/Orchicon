package permpolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AC: the consent card must be able to say which deny entries a session grant
// cannot override. That is exactly the entries at or below the granted
// directory.

// TestDenyBelow_EqualAndUnderTheDirectory covers the two reachable shapes: the
// entry IS the directory, and the entry lives under it.
func TestDenyBelow_EqualAndUnderTheDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	s := writePolicy(t, `deny:
  - `+home+`/secrets
  - `+home+`/secrets/keys/**
  - `+home+`/other/**
accept: []
`)
	got, err := s.DenyBelow(filepath.Join(home, "secrets"))
	if err != nil {
		t.Fatalf("deny below: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("entries = %v, want the equal entry and the one below it", got)
	}
	for _, want := range []string{home + "/secrets", home + "/secrets/keys/**"} {
		found := false
		for _, g := range got {
			if g == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("entries = %v, missing %q", got, want)
		}
	}
}

// TestDenyBelow_HomeSpelledEntryMatchesAbsoluteDirectory: the operator writes
// `~/.ssh/**`, the ask's key is the absolute directory.
func TestDenyBelow_HomeSpelledEntryMatchesAbsoluteDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	s := writePolicy(t, `deny:
  - ~/.ssh/**
accept: []
`)
	got, err := s.DenyBelow(filepath.Join(home, ".ssh"))
	if err != nil {
		t.Fatalf("deny below: %v", err)
	}
	if len(got) != 1 || got[0] != "~/.ssh/**" {
		t.Fatalf("entries = %v, want [~/.ssh/**] in the operator's own spelling", got)
	}
}

// TestDenyBelow_UnrelatedEntryAndGlobAboveAreNotReported: only entries inside
// the directory count. An unrelated path and a directory-independent glob do
// not.
func TestDenyBelow_UnrelatedEntryAndGlobAboveAreNotReported(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	s := writePolicy(t, `deny:
  - `+home+`/elsewhere/**
  - "**/.env"
  - `+home+`/accept-listed/**
accept: []
`)
	got, err := s.DenyBelow(filepath.Join(home, "project"))
	if err != nil {
		t.Fatalf("deny below: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("entries = %v, want none", got)
	}
}

// TestDenyBelow_MalformedFileIsAnErrorNotASilentSuccess: a broken policy must
// never look like "no deny entries below".
func TestDenyBelow_MalformedFileIsAnErrorNotASilentSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "permission-policy.yaml")
	if err := os.WriteFile(path, []byte("deny: [unterminated\n"), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	if _, err := NewStore(path).DenyBelow("/tmp/x"); err == nil {
		t.Fatal("want an error for a malformed policy file, got nil")
	}
}

// TestDenyBelow_EmptyDirectoryIsNoOp keeps an unresolved ask (no directory)
// from reporting the whole deny list.
func TestDenyBelow_EmptyDirectoryIsNoOp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := writePolicy(t, `deny:
  - `+home+`/secrets/**
accept: []
`)
	got, err := s.DenyBelow("")
	if err != nil {
		t.Fatalf("deny below: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("entries = %v, want none for an empty directory", got)
	}
	// A trailing slash must not defeat the prefix test.
	got, err = s.DenyBelow(filepath.Join(home, "secrets") + string(filepath.Separator))
	if err != nil {
		t.Fatalf("deny below: %v", err)
	}
	if len(got) != 1 || !strings.HasSuffix(got[0], "secrets/**") {
		t.Fatalf("entries = %v, want the secrets entry", got)
	}
}
