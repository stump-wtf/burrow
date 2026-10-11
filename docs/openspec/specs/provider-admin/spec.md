---
status: draft
date: 2026-10-10
implements: [ADR-0004]
amends: [SPEC-0002, SPEC-0003]
---

# SPEC-0004: Provider admin (store-backed rows, gateway-derived models, write API)

## Overview

Provider rows live in burrow's Postgres store and are managed through an
admin-gated write API; each row's model menu is derived from the row's own
LiteLLM key (`GET {baseURL}/models`), cached. The desktop-facing surface
(SPEC-0002 rows, SPEC-0003 visibility) is unchanged in shape — only the
menu's source moves.

## Requirements

### Requirement: Rows in the store

The store SHALL carry an `llm_providers` table: `id` (matching `/^lpr_/i`,
unique), `providerId`, `name`, `baseURL` (absolute http(s)), `apiKeyEnv` (an
environment-variable name, never one in burrow's own `BURROW_` namespace), `groups` (NULL for every member; otherwise a
non-empty JSON array of non-empty strings — an empty array is invalid, per
SPEC-0003), `nameOverrides` (JSON object, model id to display name),
`created_at`, `updated_at`. Writes SHALL apply the same validation
`providers.Load` applied to the file; an invalid write SHALL be rejected
without touching the served set.

#### Scenario: First-boot seed

- **GIVEN** an empty `llm_providers` table and a configured
  `BURROW_PROVIDERS_FILE`
- **WHEN** burrow starts
- **THEN** each file row is inserted (model names becoming name overrides;
  model-level `groups` are dropped with a boot warning), and the file is not
  consulted again while the table is non-empty

### Requirement: Gateway-derived model menus

Each row's `models[]` SHALL be the model ids its key reports from
`GET {baseURL}/models` (OpenAI-compatible `{"data":[{"id":…}]}`), fetched
with `Authorization: Bearer <the row's key>` from `apiKeyEnv`. The fetch
SHALL be cached per row for `BURROW_MODELS_TTL` (default 60s) and, on fetch
failure, the last-known ids SHALL be served; a row with no key set or a
never-succeeded fetch serves no models. `GET` for the desktop-facing surface
SHALL NOT be blocked on a live gateway call beyond the cached fetch. A
model's display name SHALL be its name override when present, else its id.

#### Scenario: Model added in LiteLLM

- **GIVEN** a row whose key's scope gains a model at the gateway
- **WHEN** the row's cache TTL next expires
- **THEN** the row lists the new model and the row's `updated_at` advances,
  so the client's resource snapshot triggers a re-sync

#### Scenario: Gateway unreachable

- **WHEN** a cache refresh fails
- **THEN** the last-known menu is served and the failure is retried after a
  bounded cooldown, not once per request

### Requirement: Admin surface

`/v1/admin/llm-providers` SHALL serve CRUD for rows: `GET` (list, each row
with its stored fields and per-row `etag`), `POST` (create; `409` on an
existing id), `GET|PUT|DELETE /{id}` (PUT replaces; `404` unknown id; `428`
PUT without `If-Match`; `412` stale `If-Match`, whose body carries the
current row). `etag` SHALL be the row's `updated_at` (quoted). These routes
are burrow-designed (outside the client contract): error codes are burrow's
own, and unknown fields SHALL be rejected rather than ignored.

### Requirement: Admin authorization

Admin routes SHALL accept either (a) a web/desktop session whose member's
recorded OIDC groups include any of `BURROW_ADMIN_GROUPS`, or (b) an API
token bearing the `llm-admin` scope. `BURROW_ADMIN_GROUPS` defaults to empty,
which closes group-based administration; with no tokens minted and no groups
configured the admin surface serves `403` to everyone. API tokens SHALL be
minted, listed, and revoked only by an existing admin at
`/v1/admin/api-tokens`, SHALL be stored only as SHA-256 hashes, SHALL carry
a name, the scope list, and an expiry (`BURROW_API_TOKEN_TTL`, default 90
days), and every use SHALL update `last_used_at`.

#### Scenario: Machine automation

- **GIVEN** CI holds an `llm-admin` API token
- **WHEN** it sends `PUT /v1/admin/llm-providers/{id}` with a fresh
  `If-Match`
- **THEN** the row is replaced and the audit records the token's name as the
  actor

### Requirement: Admin console surface

The `/admin` shell SHALL be server-rendered pages in burrow's own web app
(embedding the shared design-language tokens, `docs/design-language.md`),
gated by the same decision as the admin routes: a session whose recorded
OIDC groups include any of `BURROW_ADMIN_GROUPS` (or an `llm-admin` API
token, as the write API admits) may see it. With no groups configured the
surface is closed and SHALL answer a plain "not enabled" page, not a 404;
a signed-in non-admin SHALL get a plain refusal page. The index SHALL list
the store's provider rows read-only — display name, id, `baseURL`, the
key-presence light (`hasApiKey`), the row's `groups` (org-wide when null),
and the derived menu's model count ("deriving…" while no menu is cached —
the console is never blocked on a live gateway call) — with no edit
controls: all mutations SHALL go through the admin API. Key values SHALL
never be rendered: the light is the only projection of a key a page may
carry.

#### Scenario: Admin opens the console

- **GIVEN** `BURROW_ADMIN_GROUPS` is configured and a member in one of the
  groups holds a session
- **WHEN** they open `/admin`
- **THEN** they see the provider list with each key shown as a light only,
  and editing a row requires the admin API

### Requirement: Audit

Every create, update, and delete SHALL append a row to `llm_provider_audit`
(actor identity — user id or token name — action, provider id, and the full
before/after row as JSON, timestamped), written in the same transaction as
the change. `GET /v1/admin/audit?limit=` SHALL return them newest-first
(admin-gated) for the console and for incident review. Secret values are
never stored or logged: rows name environment variables, and audit rows
repeat only those names.

### Requirement: Visibility unchanged

Desktop-facing visibility rules (SPEC-0003 REQ "Visibility", "Connect
follows visibility", "Resource snapshot follows visibility") SHALL apply to
store rows unchanged, with a model's audience being its row's audience.
Sign-in SHALL request the `groups` scope unless `BURROW_OIDC_GROUPS_SCOPE` is
`false` (rows are mutable at runtime, so the need can no longer be decided at
boot; ADR-0004).

## Out of scope

The remaining console UI (stump.wtf/burrow#23): the editor and the read-side
pages build on this spec's `/admin` shell (shard 1 of the issue split).
Skills and MCP administration (#12, #4). Skill of the gateway's own admin API
— burrow never calls it. Ansible-side retirement of the providers-file render
(tracked in stump.wtf/burrow#22; the seed makes the transition safe in either
order).
