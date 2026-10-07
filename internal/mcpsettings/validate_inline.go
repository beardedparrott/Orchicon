package mcpsettings

// validate_inline.go — the INLINE half of definition validation: the MCP specs a WORKER VERSION carries
// in its own permissions (`permissions.mcp_servers`), which have no row and no owner column.
//
// WHAT WAS MISSING, AND WHY IT MATTERED. A definition with a ROW is validated on every write: the name,
// the transport, the argv, the env/header KEY GRAMMAR, and every ${SECRET_NAME} it references must
// exist in the tenant secrets store (validateCreate → validateArgs/validateMapKeys/validateURL/
// validateSecretRefs). An INLINE spec had NONE of it — permissions is opaque JSON to the worker service
// (it was not even checked for being JSON on the two version-update paths). So a spec could name an env
// key no environment can carry, and the MCP stdio transport hands those straight to a child process as
// `k+"="+v` (mcpclient/transport_stdio_unix.go) — a key with a space or an "=" corrupts the child's
// environment. And a spec could reference a secret that does not exist, which surfaced only when the
// worker RAN, per server (ResolveSecretRefs, at session time) — long after the save that could have
// said so.
//
// IT VALIDATES ONLY WHAT THE EDIT CHANGES, which is deliberate rather than lenient. Every client sends
// the permissions blob WHOLE (the scope editors own the mcp_servers key of the blob, never the whole
// value), so judging the incoming set in full would refuse ANY edit of a version that happens to carry
// a stale spec — a reference to a secret deleted since, say. That is precisely the "brick the save"
// failure validateModelRefForUpdate documents for model_refs, and the same answer applies: a spec whose
// canonical JSON is IDENTICAL to the stored one is left alone (stored data keeps working), and
// everything new or changed is judged.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/beardedparrott/orchicon/internal/db"
)

// invalidContext re-says an invalid-argument error WITH context in front of it — usually the spec the
// fault is about — without stacking a second "invalid argument:" prefix (invalidf would) and without
// losing the sentinel the handlers map to CodeInvalidArgument.
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
//
// previous is the permissions blob the version (or, for a new version, the version it was copied from)
// already held — the base the diff is taken against; incoming is the blob this write will store. A spec
// whose canonical JSON matches the stored one is SKIPPED (see the file comment), so this never turns
// stored data into a save-time failure; everything else must satisfy the owned-row rules.
//
// Errors are the package's invalid-argument kind (invalidf), so a handler maps them to
// CodeInvalidArgument without sniffing strings.
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

// inlineSpecIsHTTP reports whether an inline spec speaks streamable HTTP. It mirrors
// mcpsettings.specFromInline's own inference (type first, url as the fallback) so validation and
// resolution agree about which half of the spec is in play; an unrecognized/empty type is NOT refused —
// the resolver infers it, and refusing would retroactively invalidate stored specs for a distinction
// the system does not make.
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
// bounds, the env/header key grammar and bounds, the transport's own requirement (a command for stdio,
// an absolute http(s) URL for HTTP), and the existence of every ${SECRET_NAME} it references.
func validateInlineSpec(ctx context.Context, tx pgx.Tx, tenantID string, s db.InlineMCPServer) error {
	id := s.ID
	if len(id) > maxNameLen {
		// The id is the server's NAMESPACE (`mcp__<id>__<tool>`, the shape the ask-mode classifier and
		// the tool router read), not a display label — so it is bounded like an owned row's name.
		//
		// THE EMPTY-ID CASE IS NOT CHECKED HERE because it cannot arrive: db.MCPServersFromPermissions
		// drops an entry with no id (its documented degradation — a shape it cannot resolve is ignored
		// rather than failing a session), so a spec that reaches this function always has one.
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
	// is odd but harmless, while a key the transport cannot hand to a child process is not — and the
	// resolver passes both maps through (specFromInline).
	if err := validateMapKeys(s.Env, "env", 64); err != nil {
		return invalidContext(fmt.Sprintf("inline server %q", id), err)
	}
	if err := validateMapKeys(s.Headers, "headers", 32); err != nil {
		return invalidContext(fmt.Sprintf("inline server %q", id), err)
	}
	// EVERY REFERENCE MUST RESOLVE. This is the rule an owned row has enforced at create/update since
	// the secrets store existed; refusing it here is what stops a version being saved with a credential
	// that can only fail when the worker runs.
	if err := validateSecretRefs(ctx, tx, tenantID, s.Env, s.Headers); err != nil {
		return invalidContext(fmt.Sprintf("inline server %q", id), err)
	}
	return nil
}
