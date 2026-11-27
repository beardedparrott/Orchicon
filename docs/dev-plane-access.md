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
curl -s -H "Authorization: Bearer $KEY" http://localhost:8080/orchicon.api.v1.WorkItemService/ListWorkItems
```

Use `:$DEV_PLANE_PORT` style variables in scripts rather than hardcoding, so a moved instance does not
silently point you somewhere else.

## If the fetch fails

`orchicon secret get DEV_PLANE_KEY` can fail for three reasons, and the message tells you which:

| Message | Meaning | What to do |
|---|---|---|
| `not running inside a worker execution (missing …)` | you are not in a dispatched run, or the worker has no role binding on its own instance | say so in your summary — this is a platform-side grant |
| `not available to this run` | the secret exists but was **not selected** on your work item | say so; do not look for the key elsewhere |
| `run … is completed/failed/aborted` | the run is over and cannot fetch | not recoverable mid-run |

**Do not improvise around a refusal.** Do not read a keyring, a dotfile, another worker's environment,
or a checked-in token. A refusal means the operator did not grant that secret to this work item, and
the correct report is a NOT VERIFIED criterion naming the missing grant.

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
