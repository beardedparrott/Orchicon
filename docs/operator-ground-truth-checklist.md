# Operator Ground-Truth Checklist (Multi-adapter gate)

> **Human-gated.** This checklist is run by the OPERATOR with the real keys
> against the live instance. A worker cannot verify credential paths it
> does not hold — the worker deliverable is the standing CI audit, the
> env-gated live smoke suite, the parity matrix, and this checklist; the
> PASS/FAIL ground truth below is recorded in the work item by the human.
>
> Run the env-gated live smoke first (it exercises every provider endpoint
> directly):
>
> ```
> ORCHICON_TEST_LIVE_ORCHICON=1 go test ./internal/orchicon/ -run TestLiveSmokeOrchicon
> ```
>
> Where a provider key is missing, set `ORCHICON_TEST_LIVE_<PROVIDER>` only
> after wiring the secret. The smoke NEVER runs in default CI (it is gated).

---

## 1. No synthesized data planes (final audit)

- [ ] `make ci` (or `make synth-data`) passes locally — **exit 0** confirms
  no `MockModelDiscoverer` / `MockMCPDiscoverer` / `mockModels` /
  `MockProvider` in NON-TEST source, and that
  `internal/orchicon/sourcing.go` still carries the `LIVE TRUTH ONLY`
  probe-or-nothing contract.
- [ ] The same `synth-data` gate is **standing CI**: a PR touching
  non-test adapter code that introduces a synthesized-data fallback FAILS
  the build (`.github/workflows/ci.yml` runs `make ci-go` on PRs → `develop`).
- [ ] The one sanctioned simulation switch, `ORCHICON_SIMULATE_ADAPTER`
  (ADR-0010 D2, opencode offline-dev opt-in), is **NOT** flagged — confirm
  the audit is silent on it.

## 2. Live-source verification (per provider, operator keys)

For each built-in provider (`anthropic`, `openai`, `openrouter`, `opencode`,
`opencode-go`, `commandcode`, `ollama`) plus one custom OpenAI-compat:

| Check | How | Pass criterion | Result |
|---|---|---|---|
| Probe lists ONLY live endpoint models | Settings → Adapters → provider → eyeball the model list against the endpoint's `/models` (or `/v1/models`) | The Settings eyeball lists **only** ids the endpoint returned; no ids that "seemingly exist". Live smoke asserts `ListModels > 0` with a non-empty id. | |
| Probe failure → degraded/amber, ZERO rows | Revoke the token (or point the base URL at a dead port) and reload | The provider shows the **degraded/amber** state with **zero** model rows; the picker is never blank-and-silent and never shows a synthesized list. | |
| Diagnosable log line | Inspect the control-plane log after the failed probe | A `sourcing: probe <url> → HTTP <code>` line naming the **URL** and the **failure class** (`401 = token problem`, `404 = wrong base shape`, or `unreachable`). | |

## 3. Corrected-contract conformance (fix branch, not the original ADR)

Each corrected contract must have a PASSING check:

| Contract | Passing check | Result |
|---|---|---|
| **Probe-or-nothing sourcing** | Failed probe → degraded (amber) with zero rows and the diagnosable log line; the vendored catalog never contributes a model id it did not probe (catalog = metadata enrichment only). | |
| **CLI-aware validation registry (`CLIProviderRegistry`)** | Save a worker setting with ref `opencode/deepseek/<model>` → it validates AND re-saves successfully (the registry is injected into the settings validator + gateway, not only the picker). | |
| **Batch Save/Discard in providers tab** | In Settings → Adapters → Providers, toggle hidden-models then **Discard** → the change is NOT persisted; **Save** → a single update is applied. No per-checkbox auto-save. | |
| **Session bootstrap refresh arms on load** | Full reload of the app → the access-token proactive refresh is armed on the bootstrap path (a dead session lands on `/login?next=…`, not a raw `ConnectError: [unauthenticated]`). | |

## 4. Results recording

Record the outcomes in this task / run summary after the run:

```
# per provider: probe ok?  rows?  degraded-on-401?  log line?  live turn ok?
anthropic:      probe=ok rows=N degraded-401=observed log=ok turn=ok usage(in,out,cacheRead,cacheWrite)=(a,b,c,d)
openai:         ...
openrouter:     ...
opencode:       ...
opencode-go:    ...
commandcode:    ...
ollama:         ...
custom(openai-compat): ...

# corrected contracts
probe-or-nothing = PASS|FAIL
cli-validator     = PASS|FAIL
batch-save-discard= PASS|FAIL
session-bootstrap = PASS|FAIL

# final audit
make synth-data = PASS (exit 0)
```

---

## Operator setup reference

See `DOCUMENTATION.md` → `## Operator Setup (Adapters)` for this surface: how
providers/tokens/secrets are added (Settings → Adapters), MCP registration,
memory scope, and compaction controls.
