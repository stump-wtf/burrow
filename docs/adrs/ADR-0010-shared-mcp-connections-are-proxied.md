---
status: proposed
date: 2026-10-29
decision-makers: [Joe Stump (pending review)]
related: ADR-0001 (staging A → B → C → D), ADR-0002 (the store is Postgres), ADR-0003 (access by OIDC groups), ADR-0005 (what a row may name), ADR-0004 (a cached menu, reused here), SPEC-0003 REQ "Visibility", stump.wtf/burrow#4
---

# ADR-0010: Shared MCP connections are burrow-proxied, with credentials sealed in the store

## Context and Problem Statement

`docs/client-contract.md` ("Stage C") tells the desktop client to list an org's
MCP connections and explicitly **not** to register them as local MCP entries:
the agent reaches them through burrow's own `search_capabilities` /
`execute_capability`. That makes burrow an MCP client as well as an MCP server,
and it leaves four things the contract does not answer, each of which the Stage
C code has to have an opinion about:

- Where a credential lives. A connection has to be usable again after a restart,
  and burrow has to *present* it upstream, so hashing it the way a session token
  is hashed is not enough. The contract names two modes — `credentialMode:
  shared | per_member` — and issue #4's 2026-10-10 sequencing note says the
  `per_member` half anticipates the group work in #12/#13, so the shape chosen
  now should not have to be retrofitted.
- How an upstream server is addressed. A connection row is data an operator
  edits, and burrow will send a stored credential to whatever URL it names.
- What the OAuth connect looks like when the client's only moves are "open this
  URL" and "poll the list", and whose credential an OAuth token lands on for a
  `shared` connection.
- How `connected`, `connectedForMe` and `connectedAt` are computed, and how a
  tool list survives an agent searching on every prompt.

## Considered Options

**Credentials.**

- *Environment only.* Zero secrets in the database. It cannot hold a
  per-member key or an OAuth token at all, so it cannot implement the contract's
  `per_member` mode or the OAuth dance.
- *Hashed, like a session token.* Then burrow cannot present the credential and
  the proxy is impossible.
- *Plaintext columns.* One read of the database — a backup, a replica, a
  `SELECT` in a debugging session — hands out every upstream credential the org
  has.
- *Sealed (this decision).* A shared API key stays where the deployment keeps
  secrets and is named by variable, exactly as ADR-0005 handles a provider row's
  `apiKeyEnv`; anything burrow must store — a member's own key, an OAuth
  access and refresh token — is sealed with AES-256-GCM under
  `BURROW_MCP_CREDENTIAL_KEY` (from OpenBao in deployment), next to a SHA-256
  fingerprint that says *which* credential a row holds without being it.

**Upstream addressing.** Free-form URL per row; or a fail-closed host allowlist
checked at write time; or a host allowlist checked at write time **and** on
every outbound call (this decision). The middle one is what ADR-0005 does for a
provider row, but a provider row's URL is only fetched by an admin's save; a
connection's URL is fetched on an agent's tool call, so a row edited afterwards,
or a database restored from before a tightening, would ship a live credential to
the new host.

**The connect.** Redirect the browser back into burrow's session-authenticated
console; or a single-use state that is itself the proof (this decision), the way
the desktop handoff's grant already works. The client's two moves are "open the
URL" and "poll", and a browser round trip through an external authorization
server is exactly where a session cookie is least reliable.

**Tool discovery.** Ask every connected upstream server on every
`search_capabilities`; or cache per (connection, credential fingerprint) for a
window (this decision), which is ADR-0004's shape applied to a different menu.

## Decision Outcome

- `mcp_connections` holds the connection and **no secret**: `url`, `auth_type`,
  `credential_mode`, `expose_directly`, `api_key_env`, the `mcp_groups` audience
  in SPEC-0003's shape, and the OAuth endpoints and client id. `mcp_credentials`
  holds one row per credential, keyed by `(connection_id, member_id)` where
  `member_id = ''` is the org's shared slot, with `credential_hash`,
  `credential_sealed` and `refresh_sealed`.
- burrow is a **public PKCE client** towards an upstream authorization server.
  It carries no client secret for any connection, so writing a connection row
  never means pasting a credential into one.
