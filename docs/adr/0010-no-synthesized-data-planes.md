# ADR-0010: No synthesized data planes — live-truth sourcing and validation

Date: 2026-09-02
Status: Accepted (operator directive during Native Adapter QA, round 1–4)
Supersedes parts of: ADR-0006 (sourcing fallback order), ADR-0004 (picker tiers), ADR-0003/0005 examples

## Context

The Native Adapter feature's first human QA round surfaced a failure-mode class that the
original ADRs encoded as *features*: synthesized data served when live truth was
unavailable. Concretely:

- The model picker's model tier called opencode-CLI discovery for EVERY adapter; for the
  native `orchicon` kind the CLI can never emit the orchicon provider namespace, so the
  model list was structurally empty.
- The providers-tab eyeball served the vendored catalog snapshot when a probe failed
  (and did not probe at all when a base-URL override matched the built-in default), so a
  broken connection *looked* like a working provider with plausible models.
- `MockModelDiscoverer` (4 fake models) and `MockMCPDiscoverer` (hardcoded server list)
  served silently when the opencode binary was absent, in every mode.
- The legacy provider tier was a static hardcoded set while the model tier was live —
  validator and picker disagreed, so freshly-selected CLI refs failed validation with
  "provider not found".
- The custom-provider auto-resolver silently dropped the port from rewritten URLs.

Every one of these produced a UI that looked alive while being wrong. The operator's
directive, verbatim in intent: **"It should be it either works or it doesn't. I don't
want to just throw in some random possible models that seemingly exists from that
provider. That will confuse users if they are having connection issues."**

## Decision

1. **Live truth only for model ids.** Model lists come from: the `/models` probe
   (per-wire URL + auth derivation), the operator's manual entries, or CLI discovery
   (legacy adapters). A failed probe yields an EMPTY, visibly-degraded list — never a
   synthesized fallback. The vendored catalog is **metadata enrichment only** (context /
   output / tools / pricing by id match); it never contributes model ids.
2. **No mock planes in any mode.** Discoverers are nil without their binary; the RPC
   layer returns actionable Unimplemented errors naming `ORCHICON_OPENCODE_BIN`. The
   opencode adapter's `ORCHICON_SIMULATE_ADAPTER=1` path remains as the ONLY sanctioned
   simulation (explicit opt-in, loud-warned, never a fallback).
3. **Runtime vs UI surface semantics.** The "works or it doesn't" signal lives at the UI
   surface (eyeball, picker: empty + degraded + amber). Runtime model metadata stays
   non-fatal (empty list + warn — chat never needs the models list; failing a worker turn
   over a metadata hiccup is worse than an empty list). Both paths: zero synthesized ids.
4. **Live usage only.** Usage records come from real provider streams; no estimated,
   interpolated, or synthesized token counts anywhere (the directive applied to money).
5. **Validator = picker.** Model-ref validation uses the CLI-aware registry
   (`aigateway.CLIProviderRegistry`: static catalog ∪ live CLI provider ids, TTL-cached,
   never errors) injected into both the settings validator and the gateway. The static
   builtin catalog alone is invalid for CLI-namespace refs.
6. **Picker contract (operator UX directive).** All three tiers live INSIDE the selector
   box: closed state = selected-ref trigger or the always-enabled search input; open =
   search → adapter pills → provider pills → model list. Provider pills under legacy CLI
   adapters are OPTIONAL filters derived from live CLI discovery (All reset); the
   provider tier gates only under orchicon. No auto-open effects — they fight the user.
7. **Plane-aware URL resolution.** Custom-provider loopback base URLs are rewritten to
   the detected host gateway in container mode ONLY (host AND port preserved — the
   port-dropping bug is the cautionary tale), with plain-language notes, and a
   self-healing probe persists a verified repaired URL (audit-logged) so chat uses it too.
8. **CI guard (task #14).** A standing audit greps non-test code for synthesized-data
   fallbacks and fails the build; the corrected contracts are the E2E gate's ground truth.

## Consequences

- Fresh/failed environments show EMPTY model lists with a precise log line (URL + failure
  class) instead of plausible fakes. This is the intended behavior.
- `catalog.json` remains in the repo as metadata (context/pricing/tools) and for tests.
- The probe cache keys fold bearer + base URL (credential rotation and self-heal
  candidates never hit stale entries).
- Workers implementing #12/#13 inherit these semantics as binding ACs; #14's gate
  enforces them.