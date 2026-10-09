# burrow: OpenWork Desktop Client Contract

> **Provenance.** Verbatim copy of the client contract artifact
> `mcp://cairn/s4vJe24V` (https://cairn.stump.wtf/s4vJe24V), written only from
> the MIT desktop client of https://github.com/different-ai/openwork at
> upstream `dev` commit `70c948c`. This file is burrow's specification; see
> ADR-0001 for the clean-room rules that govern how it may be used and
> extended. Do not edit this copy to describe burrow's choices — record those
> in ADRs and specs instead, and re-copy from the source artifact if the
> contract is ever revised.

This document describes what the OpenWork desktop app (MIT) sends to an organization control plane, and what it reads back. burrow must answer these requests.

## Provenance and clean-room rule

- **Source.** Derived only from MIT-licensed client code in https://github.com/different-ai/openwork at upstream `dev` `70c948c`: `apps/app/src`, `apps/desktop/electron`, `apps/server/src`, and `packages/*`. Every `file:line` below refers to that tree.
- **Excluded.** Nothing here comes from `ee/` (the EE-licensed "Den" backend) or from `packages/docs/openapi.json`, whose metadata declares the EE license.
- **Rule for anyone building burrow:**
  - Do not open `ee/` or `openapi.json`.
  - Where the client leaves something unspecified, design it yourself.

## Discovery

- **Base URL** comes from `~/.config/openwork/desktop-bootstrap.json`. The app keeps only the origin (`baseUrl`).
  - http is allowed only for loopback (`apps/app/src/app/lib/organization-server-input.ts`).
- **API base** for a non-`*.openworklabs.com` origin defaults to `<origin>/api/den` (`den.ts:790-849`). Every `/v1/...` route below lives under the API base.
- **`GET <origin>/api/runtime-config`** (optional): the client reads only `denApiUrl`, with a 2-second timeout (`den.ts:949-1013`). Return `{"denApiUrl": "https://<host>/api/den"}`.

## Authentication

### 1. Browser sign-in

The desktop opens `<origin>/?mode=sign-in|sign-up&desktopAuth=1&desktopScheme=openwork` (`den.ts:1396-1415`). burrow's page at `/` is burrow's own design: authenticate the user with Pocket ID OIDC, then:
- redirect to `openwork://den-auth?grant=<grant>&denBaseUrl=<origin>`;
- **always include `denBaseUrl`**;
- also show the grant as copyable text, because the client accepts a pasted link or a raw grant of 12+ characters (`manual-auth-input.ts:9-36`).

### 2. Exchange

POST `/v1/auth/desktop-handoff/exchange` with body `{grant}` and **no auth header**.
- **Response:**
  - `token` (required);
  - `user{id,email,name?}`;
  - `organization{id,slug?,name?}` (optional);
  - `connectEnabled` (optional).
- The client parsers are at `den.ts:1796-1843`.
- Make the grant **single-use** and short-lived. Its format is opaque to the client.

### 3. Session

- Every call sends the session token in the `Authorization` header, using the Bearer scheme.
- Org-scoped calls also send `x-openwork-org-id` and `x-openwork-legacy-org-id`.
- No cookies; the timeout is 12 seconds (`den.ts:2747-2794`).
- **Error body:** `{error:<code>, message?, details?}`.
- **No refresh endpoint.** The client polls GET `/v1/me`.
- **A 401 signs the user out only when `error` is one of:** `unauthorized`, `invalid_session`, `session_expired`, `session_revoked`, `session_not_found`, `invalid_token`, `token_expired`, `token_revoked` (`den.ts:521-547`). Anything else reads as "unavailable", and the token is kept.

### 4. Sign-out

- POST `/api/auth/sign-out` with body `{}`, best effort.
- On desktop the call goes to `<api base>/api/auth/sign-out` (`den.ts:1417-1422,2872-2878`).

### 5. Pre-provisioning (optional)

- A `handoff.grant` in `desktop-bootstrap.json` is exchanged at boot.
- Minting: POST `/v1/auth/desktop-handoff {desktopScheme:"openwork"} → {grant}` (`packages/openwork-bootstrap/bin/openwork.mjs:970-979`).

## Stage A: sign-in, org and Library skills

| Method | Path | Read by the client |
|---|---|---|
| GET | `/v1/me` | `user.{id,email,name}` |
| GET | `/v1/me/orgs` | `orgs[].{id,name,slug,role}` (all four required, or the row is dropped), `activeOrgId`, `activeOrgSlug` |
| POST | `/v1/me/active-organization` | body `{organizationId?, organizationSlug?}`; response ignored |
| GET | `/v1/me/desktop-config` | `{}` is valid. **Return `connectEnabled: true`**, or the agent gets no remote skills (`openwork-extensions-preview-steering.ts:346`). Also `automationsEnabled`, `dashboardEnabled` (false for now). Normalized by `packages/types/src/den/desktop-policies.ts:374-390,759-794` |
| GET | `/v1/resources` | `{organizationId, orgMemberId, teamIds[], resources{llmProviders{id:ts}, marketplaces{id:{lastUpdatedAt, plugins[]}}}}` (`den.ts:640-657`) |
| GET | `/v1/org` | `currentMember.id`, `members[].{id,user{name,email}}`, `teams[].{id,name,memberIds[]}` |
| GET | `/v1/me/library` | `items[]` (`den-library.ts:129-162`) |
| GET | `/v1/resources/marketplace-capabilities` | `items[].{configObjectId, marketplaceId\|null, objectType, pluginId}` |
| GET | `/v1/marketplaces?status=active&limit=100` | `items[].{id,name,description,status,pluginCount,updatedAt}` |
| GET | `/v1/marketplaces/:id/resolved` | see `den.ts:2539-2605` |
| GET | `/v1/plugins/:id/resolved` | see `den.ts:2189-2224,2573-2615` |
| GET | `/v1/plugins/:id/config-objects` | `items[].{configObjectId, configObject{id,objectType,title,description,latestVersion{rawSourceText}}, removedAt?}` |
| GET | `/v1/config-objects/:id/versions/latest` | `item\|root {id, rawSourceText}` |
| GET | `/v1/llm-providers` | stage A: `{"llmProviders": []}` (it is polled) |
| GET | `/v1/mcp-connections?scope=usable` | stage A: empty list |
| GET | `/v1/inference-providers?scope=usable` | **404**: the client treats it as "none" |

**Responses the client tolerates as errors:** `/v1/app-version`, `/v1/telemetry/ingest`, `/v1/inference/analytics/*`, `/v1/gateway/usage-limits/me`, and `/v1/install-config` are optional. The client swallows errors from them.

### The MCP endpoint (how the agent gets skills)

- **Minting the MCP credential:** the client sends a POST to `/v1/mcp/token {scopes:["mcp:read","mcp:write"]}` with org headers (`den.ts:3022-3034,1932-1954`).
  - Response: `{token, expiresAt, organizationId, resource}`.
  - **The MCP URL is `<resource>/agent` and must end in `/mcp/agent`** (`cloud-mcp-reconciler.ts:179-184`; `cloud-mcp-health.ts:694`).
- **Configuration:** the local server writes a remote MCP entry whose `Authorization` header carries the MCP token (Bearer scheme), with `oauth:false`. No MCP OAuth or `.well-known` discovery is needed.
  - It re-mints on expiry, or on error codes such as `invalid_mcp_token` or `missing_mcp_token`, or any code containing "expired" or "auth" (`cloud-mcp-reconciler.ts:219-257,305-407`).
- **Transport:** Streamable HTTP with JSON or SSE responses.
  - `initialize` returns `protocolVersion` `2025-06-18` or `2025-03-26`, and optionally an `mcp-session-id`.
  - `notifications/initialized` returns **HTTP 202 with an empty body** (`apps/server/src/connect-mcp-transport.ts:96-147`).
- **Tools:**
  - `search_capabilities` and `execute_capability` are required by name (`cloud-mcp-health.ts:25-30`); `get_skill` is optional.
  - Their argument and result schemas are **not** in client code. Design them yourself; MCP servers describe their own tools.
- **Resource `skill://index.json`:**
  - `$schema: https://schemas.agentskills.io/discovery/0.2.0/schema.json`
  - `skills[{name (kebab-case), type:"skill-md", description, url:"skill://…", capability:"skill:<x>"|"plugin:<pluginId>:<configObjectId>", title?, marketplaceName?, pluginName?}]`
  - Source: `connect-skill-catalog.ts:15-33`. It is injected into the agent's prompt (`:142-191`).
- **Optional resource:** `automation://index.json` (`connect-automation-catalog.ts:11`).

## Stage B: org models (LiteLLM)

| Method | Path | Notes |
|---|---|---|
| GET | `/v1/llm-providers` (and `?scope=usable`) | `llmProviders[].{id,providerId,name,source,providerConfig,hasApiKey,models[{id,name,config}],updatedAt}` |
| GET | `/v1/llm-providers/:id/connect` | `llmProvider{…, apiKey \| apiKeys{ENV:value}}` |

**Strict rules the local server enforces.** One bad row fails the whole sync (`apps/server/src/cloud-provider-sync.ts:304-357`):
- `id` matches `/^lpr_/i`;
- `source` is `models_dev`, `custom` or `openwork`. **Use `custom`**, because `openwork` sets OpenWork-hosted env vars (`:653-657`);
- every model has `id`, `name` and `config` (an object or a JSON string).

`providerConfig` is copied into the OpenCode config (`:661-702`). Example for LiteLLM:

```json
{"npm":"@ai-sdk/openai-compatible","options":{"baseURL":"https://litellm.stump.rocks/v1"},"env":["LITELLM_API_KEY"]}
```

Credentials come from `apiKey`/`apiKeys` on `/connect` and are written into env (`:642-660`).

## Stage C: shared MCP connections (later)

- GET `/v1/mcp-connections?scope=usable|manageable`
  - `connections[].{id,name,url,authType(oauth|apikey|none),credentialMode(shared|per_member),exposeDirectly,connected,connectedAt,connectedForMe}` (`den.ts:2065-2111`)
- GET `/v1/mcp-connections/:id/connect/start → {status: connected|needs_auth, authorizeUrl}`. The client opens the URL in a browser, then polls the list.
- PUT `/v1/mcp-connections/:id/my-credential {apiKey}`
- POST `/v1/mcp-connections/:id/disconnect-my-account`
- POST `/v1/oauth-providers/:id/disconnect`
- Connections execute through burrow's MCP endpoint (`search_capabilities` / `execute_capability`), **not** as local MCP entries (`use-org-mcp-connections.ts:130-135`).

## Stage D: Automations and Dashboard (later)

- The routes are listed at `den.ts:2978-3345`.
- The runner protocol is in `apps/desktop/electron/automation-runner.mjs`: a work poll every 60 seconds, then claim, heartbeat, events and complete.
- The client mints its runner credential with a POST to `/v1/automation-runners/token`, which returns `{token, expiresAt}`.
  - The token is `base64url(JSON{v:2, a:<apiBaseUrl>, …}).<sig>`.
  - The client checks only that `a` equals the API base; it does not verify the signature (`automation-runner.mjs:982-1015`).
- Schemas: `packages/types/src/automations.ts`, `workflows.ts`.

## What a third-party server cannot do (or must work around)

- **Signed connect links** (`openwork://connect` whose `token` query parameter is a JWS) need a vendor public key baked into the build (`connect-link-keys.mjs:15-19`).
  - Use the keyless form instead: `openwork://connect?code=…&apiBaseUrl=…`, which calls POST `/v1/install-connect/{preview|exchange}` (`connect-link.mjs:332-407`).
- **The MCP App host and directly exposed connections** require burrow's origin to equal `enterpriseActivation.denBaseUrl` in `desktop-bootstrap.json` (https), or a `*.openworklabs.com` origin (`enterprise-den-origin.ts:42-70`).
  - That's a manual edit, not a signature.
  - The core MCP endpoint does **not** need it (`routes/cloud-mcp.ts:184-189`).
- No license checks were found in the client.

## Open questions (design them, don't copy them)

- The grant format and the web sign-in page.
- The `search_capabilities` / `execute_capability` schemas.
- Where Library content lives. Proposal: a Gitea repo of skills, e.g. `stumpcloud/skills`.
- The org and member model. Proposal: groups from Pocket ID.
- Skill hubs, which the client doesn't reference.
