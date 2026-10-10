---
status: draft
date: 2026-10-09
implements: [ADR-0001]
---

# SPEC-0001: Stage A control plane (sign-in, org, Library skills, MCP endpoint)

## Overview

The first landable slice of burrow: everything the OpenWork desktop app needs
to sign in against an org's own OIDC provider, see its org, browse the Library,
and have its agent load skills over MCP. The wire contract is
`docs/client-contract.md`; this spec pins burrow's side of it — the decisions
the contract leaves open ("design it yourself") and the requirements each
endpoint must keep holding.

## Requirements

### Requirement: Browser sign-in

The page at `/` SHALL accept the desktop app's
`?mode=sign-in&desktopAuth=1&desktopScheme=openwork` query and authenticate
through the configured OIDC issuer using the authorization-code flow with
PKCE (S256). On success burrow SHALL mint a grant, redirect to
`openwork://den-auth?grant=<grant>&denBaseUrl=<origin>` (denBaseUrl always
present, URL-encoded), and also render the grant as copyable text.

### Requirement: Grant properties

A grant SHALL be at least 12 characters (manual-paste floor), single-use, and
short-lived (default 5 minutes). Exchange of an unknown, used, or expired
grant SHALL fail with a 401 whose `error` code is `invalid_grant` — a code
that is deliberately not in the client's session-termination set.

### Requirement: Exchange and sessions

`POST /v1/auth/desktop-handoff/exchange` (no auth header) SHALL return
`{token, user{id,email,name}, organization{id,slug,name}, connectEnabled:true}`.
Sessions SHALL be Bearer tokens stored hashed in the database (Postgres since
ADR-0002). The API SHALL answer
401 with exactly `unauthorized` (no header), `invalid_session` (unknown
token), or `session_expired` (expired token) — all three are in the client's
sign-out set, and no other 401 code may leak into these paths.
`POST <api base>/api/auth/sign-out` SHALL revoke the session.

### Requirement: Pre-provisioning mint

`POST /v1/auth/desktop-handoff {desktopScheme:"openwork"}` SHALL require an
authenticated session and mint a grant for the calling member. An
unauthenticated mint would be an auth bypass, so it SHALL 401.

### Requirement: Single-org model

Stage A SHALL expose one org from configuration (`BURROW_ORG_NAME` /
`BURROW_ORG_SLUG`), with every OIDC user a member holding role `owner`.
`/v1/me/orgs` rows SHALL carry all four of `id`, `name`, `slug`, `role` — the
client drops a row missing any one — plus `activeOrgId` and `activeOrgSlug`.
`/v1/org` SHALL return `currentMember.id`, `members[].{id,user{name,email}}`,
and an empty-but-present `teams` list. Group mapping is a later seam.

### Requirement: Desktop config

`/v1/me/desktop-config` SHALL return `connectEnabled: true` (without it the
agent gets no remote skills), `automationsEnabled: false`, and
`dashboardEnabled: false`.

### Requirement: Skills mapping

Library content SHALL come from a local directory of `<name>/SKILL.md` or a
git repository cloned and refreshed into a cache. burrow SHALL map skills
onto the client's marketplace/plugin/config-object model as: **one
marketplace** (`mkt_<org slug>`, named `<OrgName> Library`), **one plugin per
skill** (`plg_<name>`), and **one config object per skill** (`cfo_<name>`,
`objectType: "skill"`, `latestVersion.rawSourceText` = the SKILL.md source).
Skill directories SHALL be kebab-case (≤64 bytes) with a frontmatter
`description`; invalid entries are skipped with a log line, never fatal.
The endpoints `/v1/me/library`, `/v1/resources`,
`/v1/resources/marketplace-capabilities`, `/v1/marketplaces`,
`/v1/marketplaces/:id/resolved`, `/v1/plugins/:id/resolved`,
`/v1/plugins/:id/config-objects`, and
`/v1/config-objects/:id/versions/latest` SHALL answer with these derived
objects, and unknown ids SHALL 404.

### Requirement: Stage stubs

`/v1/llm-providers` SHALL return `{"llmProviders":[]}`,
`/v1/mcp-connections?scope=usable` SHALL return an empty `connections` list,
`/v1/inference-providers` SHALL return 404, and `GET <origin>/api/runtime-config`
SHALL return `{"denApiUrl":"<origin>/api/den"}`.

### Requirement: MCP endpoint

`POST /v1/mcp/token` SHALL return `{token, expiresAt, organizationId,
resource}` where `resource` is `<origin>/mcp` — so `<resource>/agent` ends in
`/mcp/agent`. The endpoint at `POST /mcp/agent` SHALL speak Streamable HTTP
JSON-RPC: `initialize` answers `protocolVersion` `2025-06-18`;
`notifications/initialized` (and every notification) answers **HTTP 202 with
an empty body**; tools `search_capabilities`, `execute_capability`, and
`get_skill` are listed with burrow-designed schemas; the resource
`skill://index.json` SHALL satisfy the agentskills.io discovery 0.2.0 schema
the client enforces (kebab-case names ≤64, `type: skill-md`, description
1..1024, `url: skill://…`, `capability: skill:<name>` or
`plugin:<pluginId>:<configObjectId>`). Missing, unknown, or expired MCP
tokens SHALL fail 401 carrying `missing_mcp_token` / `invalid_mcp_token` /
`expired_mcp_token` — codes the client remints on.

### Requirement: Secrets

The OIDC client credential and equivalent SHALL arrive only via environment
variables (OpenBao in deployment) and never live in the repo or the database.
Tokens and grants SHALL be persisted only as SHA-256 hashes.
