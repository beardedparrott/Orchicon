# Persistent permission policy (deny / accept list)

Session grants are in-memory, per conversation, and die with the plane — a
momentary human judgment. This is the other thing: **durable policy the
operator writes once**, in a YAML file.

The two are deliberately not the same mechanism. Conflating them would either
lose the policy on every restart or make every grant permanent.

## Precedence

```
never-allow binary class  (sudo/dd/mkfs/… — absolute, never prompts)
        ↓
deny list                 (refuse; outranks a session grant)
        ↓
session grant             (proceed silently)
        ↓
accept list               (proceed silently)
        ↓
conversation's project    (default scope: proceed silently)
        ↓
otherwise                 (ask)
```

The deny list sits **above** the session grant on purpose: a grant suppresses
prompts, but it cannot open a path the operator excluded. It has the same
standing as the never-allow binary class, expressed for paths.

Exactly **one** function implements this order — `permpolicy.Store.Decide`.
The consent core (`internal/askorchicon`) and the guard shim consult it through
one accessor, so enforcement and prompting cannot drift.

## The file

* Path: `ORCHICON_PERMISSION_POLICY`, default
  `<ORCHICON_DATA_DIR>/permission-policy.yaml`. A documented copy lives at
  `deploy/permission-policy.example.yaml`.
* YAML, so the operator can comment it.
* **The file is the single source of truth; the plane is the single writer.**
  Both clients manage entries through the plane API, which reads and rewrites
  this file — a hand-edit and a UI change cannot diverge.
* **Malformed input fails loudly at boot** with the path and the parse error.
  A policy file that silently parses to nothing is worse than no file, because
  the operator believes the exclusions are in force. Unknown keys are malformed
  too.
* **Empty or absent means "no policy"** — nothing denied, nothing
  pre-accepted — never "deny everything".

## Reload semantics: read on each consult

The file is re-read on **every** gated decision. A gated decision is a write, an
execution, or a read of a denied path — never a hot loop — and the file is a few
hundred bytes, so reading it per decision buys "an edit takes effect
immediately" with no watcher and no staleness window.

Do not cache it; do not add a file watcher.

## The preset

A fresh instance (where `ORCHICON_PERMISSION_POLICY` is unset and no file
exists yet) is seeded with:

```yaml
deny:
  - ~/.ssh/**
  - ~/.gnupg/**
  - ~/.aws/**
  - ~/.config/gh/**
  - ~/.git-credentials
  - ~/.netrc
  - ~/.docker/config.json
accept: []
```

Credential stores are the reason: reads never ask, so without these entries the
agent could read a private key or a stored token with no prompt at all. Nothing
is pre-accepted beyond project directories, which are already the default scope.

An explicitly pointed-at nonexistent path is **never** created — that is what
keeps "an absent file means no policy" a state you can actually test.

## Clients

* **GUI**: Settings → **Permissions** — list, add and remove entries. A denied
  row says a session grant cannot override the entry instead of offering a grant
  that would be refused.
* **TUI**: Control → **Permissions** — the same list, add (create form) and
  remove (confirmed action), with the same "a session grant cannot override
  this entry" statement on a deny row.
* Both talk to the plane API (`GetPermissionPolicy`,
  `AddPermissionPolicyEntry`, `RemovePermissionPolicyEntry`), which reads and
  rewrites the file. An entry added in the GUI is visible in the TUI and to a
  hand-edit, and vice versa, because there is only one copy: the file.

## Caveat on UI writes

A write from either client regenerates the file from the parsed policy with a
regenerated header comment block. **Entry-level inline comments a human added
are not preserved.** The top block documents the precedence and the rules, and
the file fits on one screen; surgical YAML comment preservation is not worth the
code.
