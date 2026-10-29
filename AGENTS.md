# burrow — agent guide

Self-hosted, OpenWork-compatible control plane — Headscale for OpenWork. One Go
binary speaking the OpenWork desktop client's control-plane protocol: OIDC
sign-in, org, Library skills, and the MCP endpoint that delivers skills to the
agent. See `README.md` for the user-facing story and `docs/client-contract.md`
for the wire contract.

- Origin of truth: <https://gitea.stump.rocks/stump.wtf/burrow> (Gitea). GitHub
  (<https://github.com/stump-wtf/burrow>) is a read-only push mirror — issues,
  PRs and pushes go to Gitea, and `tea` (never `gh`) talks to it.
- `docs/client-contract.md` is the specification burrow implements. Where it
  says the client leaves something unspecified, burrow designs it and records
  the decision in an ADR or spec.

## Clean-room rules (non-negotiable)

burrow is written clean-room from the MIT-licensed OpenWork desktop client.
These rules bind every session, human or agent:

1. **Never open, read, grep, list, or fetch** anything under `ee/` in
   <https://github.com/different-ai/openwork> (or any fork or clone of it),
   `packages/docs/openapi.json`, or `packages/sdk/src/gen/*`. Those are
   EE-licensed or labelled that way. If you have already read any of them in
   this or an earlier session, stop and say so instead of building.
2. The spec is `docs/client-contract.md`, written only from the MIT desktop
   client. To confirm a detail, read only the MIT client paths it cites:
   `apps/app/src`, `apps/desktop/electron`, `apps/server/src`, `packages/*`
   (except the two excluded above).
3. Where the contract says the client leaves something unspecified, design it
   yourself. Don't look for how the upstream cloud backend does it.
4. Name and brand it `burrow`. Never the upstream product's cloud names in
   user-facing text, except to say burrow is compatible with the OpenWork
   desktop app.

## Map

| Path | What lives there |
|---|---|
| `cmd/burrow` | entrypoint: config, store, OIDC, HTTP server |
| `internal/config` | every `BURROW_*` env var, in one place, with validation |
| `internal/httpapi` | the whole wire surface: OIDC web flow, `/api/den/v1/*` control-plane JSON, `/mcp/agent` |
| `internal/oidc` | OIDC RP against Pocket ID (public client + PKCE by default) |
| `internal/store` | persistence and the schema; single-use grant tokens live here |
| `internal/skills` | the Library: `<name>/SKILL.md` from a directory or a git checkout, with a cache |
| `internal/providers` | Stage B org LLM providers, fail-closed at boot |
| `internal/upstream` | Stage C: burrow as an MCP *client* — the streamable-HTTP calls, the host allowlist every one passes, the OAuth calls, the tool cache |
| `internal/seal` | AES-256-GCM sealing for a credential burrow must present again (a member's MCP key, an OAuth token) |
| `docs/client-contract.md` | the client contract, as observed from the MIT client |
| `docs/adrs`, `docs/openspec/specs` | decisions and specifications (SDD layout) |

## Commands

| Command | Does |
|---|---|
| `make` | build then `check` — the "did I break it" loop |
| `make check` | `fmt vet test race`. This is the CI gate; run it before every push |
| `make lint` | static checks only (`fmt vet`) |
| `make test` / `make race` | tests; the race run needs CGO, which the Makefile sets |
| `make tidy` | gofmt in place on tracked files, plus `go mod tidy` |

Required contexts in CI: `ci / lint*`, `ci / test*`, `secret-scan / gitleaks*`.

## Configuration

Boot fails without `BURROW_DATABASE_URL` (a `postgres://` URL, ADR-0002),
`BURROW_OIDC_ISSUER_URL`, `BURROW_OIDC_CLIENT_ID`, and one of
`BURROW_BASE_URL` or `BURROW_OIDC_REDIRECT_URL` — the OIDC redirect has to be
absolute, which `cmd/burrow/main.go` checks after the config loads.
`BURROW_OIDC_CLIENT_SECRET` stays empty for a public PKCE client. Skills need
either `BURROW_SKILLS_DIR` (a directory of `<name>/SKILL.md`) or
`BURROW_SKILLS_GIT_URL`. The rest have defaults you can read in
`internal/config/config.go` — that file is the list; do not read an env var
anywhere else. A default applies only when a variable is unset or blank: a
duration or boolean that is set but unreadable (`7d`, `300`, `off`) stops
burrow at boot instead of quietly running on the default.

Secrets never live in the repo. Credentials arrive from the environment at
runtime (OpenBao in deployment). Never log a token, session id, or provider key:
log the outcome and the identifiers, not the value.

## Conventions

- Log with `log/slog` structured fields, the way `internal/skills` does. A
  refused or failed request must be visible in production logs at Info/Warn,
  not Debug.
- HTTP responses go through the `writeJSON` helpers so error shape stays uniform.
- Comments that carry governance use the house format, e.g.
  `// Governing: ADR-0002 (store is Postgres), SPEC-0001 REQ "Single-use grants"`,
  and block comments are a header plus an append-only dated log — no ASCII
  banners.
- Architecture context: ADRs in `docs/adrs/`, specs in `docs/openspec/specs/`.
  Run `/sdd:prime [topic]` at the start of a session to load the relevant ones.

## Git and pull requests

- Worktree per concern, cut from a freshly fetched base:
  `git fetch origin && git worktree add ../burrow-wt-<n> -b <branch> origin/main`.
  Never commit from a shared checkout, never stash-and-switch.
- Branch names in practice are `feat/`, `bug/`, `toil/` plus a short
  description; the SDD tracker's `feature`/`epic` prefixes map onto the PR
  labels `feature`, `bug`, `toil`. One PR = one concern.
- Commit subjects are semantic and under 72 characters (`feat:`, `fix:`,
  `chore:`, `refactor:`, `docs:`, `test:`); the body says *why*. Never
  force-push `main`.
- PR body: what changed, why, how it was verified. Use `Closes #N` for a full
  fix and `Part of #N` for a slice, and reference the spec or ADR the change
  implements.
- Request review from the opposite identity (`@joestump` ↔ `@joestump-agent`)
  as a reviewer request, then ping the reviewer on Signal. Do not merge your
  own PR.
- Issue references resolve against the repo you commit into; StumpCloud
  infrastructure work is tracked in `stumpcloud/stumpcloud`, not here.

## CI and deploy

`.gitea/workflows/ci.yaml` gates merges, `gitleaks.yaml` scans for secrets, and
`ship.yaml` publishes `gitea.stump.rocks/stump.wtf/burrow:latest` on every
merge to `main`.

Deployment is not in this repo: it is
`playbooks/services/burrow.yaml` in <https://gitea.stump.rocks/stumpcloud/ansible>,
which runs burrow inside cloud01's edge compose stack. The container's whole
environment is rendered into `burrow.env` from that repo's `gva.yaml` plus a
Jinja template that pulls secrets from OpenBao — so a new env var needs a
change there before it exists here, and a change that makes an env var
*required* must land and converge before the image that needs it merges, or the
deploy crash-loops.

CI jobs on this fleet run inside containers: `localhost` is the job's own
network namespace and `host.docker.internal` does not resolve. A test job that
needs a service (a database, say) publishes it on a run-scoped Docker network
and connects the job container to that network by alias; copying a
`localhost:5432` recipe from GitHub Actions will not work here.
