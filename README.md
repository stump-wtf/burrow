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
| B | Org LLM providers pointing at a LiteLLM gateway | shipped (unconfigured; set `BURROW_PROVIDERS_FILE` to enable) |
| C | Shared MCP connections | planned |
| D | Automations and Dashboard | planned |

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
