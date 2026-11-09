package mcpsettings

// IT VALIDATES ONLY WHAT THE EDIT CHANGES, which is deliberate rather than lenient.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/beardedparrott/orchicon/internal/db"
)

// invalidContext re-says an invalid-argument error WITH context in front of it — usually the spec
// the fault is about — without stacking a second "invalid argument:" prefix (invalidf would).
func invalidContext(prefix string, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if errors.Is(err, errInvalidArgument) {
		msg = strings.TrimPrefix(msg, errInvalidArgument.Error()+": ")
	}
	return invalidf("%s: %s", prefix, msg)
}

// ValidateInlinePermissions validates the INLINE MCP specs that an EDIT introduces or changes.
func ValidateInlinePermissions(ctx context.Context, tx pgx.Tx, tenantID string, previous, incoming []byte) error {
	stored := make(map[string]string, 8)
	for _, s := range db.MCPServersFromPermissions(previous) {
		stored[s.ID] = inlineSpecFingerprint(s)
	}
	for _, s := range db.MCPServersFromPermissions(incoming) {
		fp := inlineSpecFingerprint(s)
		if prev, ok := stored[s.ID]; ok && prev == fp {
			continue // unchanged: stored data is left exactly as it is
		}
		if err := validateInlineSpec(ctx, tx, tenantID, s); err != nil {
			return err
		}
	}
	return nil
}

// inlineSpecFingerprint canonicalizes one spec for the change comparison. It marshals the parsed spec
// (not the raw JSON), so a pure reformat — whitespace, key order — is not mistaken for a change.
func inlineSpecFingerprint(s db.InlineMCPServer) string {
	b, err := json.Marshal(s)
	if err != nil {
		// Unreachable for this shape; a spec that cannot be fingerprinted is validated rather than
		// silently skipped, which is what the "" (never equal to a real fingerprint) buys.
		return ""
	}
	return string(b)
}

// inlineSpecIsHTTP reports whether an inline spec speaks streamable HTTP.
func inlineSpecIsHTTP(s db.InlineMCPServer) bool {
	switch s.Type {
	case "http", "streamable-http":
		return true
	case "stdio":
		return false
	}
	return s.URL != ""
}

// validateInlineSpec applies the owned-row definition rules to one inline spec: the argv shape and
// bounds, the env/header key grammar and bounds.
func validateInlineSpec(ctx context.Context, tx pgx.Tx, tenantID string, s db.InlineMCPServer) error {
	id := s.ID
	if len(id) > maxNameLen {
		// The id is the server's NAMESPACE (`mcp__<id>__<tool>`, the shape the ask-mode classifier and the
		// tool router read), not a display label — so it is bounded like an owned row's name.
		return invalidf("inline server id too long (max %d) — the id is the server's tool namespace (mcp__<id>__<tool>)", maxNameLen)
	}
	if len(s.Command) == 0 {
		if inlineSpecIsHTTP(s) {
			if err := validateURL(s.URL); err != nil {
				return invalidContext(fmt.Sprintf("inline server %q", id), err)
			}
		} else {
			return invalidf("inline server %q: a stdio server needs a command", id)
		}
	} else {
		// argv[0] IS the command and the rest are its arguments — the same two bounds the owned-row path
		// applies to command + args separately (maxCommandLen, then maxArgs × maxArgLen).
		if len(s.Command[0]) > maxCommandLen {
			return invalidf("inline server %q: command too long (max %d)", id, maxCommandLen)
		}
		if err := validateArgs(s.Command[1:]); err != nil {
			return invalidContext(fmt.Sprintf("inline server %q", id), err)
		}
		if inlineSpecIsHTTP(s) {
			if err := validateURL(s.URL); err != nil {
				return invalidContext(fmt.Sprintf("inline server %q", id), err)
			}
		}
	}
	// BOTH MAPS ARE CHECKED, whichever the transport reads: a spec carrying headers it will never send
	// is odd but harmless, while a key the transport cannot hand to a child process is not.
	if err := validateMapKeys(s.Env, "env", 64); err != nil {
		return invalidContext(fmt.Sprintf("inline server %q", id), err)
	}
	if err := validateMapKeys(s.Headers, "headers", 32); err != nil {
		return invalidContext(fmt.Sprintf("inline server %q", id), err)
	}
	// EVERY REFERENCE MUST RESOLVE.
	if err := validateSecretRefs(ctx, tx, tenantID, s.Env, s.Headers); err != nil {
		return invalidContext(fmt.Sprintf("inline server %q", id), err)
	}
	return nil
}