- `GET /v1/mcp-connections/:id/connect/start` either answers `connected`, or
  hands back the `authorizeUrl` — and for an `apikey` connection in
  `per_member` mode, `needs_auth` with an empty URL, which is the cue for the
  client to prompt for a key. The poll is the only completion signal, so the
  browser's callback (`GET /mcp-connections/:id/oauth/callback`) is keyed on the
  single-use state alone and consumes it; it is deliberately outside
  `requireSession`, and an unknown, used, expired or forged state gets the same
  one-sentence page.
- An OAuth token an org member finishes with is stored **in the slot the
  connection's mode names**: `shared` connections have one org-wide token, which
  is whichever member signed in last, and `per_member` connections have one per
  member. `POST /v1/oauth-providers/:id/disconnect` clears the shared slot and
  is an admin route; `POST /v1/mcp-connections/:id/disconnect-my-account` clears
  only the caller's own row and refuses a shared connection, because there it
  would sign everyone else out.
- A tool is addressed as `mcp:<connection id>:<tool name>`, the same colon-form
  namespace as `skill:<name>` and `plugin:<id>:<id>`. Whether a member may see
  or run it is decided by the connection's `mcp_groups` audience, in the proxy
  path exactly as in the listing, so a connection cannot be reached over MCP
  when it is invisible on the list. The same audience gates the three
  member-facing routes (`connect/start`, `my-credential`,
  `disconnect-my-account`): managing a connection does not let an admin start a
  connect on, or run a tool of, one it is not addressed to. The admin surface
  reads and writes the row under the admin gate instead.
- `connected` is what the org has (an `authType: none` connection is always
  connected; a shared one is connected when burrow can present its credential; a
  per-member one is connected when somebody's unexpired credential exists), and
  `connectedForMe` is whether *this* member may use it right now — so it is
  false for a member outside the audience and for a per-member connection they
  have not personally connected. `connectedAt` is their own credential's
  timestamp, falling back to the org's.
- An upstream tool list is cached per (connection, credential fingerprint) for
  `BURROW_MCP_TOOLS_TTL` (60s) and dropped the moment an admin edits or deletes
  that connection.
- Every connection's URL, authorize URL and token URL pass
  `BURROW_MCP_URL_HOSTS` at write time and again on every request. A shared key
  may be named only by a variable under `BURROW_MCP_API_KEY_ENV_PREFIXES`.
- Upstream trouble is reported as upstream trouble: a refused credential is
  "connect it again" (`-32001`), an unknown or unaddressable connection is
  "unknown capability" (`-32002`), and a server that answers with its own
  JSON-RPC error is `-32003` carrying that server's sentence, clamped, while a
  transport failure is `-32003` carrying burrow's. One that will not answer a
  `tools/list` simply contributes nothing to the search rather than failing it.

## Consequences

A stolen database is a set of ciphertexts plus fingerprints, and a server
started without `BURROW_MCP_CREDENTIAL_KEY` refuses to store a credential at all
(503) instead of quietly writing one in the clear. The cost is a new deployment
secret, and a rotation that invalidates every stored member key and refresh token
until members reconnect.

A shared OAuth connection has one token for the whole org, so the member who
signs in last is the identity every member's tool call carries. That is what
`credentialMode: shared` means in the contract; the audience keeps it from being
someone else's org-wide secret. Per-member attribution over a shared connection
is issue #13's key-gating question, not this one's.

The cache puts at most one upstream `tools/list` per connection per window in
front of an agent's search, and makes an operator's edit visible within one
request. The cost is that a server whose tools change on their own is seen on
the window's expiry, not instantly.

Fail-closed `BURROW_MCP_URL_HOSTS` means a deployment with the variable unset
has connections that are listed and never reachable, which is the intended
default: the flag to turn on is an explicit allowlist, not an empty string.

## More Information

- Issue: <https://gitea.stump.rocks/stump.wtf/burrow/issues/4>
- Contract: `docs/client-contract.md`, "Stage C: shared MCP connections"
- Code: `internal/upstream` (the client, the policy, the OAuth calls and the
  cache), `internal/seal` (the sealer), `internal/store/mcpconnections.go`,
  `internal/httpapi/mcpconnections.go` and `mcpproxy.go`.
- Sequencing: Stage C's visibility semantics follow SPEC-0003 so the group work
  in #12/#13 extends to MCP connections without a second seam, and its storage
  is Postgres per ADR-0002.
