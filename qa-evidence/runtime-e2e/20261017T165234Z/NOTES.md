# What this evidence is — and what it is not

This tree is the **step-0 (validity precondition) evidence** produced by the
implementation step, inside the workflow's runtime container. It is committed so
the harness's own first verdict is on the record:

- `00-preflight.log` — the host's state, verbatim: **no container engine is
  installed** (docker/podman/nerdctl/docker-compose all absent — the AC1
  precondition is genuinely satisfied on this host), plus the ports and disk.
- `results.tsv` — two PASS (no engine; a git checkout) and one **SKIP**: the
  instance's default ports (5432/4222/8080) are **held** by a plane already
  running on the host, so this run cannot prove that it drove *this instance's
  own* plane. The harness therefore exits **2: RUN INVALID** rather than
  reporting a misleading PASS.
- `defects.tsv` — empty **by design**: a held port is the host's condition, not a
  defect against a sibling file (AC10 files defects against owners only).

**The full session (AC1–AC8) has NOT been run here.** Provisioning installs a
system package and the whole procedure needs an engine-free machine where work
can actually be containerised — neither is true of a rootless workflow container,
and the ports are held. Running it on a spare host is the QA step:

```bash
scripts/runtime-e2e.sh --preflight-only                  # must exit 0 on a genuinely engine-free host
scripts/runtime-e2e.sh                                   # AC1–AC8 into qa-evidence/runtime-e2e/<UTC>/
scripts/runtime-e2e.sh --image-id <id> --custom-image <tag>   # AC6 against a real built image
```

See `docs/runtime-e2e-acceptance.md`.
