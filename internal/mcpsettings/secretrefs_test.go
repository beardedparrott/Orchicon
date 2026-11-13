package mcpsettings

// secretrefs_test.go — the reference grammar itself, with NO database.
//
// The integration tests in embedded_secret_refs_test.go prove the four surfaces (expansion, the two
// read-time views, the write gate) against a real store; these pin the grammar underneath them so it is
// exercised by the DEFAULT suite, which skips every DB-backed test when ORCHICON_TEST_DSN is unset.

import (
	"errors"
	"strings"
	"testing"
)

// TestSecretRefNamesScansEverywhere is the grammar: a reference is `${`+identifier+`}`, wherever it sits
// in the value and however many there are.
func TestSecretRefNamesScansEverywhere(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		// The whole-value form the platform has always supported.
		{"${TOKEN}", []string{"TOKEN"}},
		{"${MCP_GITHUB_TOKEN}", []string{"MCP_GITHUB_TOKEN"}},
		// THE BUG: the canonical remote-server header. It was not a reference at all.
		{"Bearer ${PIXELLAB_API_KEY}", []string{"PIXELLAB_API_KEY"}},
		{"postgres://app:${PG_PASSWORD}@db:5432/app", []string{"PG_PASSWORD"}},
		{"${A}:${B}", []string{"A", "B"}},
		{"${A} and ${A}", []string{"A"}}, // deduped, in order of appearance
		// Not references: no reference at all, unterminated, or a name that is not an identifier.
		{"", nil},
		{"no-reference-here", nil},
		{"${UNCLOSED", nil},
		{"${}", nil},
		{"${5}", nil},                         // a digit cannot START a name
		{"cost=${5}", nil},                    // …so a brace expression in ordinary text is left alone
		{"${A B}", nil},                       // a space is not part of a name
		{"$TOKEN", nil},                       // no braces
		{"${A", nil},                          // truncated
		{"${-TOKEN}", nil},                    // not an identifier
		{"${_PRIVATE}", []string{"_PRIVATE"}}, // an underscore may START a name
	}

	for _, c := range cases {
		got := secretRefNames(c.in)
		if len(got) != len(c.want) {
			t.Errorf("secretRefNames(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("secretRefNames(%q) = %v, want %v", c.in, got, c.want)
				break
			}
		}
	}
}

// TestExpandSecretRefsLeavesSurroundingText pins the replacement itself: the bytes around the reference
// survive, which is the whole difference from the old whole-value rule.
func TestExpandSecretRefsLeavesSurroundingText(t *testing.T) {
	lookup := func(name string) (string, error) {
		switch name {
		case "TOKEN":
			return "s3cret", nil
		case "OTHER":
			return "other", nil
		}
		return "", errors.New("not stored")
	}
	out, changed, err := expandSecretRefs("Bearer ${TOKEN}", lookup)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if !changed {
		t.Fatal("changed = false for a value carrying a reference")
	}
	if want := "Bearer s3cret"; out != want {
		t.Fatalf("got %q, want %q", out, want)
	}

	if out, changed, err = expandSecretRefs("${TOKEN}:${OTHER}", lookup); err != nil || out != "s3cret:other" || !changed {
		t.Fatalf("multi-ref: got %q changed=%v err=%v", out, changed, err)
	}
	// A value with no reference is returned UNCHANGED and is not reported as changed, so a caller can
	// skip work — and ordinary text is never rewritten.
	if out, changed, err = expandSecretRefs("Bearer literal", lookup); err != nil || out != "Bearer literal" || changed {
		t.Fatalf("literal: got %q changed=%v err=%v", out, changed, err)
	}
	if out, _, err = expandSecretRefs("cost=${5}", lookup); err != nil || out != "cost=${5}" {
		t.Fatalf("brace expression was rewritten: %q err=%v", out, err)
	}
	// An UNRESOLVABLE reference is an ERROR, never a pass-through: handing the literal ${NAME} to a
	// server is what produced a 401 that looked like a bad credential.
	if _, _, err = expandSecretRefs("Bearer ${NEVER_STORED}", lookup); err == nil {
		t.Fatal("an unresolvable reference expanded without an error")
	} else if !strings.Contains(err.Error(), "NEVER_STORED") {
		t.Fatalf("the error does not name the secret: %v", err)
	}
}

// TestWholeSecretRefNameIsTheNarrowQuestion keeps the write-back path's question honest: "is this value
// PURELY a reference" is a different question from "does it carry one", and the old predicate collapsed
// the two.
func TestWholeSecretRefNameIsTheNarrowQuestion(t *testing.T) {
	if name, ok := wholeSecretRefName("${TOKEN}"); !ok || name != "TOKEN" {
		t.Fatalf("wholeSecretRefName(${TOKEN}) = %q,%v", name, ok)
	}
	for _, v := range []string{"Bearer ${TOKEN}", "${A}${B}", "TOKEN", "${}"} {
		if _, ok := wholeSecretRefName(v); ok {
			t.Errorf("wholeSecretRefName(%q) reported a whole-value reference", v)
		}
	}
}
