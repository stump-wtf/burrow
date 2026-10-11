# burrow

A self-hosted, OpenWork-compatible control plane. Sign in with your own OIDC,
serve your org's skills, and later hand out your own models — from a single Go
binary you run yourself.

**Headscale is to Tailscale as burrow is to OpenWork's cloud backend.** The
OpenWork desktop app is MIT-licensed and can talk to any compatible control
plane; burrow is one you own. No vendor account, no per-seat licensing, and
single sign-on through the identity provider you already run.

## Status

**Stage A and B shipped.** The staged plan (ADR-0001):

| Stage | Scope | Status |
|---|---|---|
| A | OIDC sign-in, org, Library skills, the MCP endpoint that delivers skills to the agent | shipped |
| B | Org LLM providers pointing at a LiteLLM gateway | shipped |
| B+ | Per-member provider access by OIDC group (SPEC-0003; model-level groups retired by SPEC-0004) | shipped |
| B++ | Provider rows in the store, model menus derived from each row's gateway key, admin API (SPEC-0004, ADR-0004) | shipped |
| B+++ | Library skill packs by OIDC group, edited at `/admin/skills` (SPEC-0003) | shipped |
| C | Shared MCP connections | planned |
| D | Automations and Dashboard | planned |

## Administering LLM providers

Provider rows live in Postgres and are what the desktop client lists. Each
row's model menu is **derived**, not stored: burrow asks the row's own gateway
key with `GET {baseURL}/models` and caches the answer for
`BURROW_MODELS_TTL` (60s), serving the last-known menu while a gateway is
unreachable. The menu is therefore the key's scope by construction, and
cannot drift from it (ADR-0004).

Rows are managed at `/api/den/v1/admin/llm-providers` — burrow's own routes,
outside the client contract — which a member of a `BURROW_ADMIN_GROUPS` group
or an `llm-admin` API token may reach. `PUT` requires `If-Match` with the
row's `etag`, so automation cannot clobber a concurrent change, and every
write is audited in `llm_provider_audit`.

`BURROW_PROVIDERS_FILE` is a **first-boot seed**: consulted only while the
table is empty, after which the store is the source of truth and the file is
ignored. Sign-in requests the OIDC `groups` scope unless
`BURROW_OIDC_GROUPS_SCOPE=false` (an issuer that rejects unknown scopes).

## Administering the Library

Library skill packs are managed at `/admin/skills`, gated by the same
`BURROW_ADMIN_GROUPS` groups as the provider surface: a matrix of groups and
skills where a group with no rule leaves its members the whole catalog and a
group with a rule shows its members only the checked skills. The mapping takes
effect on the next request. Membership itself stays in the identity provider —
burrow stores a group's skill list, never a member list — so adding someone to
a pack is adding them to the group where they already belong.

## Pointing the OpenWork desktop app at burrow

Create `~/.config/openwork/desktop-bootstrap.json` on the machine running the
desktop app:

```json
{
  "baseUrl": "https://<burrow host>",
  "requireSignin": false
}
```

The app keeps only the origin; it discovers the API at `<origin>/api/den` on
its own. `http://` is allowed only for loopback (`127.0.0.1` / `localhost`)
hosts, so local development works too.

Signing in opens burrow's page in your browser, authenticates against the
configured OIDC provider, and hands a one-time grant back to the desktop app
via an `openwork://` link (or a grant you paste manually).

## Clean-room statement

burrow is written **clean-room** from the MIT-licensed OpenWork desktop client
(https://github.com/different-ai/openwork). The specification of what the
client sends and reads is [`docs/client-contract.md`](docs/client-contract.md),
derived only from MIT client code. Nothing in burrow is derived from the
EE-licensed cloud backend, its API specification, or its generated SDK — those
sources are never read. Where the client leaves behavior unspecified, burrow
designs it. See ADR-0001.

## Development

Go, standard library first:

```sh
make build   # build ./bin/burrow
make check   # fmt + vet + test + race (the CI gate)
make test
make lint
```

CI runs `make lint` and `make test` plus a secret scan; every check is a
required status before merge.

## License

MIT — see [LICENSE](LICENSE).
