# burrow

Self-hosted, OpenWork-compatible control plane — Headscale for OpenWork. A Go
single binary that speaks the OpenWork desktop client's control-plane protocol:
OIDC sign-in, org, Library skills, and the MCP endpoint that delivers skills
to the agent. See `README.md`.

- Origin of truth: https://gitea.stump.rocks/stump.wtf/burrow (Gitea). GitHub
  (https://github.com/stump-wtf/burrow) is a read-only push mirror — issues,
  PRs, and pushes go to Gitea.
- The client-facing API surface is specified by `docs/client-contract.md`.
  That file is the specification burrow implements; where it says the client
  leaves something unspecified, burrow designs it and records the design in an
  ADR or spec.

## Clean-room rules (non-negotiable)

burrow is written clean-room from the MIT-licensed OpenWork desktop client.
These rules bind every session, human or agent:

1. **Never open, read, grep, list, or fetch** anything under `ee/` in
   https://github.com/different-ai/openwork (or any fork or clone of it),
   `packages/docs/openapi.json`, or `packages/sdk/src/gen/*`. Those are
   EE-licensed or labelled that way. If you have already read any of them in
   this or an earlier session, stop and say so instead of building.
2. The spec is `docs/client-contract.md`, which was written only from the MIT
   desktop client. To confirm a detail, read only the MIT client paths it
   cites: `apps/app/src`, `apps/desktop/electron`, `apps/server/src`,
   `packages/*` (except the two excluded above).
3. Where the contract says the client leaves something unspecified, design it
   yourself. Don't look for how the upstream cloud backend does it.
4. Name and brand it `burrow`. Never the upstream product's cloud names in
   user-facing text, except to say burrow is compatible with the OpenWork
   desktop app.

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

## Workflow

- `make check` is the CI gate (fmt, vet, test, race). Run it before every push.
- CI required contexts: `ci / lint*`, `ci / test*`, `secret-scan / gitleaks*`.
- Secrets never live in the repo. The OIDC client credential and similar come
  from the environment (OpenBao in deployment) at runtime.
