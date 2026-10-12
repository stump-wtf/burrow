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

The store SHALL carry an `llm_providers` table: `id` (the contract's
`/^lpr_/i` prefix followed by 1 to 64 of `[A-Za-z0-9_-]`,
unique), `providerId`, `name`, `baseURL` (absolute http(s), with no query
string, fragment or userinfo), `apiKeyEnv` (an
environment-variable name, never one in burrow's own `BURROW_` namespace), `groups` (NULL for every member; otherwise a
non-empty JSON array of non-empty strings with no leading or trailing
whitespace — an empty array is invalid, per SPEC-0003, and an untrimmed name
would match no member), `nameOverrides` (JSON object, model id to display name),
`created_at`, `updated_at`. Writes SHALL apply the same validation
`providers.Load` applied to the file; an invalid write SHALL be rejected
without touching the served set. The `id` is a URL path segment on
`/connect`, the admin routes and the console, which is why nothing that
would need escaping there is accepted; the rule binds writes, so a row
already stored under an id it refuses SHALL stay readable and deletable.
`baseURL` is an origin and a path only because the row's key is sent to
`{baseURL}/models` and the value is served to every member: `/models` SHALL
be joined to it as a path element, never appended as text.
`providerId`, `name` and group names SHALL NOT contain control characters:
the store cannot hold a NUL, and such a value is the writer's error (`400`),
never a store failure.

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
never-succeeded fetch serves no models. A `200` whose body carries no
`data` array is a fetch failure, not an empty menu; `{"data":[]}` is the
key's real, empty scope. `GET` for the desktop-facing surface
SHALL NOT be blocked on a live gateway call beyond the cached fetch: a row
that already has an answer (a menu, or a recorded failure) SHALL be served
as last known while at most one gateway call per row runs outside the
request; rows with no answer yet MAY be waited for, together and for a
bound well inside the client's 12-second request timeout; and `/connect`
SHALL NOT wait on the gateway at all. A gateway call SHALL NOT be cancelled
by, or count as failed because of, the request that started it. A
model's display name SHALL be its name override when present, else its id.

#### Scenario: Model added in LiteLLM

- **GIVEN** a row whose key's scope gains a model at the gateway
- **WHEN** the row's cache TTL has expired and the refresh the next request
  starts has landed
- **THEN** the row lists the new model and the row's `updated_at` advances,
  so the client's resource snapshot triggers a re-sync

#### Scenario: Gateway unreachable

- **WHEN** a cache refresh fails
- **THEN** the last-known menu is served and the failure is retried after a
  bounded cooldown counted from when the failed call returned, not once per
  request

#### Scenario: Gateway hung

- **GIVEN** a gateway that accepts a row's `GET {baseURL}/models` and never
  answers
- **WHEN** a member lists providers, polls the resource snapshot, or connects
- **THEN** `/connect` answers at once with the menu known so far, and the
  listing and the snapshot answer after at most one bounded wait for all the
  rows that have no answer yet, serving those rows with no models

### Requirement: Admin surface

`/v1/admin/llm-providers` SHALL serve CRUD for rows: `GET` (list, each row
with its stored fields and per-row `etag`), `POST` (create; `409` on an
existing id), `GET|PUT|DELETE /{id}` (PUT replaces; `404` unknown id; `428`
PUT without `If-Match`; `412` stale `If-Match`, whose body carries the
current row). `etag` SHALL be the row's `updated_at` (quoted). These routes
are burrow-designed (outside the client contract): error codes are burrow's
own, and unknown fields SHALL be rejected rather than ignored, as SHALL a
body that carries anything after its one JSON object.

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

### Requirement: Audit

Every create, update, and delete SHALL append a row to `llm_provider_audit`
(actor identity — user id or token name — action, provider id, and the full
before/after row as JSON, timestamped), written in the same transaction as
the change. `GET /v1/admin/audit?limit=` SHALL return them newest-first
(admin-gated) for the console and for incident review. Secret values are
never stored or logged: rows name environment variables, and audit rows
repeat only those names.

### Requirement: Admin console

Burrow SHALL serve a server-rendered console under `/admin` (no JS build
step; the shared design tokens) for provider rows, gated by the API's own
admin check: a session whose member holds any of `BURROW_ADMIN_GROUPS`
(closed to everyone when unset). The console SHALL offer: a provider list
(name, baseURL, key presence, audience, model count), a row editor for
create/edit/delete (per-model display-name overrides on the derived menu,
row-level `groups`, and the `apiKeyEnv` NAME with a set/unset light - a key
VALUE is never rendered, accepted, or stored), a visibility preview (pick a
group; exactly the rows a member of it would get, computed by the runtime's
own SPEC-0003 group matching), and an audit view (actor, action, provider,
before/after detail). Every console mutation SHALL go through the write
API's own handlers - its validation, its `If-Match` etag handling (412 shows
the carried current row; 409 surfaces duplicate_id; 400 shows the field
errors) and its audit records - so the console is a client of the API, never
a second management path. A web (non-desktop) OIDC sign-in SHALL establish
the browser session the console reads: an HttpOnly, SameSite=Lax cookie
carrying the session token (the desktop grant flow is unchanged). The
console SHALL say that audience edits take effect at each member's next
sign-in, and SHALL warn when a row's audience is null (visible org-wide);
key rotation stays an OpenBao/LiteLLM operation outside burrow.

#### Scenario: Edit races another admin

- **GIVEN** one admin editing a row while another admin saves it
- **WHEN** the first admin saves with the stale etag
- **THEN** the editor re-renders with the API's current row and an
  explanation, and no change is lost silently

#### Scenario: Console closed by default

- **GIVEN** a deployment with `BURROW_ADMIN_GROUPS` unset
- **WHEN** any member opens `/admin`
- **THEN** the console answers 403, exactly like the API

### Requirement: Visibility unchanged

Desktop-facing visibility rules (SPEC-0003 REQ "Visibility", "Connect
follows visibility", "Resource snapshot follows visibility") SHALL apply to
store rows unchanged, with a model's audience being its row's audience.
Sign-in SHALL request the `groups` scope unless `BURROW_OIDC_GROUPS_SCOPE` is
`false` (rows are mutable at runtime, so the need can no longer be decided at
boot; ADR-0004).

## Out of scope

The console UI was in scope of this spec as of 2026-10-11: see REQ "Admin console"
(stump.wtf/burrow#23). Skills and MCP administration (#12, #4). Skill of the gateway's own admin API
— burrow never calls it. Ansible-side retirement of the providers-file render
(tracked in stump.wtf/burrow#22; the seed makes the transition safe in either
order).
