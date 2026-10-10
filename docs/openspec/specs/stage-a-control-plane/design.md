---
status: draft
date: 2026-10-09
implements: [ADR-0001]
---

# SPEC-0001 design: Stage A control plane

Implementation decisions for the stage-a-control-plane spec. The wire shapes
themselves are fixed by `docs/client-contract.md`; everything here is the
"design it yourself" layer.

## Layout

```
cmd/burrow          main: config, store, catalog, OIDC client, HTTP server
internal/config     BURROW_* environment configuration
internal/store      Postgres schema + token/grant/session lifecycle
internal/oidc       authorization-code + PKCE client (any issuer)
internal/skills     <name>/SKILL.md loading (dir or git), frontmatter, search
internal/httpapi    routes: auth pages, /api/den/v1/*, /mcp/agent
```

## Decisions

- **Go single binary, net/http routing.** Go 1.22 method+pattern routing
  (`GET /v1/me`, `POST /v1/mcp/token`, `{id}` wildcards) covers the whole
  surface without a router dependency. All `/v1` routes mount under
  `/api/den` via a subtree + `http.StripPrefix`; the client's odd double-api
  sign-out path (`/api/den/api/auth/sign-out`) falls out naturally.
- **Postgres via pgx's database/sql driver.** The store was modernc SQLite with
  `MaxOpenConns(1)` (SQLite serializes writes, and one connection avoided
  SQLITE_BUSY under the desktop client's polling); ADR-0002 moved it to the
  shared Postgres cluster before the access-control schema of #12/#13 landed on
  it, so that work happens once. Pure Go either way, so the binary is still
  CGO_ENABLED=0.
- **Tokens stored hashed.** Grants, sessions, and MCP tokens are 43-char
  base64url secrets; only their SHA-256 lands in the database. The raw value
  exists exactly once — in the response that hands it over.
- **Single-use by delete-on-read.** `ConsumeGrant` and `TakeOIDCState` are one
  `DELETE … RETURNING` each — the delete is the read, so a replay finds no row
  regardless of interleaving. Expiry is checked after the read so a burned
  grant cannot be resurrected by racing the clock.
- **401 code mapping.** missing header → `unauthorized`; unknown token →
  `invalid_session`; expired → `session_expired`. Exchange failures use
  `invalid_grant`, which the client does not treat as session-terminating —
  right, since a typo'd paste must not sign the desktop app out.
- **OIDC state without cookies.** The client contract forbids cookies on the
  API, not on the browser flow, but burrow still keeps the sign-in flow
  cookie-free: the state parameter is the key to a server-side (state,
  verifier, desktop) row with a 10-minute TTL. The `OIDCClient` interface in
  httpapi exists so the full flow is testable against a fake issuer.
- **User id = OIDC subject.** Stable across email changes; email/name claims
  refresh on every sign-in (upsert). Member ids are `mem_<sub>`.
- **Skills source.** `BURROW_SKILLS_DIR` reads a directory directly;
  `BURROW_SKILLS_GIT_URL` shallow-clones into the cache dir and `git
  pull --ff-only`s on refresh (a failed pull serves the cached clone). The
  catalog caches for 30s so the client's polling does not hit git. Frontmatter
  is parsed by hand — `key: value` lines between `---` markers; no dependency.
- **Marketplace naming.** One marketplace named `<OrgName> Library` with id
  `mkt_<org slug>`; plugin `plg_<skill>`, config object `cfo_<skill>`,
  membership `pm_<skill>`, latest version `cfv_<skill>`. IDs are derived, not
  stored: the skills directory is the only source of truth, and a re-clone
  cannot desync an id.
- **Library items as plugins.** Every skill is a Library item of `type:
  "plugin"` with `componentKinds: ["skill"]`, `componentCount: 1`, `role:
  "owner"` (single-org: everyone manages), and edges `[{kind: "org_wide"}]`.
- **MCP tool schemas (designed, not copied).** `search_capabilities {query}`
  matches every whitespace-separated term against name/title/description;
  `execute_capability {capability, input?}` resolves the `skill:` and
  `plugin:` capability forms and returns the SKILL.md with a "read and follow
  it" preamble; `get_skill {name}` returns the raw source. All results are
  single text content blocks.
- **MCP transport minimalism.** JSON responses only (the client accepts JSON
  or SSE); every notification gets 202/empty; no session state is kept — an
  `mcp-session-id` is unnecessary for the client's reader and is omitted.
  Auth failures return JSON-RPC error bodies whose message and `data.code`
  carry the remint codes (`missing_mcp_token`, `invalid_mcp_token`,
  `expired_mcp_token`).
- **Origin handling.** `BURROW_BASE_URL` overrides; otherwise the origin is
  derived per request from `X-Forwarded-Proto` (first hop) / TLS / Host, so
  loopback dev (http) and proxied production (https) both produce correct
  `denBaseUrl`, `denApiUrl`, and MCP `resource` values.
