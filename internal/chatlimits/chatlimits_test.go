package chatlimits

// chatlimits_test.go — ONE BOUND, and the rest of the product reads it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
)

// TestTheBoundIsWellAboveNormalUse pins the intent rather than the digit: the limit exists, and it is far
// enough above ordinary input that reaching it is deliberate. 10,000 was not.
func TestTheBoundIsWellAboveNormalUse(t *testing.T) {
	if MaxMessageChars < 100_000 {
		t.Fatalf("MaxMessageChars = %d, and the point of the change was to stop a long prompt or a pasted "+
			"block meeting the limit mid-paste", MaxMessageChars)
	}
	if WarnAt >= MaxMessageChars {
		t.Fatalf("WarnAt = %d is not below the bound %d, so the counter could only appear once a send is "+
			"already refused", WarnAt, MaxMessageChars)
	}
	if WarnAt != MaxMessageChars*4/5 {
		t.Fatalf("WarnAt = %d, want 4/5 of the bound (%d) — a client showing the counter from the first "+
			"character is the noise the threshold exists to avoid", WarnAt, MaxMessageChars*4/5)
	}
}

// TestAMessageAtTheBoundFitsAndOneOverDoesNot is the boundary in both directions.
func TestAMessageAtTheBoundFitsAndOneOverDoesNot(t *testing.T) {
	if err := TooLong(strings.Repeat("a", MaxMessageChars)); err != nil {
		t.Fatalf("a message exactly at the bound was refused: %v", err)
	}
	err := TooLong(strings.Repeat("a", MaxMessageChars+1))
	if err == nil {
		t.Fatal("a message over the bound was accepted")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument — a client shows this to the operator", connect.CodeOf(err))
	}
}

// TestTheErrorNamesTheLengthAndTheBound: "max N characters" left the operator to count.
func TestTheErrorNamesTheLengthAndTheBound(t *testing.T) {
	msg := strings.Repeat("a", MaxMessageChars+37)
	err := TooLong(msg)
	if err == nil {
		t.Fatal("no error for an over-long message")
	}
	text := err.Error()
	if !strings.Contains(text, "100037") {
		t.Errorf("the error does not say how long the message is, so the operator cannot tell how much to "+
			"trim: %q", text)
	}
	if !strings.Contains(text, "100000") {
		t.Errorf("the error does not name the bound: %q", text)
	}
}

// TestTheBoundIsCountedInRunesNotBytes: a message of multi-byte characters must not be refused at a third of
// the stated length.
func TestTheBoundIsCountedInRunesNotBytes(t *testing.T) {
	// Three bytes each, so a byte-based count would refuse this at MaxMessageChars/3.
	if err := TooLong(strings.Repeat("日", MaxMessageChars)); err != nil {
		t.Fatalf("a message at the bound in multi-byte runes was refused: %v", err)
	}
}

// TestNoEnforcementSiteCarriesItsOwnNumber is the drift guard, and it is the reason this package
// exists.
func TestNoEnforcementSiteCarriesItsOwnNumber(t *testing.T) {
	root := filepath.Join("..") // the sibling packages under internal/
	for _, pkg := range []string{"askorchicon", "execution"} {
		entries, err := os.ReadDir(filepath.Join(root, pkg))
		if err != nil {
			t.Fatalf("read %s: %v", pkg, err)
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			body, err := os.ReadFile(filepath.Join(root, pkg, name))
			if err != nil {
				t.Fatalf("read %s/%s: %v", pkg, name, err)
			}
			if strings.Contains(string(body), "message too long (max") {
				t.Errorf("%s/%s builds its own \"message too long\" error instead of asking chatlimits.TooLong — "+
					"the wording and the number would then disagree with every client", pkg, name)
			}
		}
	}
}
