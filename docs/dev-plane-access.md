# Reaching the dev Orchicon plane

Workers on **prod** often need to exercise the **dev** plane to do their testing. This page says how,
and what the limits are.

## The two planes

| | prod | dev |
|---|---|---|
| host port | `:8091` | `:8080` |
| who may touch it | **nobody**, except by reading this page | **you**, freely |

Dev is yours: create work items, workers, projects, runs, secrets; restart it when a task needs a
restart. **Prod is off limits** — do not read it, write it, migrate it, or restart it.

## Your credential

Dev requires an API key. The key is **not** in your environment: this platform deliberately does not
put secret values where every child process can read them. Fetch it on demand:

```bash
KEY=$(orchicon secret get DEV_PLANE_KEY)
```

That prints the value on **stdout and nothing else**, so the shell substitution above is the intended
use. Errors go to stderr.

Then call dev directly:

```bash
# Connect endpoints are POST with a JSON body — a bare GET returns 405 and looks like a broken plane.
curl -s -X POST \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"page_size": 5}' \
  http://localhost:8080/orchicon.api.v1.WorkItemService/ListWorkItems
```

Dev is on `:8080` today. If a call fails to connect, check the port rather than assuming the instance
moved — and say so in your summary instead of guessing another one.

## If the fetch fails

`orchicon secret get` is a plain get: it resolves the plane and your credential the same way `orch`
does (`ORCHICON_URL`/`ORCHICON_TOKEN`, or the saved profile), reads any secret the tenant holds, and
needs nothing from your execution. The three failures are about the *caller* and the *name*:

| Message | Meaning | What to do |
|---|---|---|
| `no plane URL — set ORCHICON_URL …` | this shell has no plane configured | say so in your summary; do not guess a URL |
| `no credential for <url> — set ORCHICON_TOKEN …` | no credential resolved for that plane | say so; the operator has to grant you access |
| `no secret named "<NAME>" in this tenant` | the name is wrong, or the secret does not exist | check the spelling with the operator before retrying |

There is **no** run-scoped refusal any more: the fetch does not depend on a work item's `secret_ids`,
on your execution, or on your worker having a role. If the secret exists in the tenant and your
credential is valid there, the read succeeds — and it is audited.

**Do not improvise around a failure.** Do not read a keyring, a dotfile, another worker's environment,
or a checked-in token. A missing secret or a missing credential is a grant the operator has to make;
the correct report is a NOT VERIFIED criterion naming what was missing.

## Every read is audited

Each successful fetch writes a `secret.read` audit row naming your run and the secret. So:

- fetch when you need the value, not in a loop
- **never print the value** into your output, a log, a commit, or a file in the repository
- the audit trail is the record that you read it; it must not also become the leak

## Clean up after yourself

Anything you create on dev must be deleted when you are done — work items, runs, projects, workers,
secrets, throwaway databases. Leave dev as you found it, or the next worker inherits your mess and
mistakes it for real state.

## What the key cannot do

`DEV_PLANE_KEY` reaches dev over **HTTP**. It does not give you the `orchicon_plane_*` MCP tools — that
channel is minted per execution from your own instance's role binding, and it is a different door.
If a task needs the MCP channel, that is a role question for the operator, not a credential one.
