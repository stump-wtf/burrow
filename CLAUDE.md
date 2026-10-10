# burrow

The agent guide for this repository is **[`AGENTS.md`](AGENTS.md)** — read that
first. It carries the project description, the clean-room rules (unchanged by
the move, still non-negotiable), the code map, the commands, the conventions,
and the git/PR/CI practice. This file keeps only what tooling reads out of it,
so the two cannot drift.

## Workflow

- `make check` is the CI gate (fmt, vet, test, race). Run it before every push.
- CI required contexts: `ci / lint*`, `ci / test*`, `secret-scan / gitleaks*`.
- Secrets never live in the repo. The OIDC client credential and similar come
  from the environment (OpenBao in deployment) at runtime.

## Architecture Context

This project uses the [SDD plugin](https://github.com/joestump/claude-plugin-sdd) for architecture governance.

- Architecture Decision Records are in `docs/adrs/`
- Specifications are in `docs/openspec/specs/`

Run `/sdd:prime [topic]` at the start of a session to load relevant ADRs and specs into context.

### SDD Configuration

#### Tracker

- Type: Gitea
- Owner: stump.wtf
- Repo: burrow
- Host: https://gitea.stump.rocks

#### Branch Conventions

- Prefix: feature
- Epic Prefix: epic

#### PR Conventions

- Close Keyword: Closes
- Ref Keyword: Part of
- Include Spec Reference: yes

### Governing Comments

When implementing code governed by ADRs or specs, leave comments referencing the governing artifacts:

```
// Governing: ADR-0003 (native multiplexer, tmux as backend), SPEC-0002 REQ "Backpressure"
```
