package project

// nul_boundary_test.go — "invalid byte sequence for encoding UTF8: 0x00" must be unreachable, and when a
// client does send one it must be told WHICH FIELD.
//
// REPORTED (Windows TUI, creating a project):
//
//	internal: db: create project: ERROR: invalid byte sequence for encoding "UTF8": 0x00 (SQLSTATE 22021)
//
// The byte is invisible, so the operator saw a name that read "Temp" and a server that refused it. A text
// column cannot hold U+0000 at all — there is no version of this that could have worked — so the only
// question was whether the failure said anything useful. It did not: SQLSTATE 22021 names neither the field
// nor the character.

import (
	"strings"
	"testing"
)

func TestNameWithANulIsRefusedByName(t *testing.T) {
	_, err := validateName("Temp\x00")
	if err == nil {
		t.Fatal("validateName accepted a NUL. It will pass straight to Postgres and come back as SQLSTATE " +
			"22021 — a byte offset for a character the operator cannot see")
	}
	if !strings.Contains(err.Error(), "control characters") {
		t.Errorf("the refusal does not name the problem: %v — it must say the value contains a control "+
			"character, because nothing else about the request is wrong", err)
	}
	// The character and its position are the two facts that make it findable in a string that looks fine.
	if !strings.Contains(err.Error(), "U+0000") {
		t.Errorf("the refusal does not name the character: %v", err)
	}
}

func TestNoControlCharacterEverSurvivesValidation(t *testing.T) {
	// THE PROPERTY, NOT ONE ROUTE TO IT. Every C0 control and DEL is equally unstorable in a text column and
	// equally invisible, so the invariant is that NONE of them is in the value that gets inserted. Two
	// routes satisfy it and both are correct: the value is refused with a message, or the character was
	// whitespace and got trimmed. What must never happen is a control rune coming out the other side — that
	// is the 22021.
	for _, r := range []rune{0x00, 0x01, 0x07, 0x0b, 0x1b, 0x7f} {
		got, err := validateName("Temp" + string(r))
		if err != nil {
			continue // refused, with the message TestNameWithANulIsRefusedByName pins
		}
		for _, out := range got {
			if out < 0x20 || out == 0x7f {
				t.Errorf("validateName(%q) returned %q, which still contains %#U — this reaches Postgres as "+
					"SQLSTATE 22021", "Temp"+string(r), got, out)
			}
		}
	}
	// And the reported case specifically: a NUL is NOT whitespace, so trimming cannot save it — it must be
	// refused outright, or the guard above would pass while the bug shipped.
	if _, err := validateName("Temp\x00"); err == nil {
		t.Error("a NUL was not refused; trimming cannot remove it, so it would reach the database")
	}
}

func TestAnOrdinaryNameStillPasses(t *testing.T) {
	// VACUITY: a rule that rejects everything would satisfy the tests above.
	for _, ok := range []string{"Temp", "Orchicon", "a b", "proj-1", "Ünïcode"} {
		if _, err := validateName(ok); err != nil {
			t.Errorf("validateName(%q) = %v, want accepted", ok, err)
		}
	}
}
