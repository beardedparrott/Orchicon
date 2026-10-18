# desktop-e2e — packaged app, clean machine, no plane, streaming

Run: `20261018T205751Z` · instance `dev` · platform `linux` · control port `8080`

**Verdict: RUN INVALID — nothing was proven**

| status | count |
|---|---|
| PASS | 1 |
| FAIL | 0 |
| SKIP | 1 |

A SKIP is UNPROVEN, never a pass.

## Per-step

| step | PASS | FAIL | SKIP |
|---|---|---|---|
| 0 | 1 | 0 | 1 |

## Defects filed (against the OWNING sibling, never patched here)


## Why this run is not a proof

a plane is already answering http://127.0.0.1:8080/healthz — this run cannot prove the no-plane offline state
