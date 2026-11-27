package adapter_test

// antigravity_notinstalled_test.go — the INSTALL sentence, and its separation from the SIGN-IN one.
//
// These are two different operator problems with two different fixes, and collapsing them (which the
// adapter did until this change) produced unfollowable advice: "run `agy` and sign in" on a host that
// has no `agy`. The pinning here is deliberately BYTE-EXACT on the install sentence, because it is the
// empty state's entire visible content and the GUI mirrors it verbatim from Go (see
// frontend/src/lib/adapter-errors.ts, whose drift test reads this file).

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

func TestAntigravityNotInstalledMessageNamesTheDownload(t *testing.T) {
	msg := adapter.AntigravityNotInstalledMessage
	// The exact bytes the GUI and the TUI both render. Anything downstream that paraphrases this
	// sentence produces a fourth wording of one fact.
	const want = "Antigravity CLI (`agy`) not found — install from https://antigravity.google/download"
	if msg != want {
		t.Fatalf("AntigravityNotInstalledMessage = %q\nwant %q", msg, want)
	}
	for _, part := range []string{"`agy`", "https://antigravity.google/download"} {
		if !strings.Contains(msg, part) {
			t.Errorf("the install sentence does not carry %q", part)
		}
	}
}

// TestAntigravityInstallAndAuthMessagesAreDistinct is the whole point of the split: each sentence must
// name ITS fix and not the other's, so a client that renders the wrong one is visibly wrong.
func TestAntigravityInstallAndAuthMessagesAreDistinct(t *testing.T) {
	install := adapter.AntigravityNotInstalledMessage
	auth := adapter.AntigravityAuthRequiredMessage
	if install == auth {
		t.Fatal("the install and sign-in messages are identical — an absent CLI and a signed-out one " +
			"have different fixes, and one sentence cannot name both")
	}
	if strings.Contains(install, "sign in") || strings.Contains(install, "not signed in") {
		t.Errorf("the install sentence %q tells the operator to sign in; there is nothing to sign in to", install)
	}
	if !strings.Contains(auth, "sign in") {
		t.Errorf("the auth sentence %q does not name its fix (sign in)", auth)
	}
	if strings.Contains(auth, "antigravity.google/download") {
		t.Errorf("the auth sentence %q carries the download URL; agy IS installed in that case", auth)
	}
}

// TestAntigravityAuthFailureDoesNotMatchAGenericPathError pins that the classification stays a claim
// about the CREDENTIAL: the resolver's own "not on PATH" text must not be swept into the auth bucket by
// the shared signal list. (It is classified by the resolver's typed failure, not by string matching.)
func TestAntigravityAuthFailureDoesNotMatchAGenericPathError(t *testing.T) {
	for _, in := range []string{
		"antigravity: the `agy` CLI is not on PATH",
		"antigravity: the `agy` CLI could not be resolved on this host",
	} {
		if msg, ok := adapter.AntigravityAuthFailure(in); ok {
			t.Errorf("AntigravityAuthFailure(%q) = (%q, true); a missing binary is the INSTALL precondition "+
				"and must not be reported as a sign-in problem", in, msg)
		}
	}
}
