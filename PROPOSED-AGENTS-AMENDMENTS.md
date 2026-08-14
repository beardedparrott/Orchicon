# Proposed AGENTS.md Amendments — License note + password-standard rewrite

**Status:** FLAGGED PROPOSAL — for the human to review and apply. Per AGENTS.md,
the file is the human-maintained instruction file and must **NOT** be edited by
agents directly. This document carries the exact proposed diff; the human applies
it to AGENTS.md, or instructs an agent to do so.

Two changes:

1. Add a **License** section mirroring README.md's `## License` (which itself
   mirrors the `LICENSE` file).
2. Rewrite the **password standard** under *Security Standards → Secrets &
   credentials → Hashed at rest* to match the implemented authentication
   architecture (embedded IdP credential store landed in
   `feat/local-credentials-store` / `feat/oidc-base-auth`).

---

## Change 1 — License section

### Placement recommendation

Insert a `## License` section directly after the `## Project` section (before
`## WARNING`). Rationale: AGENTS.md is the entry-point instruction file; the
licensing constraint is a project fact every agent should encounter before doing
any work, and placing it beside `## Project` (the "what is this repo" block) keeps
legal context next to identity context.

*Alternative (not recommended):* append at end of file after the `## UPDATES.md`
section, mirroring README.md's end-of-file placement. Rejected because agents read
AGENTS.md top-down as instructions; a tail license is more likely to be missed than
a header one.

### Proposed diff

```diff
--- a/AGENTS.md
+++ b/AGENTS.md
@@ -6,6 +6,15 @@
 ## Project
 
 - **Repo**: https://github.com/beardedparrott/Orchicon.git
 - **Language**: Go (control plane) + TypeScript (frontend)
 - **Design docs**: `DOCUMENTATION.md` — single comprehensive documentation file
 
+## License
+
+Copyright © 2026 beardedparrott. All rights reserved.
+
+This software is provided free of charge for personal and non-commercial
+use. You may use, copy, and modify it for your own non-commercial
+purposes. Redistribution, sublicensing, or integration into commercial
+products that generate revenue requires explicit written permission from
+the owner. See the [LICENSE](./LICENSE) file for the full terms.
+
 ## WARNING
```

This is a verbatim copy of README.md's `## License` section (lines 229–237),
which in turn distills the `LICENSE` file (Copyright 2026 beardedparrott,
All rights reserved, free-as-in-beer personal/non-commercial use, written
permission required for revenue-generating redistribution/sublicensing/
integration). Keeping the three documents textually aligned avoids license drift
between README.md, LICENSE, and AGENTS.md.

---

## Change 2 — Password-standard rewrite

### Current text (AGENTS.md line 160)

> - **Hashed at rest.** API keys are hashed before storage (never plaintext). Passwords are never stored by the control plane (OIDC handles authentication). See DOCUMENTATION.md §Authentication.

### Proposed text

> - **Hashed at rest.** API keys are hashed before storage (never plaintext). Human passwords are stored only by the embedded identity provider (bcrypt/argon2), never by control-plane business logic; external authentication flows through OIDC. See DOCUMENTATION.md §Authentication.

### Proposed diff

```diff
--- a/AGENTS.md
+++ b/AGENTS.md
@@ -157,7 +157,7 @@
 ### Secrets & credentials
 
 - **No secrets in code or commits.** DSNs, API keys, tokens, and passwords come from the environment (`internal/config`) or a secret store — never hardcoded, never committed. The `.env.example` file documents the variables without containing real values.
 - **No secrets in logs.** Never log DSNs, tokens, passwords, or full request payloads that may carry credentials. The slog setup in `cmd/orchicon/main.go` logs structured fields; only log non-sensitive identifiers (tenant id, project id, trace id).
-- **Hashed at rest.** API keys are hashed before storage (never plaintext). Passwords are never stored by the control plane (OIDC handles authentication). See DOCUMENTATION.md §Authentication.
+- **Hashed at rest.** API keys are hashed before storage (never plaintext). Human passwords are stored only by the embedded identity provider (bcrypt/argon2), never by control-plane business logic; external authentication flows through OIDC. See DOCUMENTATION.md §Authentication.
 - **Dev-only credentials are placeholders.** The container's internal Postgres runs with trust auth on localhost (`orchicon:orchicon` in the default DSN); the `.env.example` documents real variables without values. None of these may appear in a production deployment config.
```

### Why

- The old sentence ("Passwords are never stored by the control plane") is
  factually obsolete: the embedded OpenID Provider (in-process
  `internal/auth/op` on zitadel/oidc v3) stores local-account credentials in the
  tenant-scoped `local_credentials` table, argon2id (RFC 9106 m=64 MiB/t=3/p=4)
  PHC strings by default with bcrypt `$2a$`/`$2b$`/`$2y$` accepted on verify via
  prefix dispatch. See DOCUMENTATION.md §Authentication.
- The rewrite preserves the security intent (plaintext is never persisted,
  logged, or returned; only the hash is at rest) while scoping it correctly:
  credential storage lives **inside the identity-provider boundary**
  (`internal/auth` + `internal/auth/op`), never in control-plane business logic,
  service RPCs, or Ask Orchicon tools. External auth still flows through OIDC.
- DOCUMENTATION.md already describes this as "a deliberate, narrow amendment to
  the AGENTS.md 'passwords are never stored by the control plane' standard" — this
  change brings AGENTS.md in line with the documented and shipped reality so the
  two documents stop contradicting each other.

---

## Scope / non-goals

- Only the two bullets above. The `Things you need to know → Auth` entry
  (line 343, "OIDC-based with built-in dev IdP for local verification (HS256)")
  remains accurate and is untouched.
- No change to README.md or LICENSE — those already carry the canonical text;
  this proposal only aligns AGENTS.md to them.
- This document itself is the deliverable of PR branch
  `docs/agents-md-license-password-proposal`; it is committed and PR'd so the
  human can review the diff and apply it to AGENTS.md (or authorize an agent to).
