package mcpsettings

// secretrefs.go — THE secret-reference grammar, in one place.
//
// A definition's env/header value may carry `${SECRET_NAME}` REFERENCES, which are expanded to the
// stored plaintext at session time and never stored inline. There is exactly ONE grammar here and
// every consumer reads it: the resolver's expansion (ResolveSecretRefs), the read-time views
// (RequiredSecretsFor, anySecretStored), the write-time gate (validateSecretRefs) and the write-back
// paths.
//
// WHY THIS FILE EXISTS AT ALL. It used to be a single predicate, secretRefName, requiring the
// reference to be the ENTIRE value:
//
//	"${TOKEN}"                → a reference
//	"Bearer ${TOKEN}"         → NOT a reference: transmitted verbatim, has_secret_stored false
//
// `Authorization: Bearer ${TOKEN}` is the canonical shape in essentially every remote MCP server's
// documentation, so the documented configuration was the one that could not work — and it failed as
// a 401 from the server rather than as "your secret was not found". Five callers shared that one
// predicate and therefore all five were wrong in the same way, which is the other half of why the
// fix belongs here: one scanner, so the readers and the expander cannot drift apart again.
//
// THE GRAMMAR. `${` … `}` with a non-empty identifier-shaped name ([A-Za-z_][A-Za-z0-9_]*), anywhere
// in the value, any number of times. The identifier rule is deliberately not "everything up to the
// next }": a value that legitimately contains a brace expression (`${5}`, `{}`) must not be mistaken
// for a credential reference and refused as "secret 5 is not stored".
//
// ONE CONSEQUENCE, stated because it is the deliberate behaviour: a value carrying a `${NAME}` that
// is not a stored tenant secret is REFUSED when the definition is saved (validateSecretRefs), and
// fails the session at resolve time if it predates that gate. That is the existing rule for the
// whole-value form, now applied uniformly — so a shell-style `${HOME}` inside an env value has to be
// spelled literally rather than passed through for a child to expand.

import (
	"fmt"
	"strings"
)

// secretRefNameRE-ish helpers: identifiers only, so braces in ordinary text are not references.
func isSecretRefNameByte(b byte, first bool) bool {
	if b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') {
		return true
	}
	if !first && b >= '0' && b <= '9' {
		return true
	}
	return false
}

// scanSecretRefs walks v and calls visit for EVERY `${NAME}` reference it carries, with the name and
// the byte offsets of the whole `${…}` span. Returning false from visit stops the scan. A `${` that
// does not close on an identifier-shaped name is ordinary text and is skipped.
func scanSecretRefs(v string, visit func(name string, start, end int) bool) {
	for i := 0; i < len(v); i++ {
		if v[i] != '$' || i+2 >= len(v) || v[i+1] != '{' {
			continue
		}
		j := i + 2
		if !isSecretRefNameByte(v[j], true) {
			continue
		}
		j++
		for j < len(v) && isSecretRefNameByte(v[j], false) {
			j++
		}
		if j >= len(v) || v[j] != '}' {
			continue
		}
		if !visit(v[i+2:j], i, j+1) {
			return
		}
		i = j
	}
}

// secretRefNames returns every secret name v references, in order of appearance, deduped.
func secretRefNames(v string) []string {
	var out []string
	seen := map[string]bool{}
	scanSecretRefs(v, func(name string, _, _ int) bool {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
		return true
	})
	return out
}

// hasSecretRef reports whether v carries at least one `${NAME}` reference.
func hasSecretRef(v string) bool {
	found := false
	scanSecretRefs(v, func(string, int, int) bool {
		found = true
		return false
	})
	return found
}

// expandSecretRefs replaces EVERY `${NAME}` reference in v with its resolved value, leaving the
// surrounding text intact: "Bearer ${TOKEN}" becomes "Bearer <plaintext>". It reports whether
// anything was replaced so a caller can skip work for a value that carries no reference.
//
// A reference whose name cannot be resolved is an ERROR rather than a pass-through: handing the
// literal "${NAME}" to a server produces a 401 from the far end ("Invalid API token"), which reads as
// a bad credential instead of a missing one.
func expandSecretRefs(v string, lookup func(string) (string, error)) (string, bool, error) {
	if !hasSecretRef(v) {
		return v, false, nil
	}
	var b strings.Builder
	b.Grow(len(v))
	last, changed := 0, false
	var expandErr error
	scanSecretRefs(v, func(name string, start, end int) bool {
		val, err := lookup(name)
		if err != nil {
			expandErr = fmt.Errorf("secret %q: %w", name, err)
			return false
		}
		b.WriteString(v[last:start])
		b.WriteString(val)
		last = end
		changed = true
		return true
	})
	if expandErr != nil {
		return "", false, expandErr
	}
	b.WriteString(v[last:])
	return b.String(), changed, nil
}

// wholeSecretRefName reports whether v is EXACTLY one reference (`"${NAME}"`, nothing else) and
// returns that name. It is the narrow question the write-back paths ask — "is this value purely a
// reference, so it can be pointed somewhere else" — and it is NOT the contract for reading or
// expanding: use scanSecretRefs/expandSecretRefs for that.
func wholeSecretRefName(v string) (string, bool) {
	name, ok := "", false
	scanSecretRefs(v, func(n string, start, end int) bool {
		if start != 0 || end != len(v) {
			return false
		}
		name, ok = n, true
		return false
	})
	return name, ok
}
