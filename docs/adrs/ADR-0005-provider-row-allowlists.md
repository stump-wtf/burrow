---
status: proposed
date: 2026-10-16
decision-makers: [Joe Stump]
related: ADR-0001 (clean-room control plane), ADR-0003 (provider access by OIDC group), ADR-0004 (gateway-derived model menus), SPEC-0004 (provider-admin), #24, #25, #36
---

# ADR-0005: A provider row may name only an allowlisted key variable and an allowlisted gateway host

## Context and Problem Statement

A provider row carries two choices that burrow acts on with its own
privileges: `apiKeyEnv`, the environment variable whose value becomes that
row's bearer token, and `baseURL`, the host that token is sent to. Since
ADR-0004 made rows writable at runtime, whoever can write a row chooses both.

`validateIdentity` only excluded burrow's own `BURROW_` namespace (#25, which
deferred the `baseURL` half), so any other variable was nameable and any
absolute http(s) URL was a host. On the next member poll of `/v1/resources`
or `/v1/llm-providers` burrow read the named variable and sent its value as
`Authorization: Bearer …` to `{baseURL}/models`; `/connect` handed the same
value to the member. One `POST /api/den/v1/admin/llm-providers` with an
`llm-admin` API token, or a forged console POST (the CSRF gap is tracked
separately), was therefore "read environment variable X and send its value to
host Y", with no session, no `/connect` and no member cooperation needed.
The reviewer reproduced it on `main` at 832bca5 with two admin writes and one
member GET: a capture server received `PGPASSWORD` and another row's key as
bearers. Today's concrete loss is every provider key in the container's
environment, including the keys behind group-restricted rows (#36).

Neither half is enough alone. Prefixing the key variables still lets a row
name a *different* provider key and aim it anywhere; allowing only known hosts
still lets a row name `PGPASSWORD` and send it to a host the deployment does
use. So this is a pair of allowlists, enforced twice: at the write, and again
wherever burrow uses a row.

## Decision Drivers

* Keys stay out of the store (ADR-0001, ADR-0004): they arrive from OpenBao in
  the container's environment and are read at request time. Anything that kept
  the exfiltration path open by storing keys instead would need a secret-at-rest
  design this repo does not have.
* Rows must stay editable without a redeploy, and the deployment must not have
  to list every variable name: rows in production name `LITELLM_BURROW_KEY` and
  its per-row suffixed variants, all rendered by the same Jinja template.
* A row written before the allowlist existed is still a row, so the check
  cannot live only in the write path.
* New configuration converges in `stumpcloud/ansible` (which renders
  `burrow.env`) before the image that reads it, or burrow crash-loops. So this
  change must not make a missing variable a boot failure.

## Considered Options

1. **Extend the denylist.** Add `PGPASSWORD` and whatever else the container
   carries to the refused-name list. Rejected: it is a list of secrets nobody
   maintains, and it says nothing about the host.
2. **Allowlist the key variable only.** Rejected: a row can still name another
   row's key, which is the loss this issue is actually about.
3. **Allowlist the host only, and validate it at the write.** Rejected: the
   rows already in the store were never validated against it, and an operator
   can repoint a row at a host that is gone from the list.
4. **Store the keys instead of naming them.** Rejected for now: it needs
   encryption at rest and a key for that, which is its own decision, and it
   would still leave `baseURL` unconstrained.
5. **Allowlists on both, enforced at the write and at use, fail closed on an
   unset list.** Chosen.

## Decision Outcome

Both dimensions are allowlists, built from two new variables and applied by
`providers.Policy`:

* `BURROW_PROVIDER_API_KEY_ENV_PREFIXES`, comma list, default `LITELLM_`. A
  row's `apiKeyEnv` must start with one of these prefixes, compared
  case-insensitively. The `BURROW_` namespace stays refused on top of that:
  a prefix must not be able to re-open burrow's own variables.
* `BURROW_PROVIDER_BASE_URL_HOSTS`, comma list, **default empty, which allows
  no host at all**. An entry is a lowercase host, optionally with a
  non-default port; the scheme's default port is implied, a non-default port
  must be written, and a scheme or path in the entry is ignored. Exact hosts
  only: no wildcard suffixes, which would turn the list back into "any host
  under a domain someone else can buy".

Enforcement points, in the order that closes the chain:

* the admin write (`POST`/`PUT`, and the console that drives it) refuses a row
  outside either list with `400 invalid_row`, naming the variable to change;
* `lookupKey` reads nothing for a name outside the prefix list, so
  `hasApiKey` is false and no fetch starts for that row;
* `fetch` re-checks the host before it dials, so a row that predates the list
  sends no bearer anywhere;
* the menu cache clears a row whose source is no longer allowlisted instead of
  serving its last-known menu, and `/connect` answers such a row exactly like
  one the member cannot see, so no key and no existence is revealed.

**Why `LITELLM_` is the default and not empty.** An empty prefix list allows
nothing, and a deployment that forgot the variable would then serve no models
at all with only a log line to say so. `LITELLM_` is what the inventory renders
today, so the default matches the working configuration and an operator who
names other variables sets the variable deliberately. The host list has no
usable default: burrow cannot guess which LiteLLM the org means, and a guessed
default would be exactly the "host of the writer's choosing" this closes. An
unset host list therefore allows nothing, and burrow says so once at boot as
well as once per row per TTL.

**Rollout order** (the same order any new `BURROW_*` variable needs, per
`AGENTS.md`):

1. `stumpcloud/ansible` sets both variables for the burrow container
   (`BURROW_PROVIDER_BASE_URL_HOSTS` to `litellm.stump.rocks`, the prefixes to
   the default) and converges. Until it converges the rows simply serve no
   models; nothing crash-loops.
2. This image merges. The rows already fit, so nothing changes for members.

Seeding is the one path that does not refuse: the providers file is the
deployment's own rendering, so a row there that trips an allowlist is logged at
Warn, seeded anyway, and serves nothing until either side is corrected.
Refusing to boot over a list that has not converged would take the whole
control plane down for a mistake in one row.

A row outside either list stays listed. It has no models, `hasApiKey` is false,
the console's menu reads "unknown", and it is inert. Dropping it from the
listing would hide from the admin exactly the row they have to fix.

### Consequences

* A refused write and a refused fetch are logged at Warn with the row id, the
  host or variable name, and the reason. Never a value. They do not reach the
  store's audit table, which records writes the store applied; a refused write
  is not one of those.
* The log line appears at most once per row per `BURROW_MODELS_TTL`, so a row
  nobody corrects does not repeat on every poll.
* `BURROW_PROVIDER_BASE_URL_HOSTS` becomes required in practice for any
  deployment that serves models. That is the point: without it a written row
  can reach nothing.
* A row whose host is on the list but whose key variable is on the list too is
  unchanged in behavior, which is what makes the change safe to merge over live
  rows.

## More Information

* #24 is the token-leak retrospective that found this; #25 closed the `BURROW_`
  prefix and deferred `baseURL`; #36 is this report.
* The console's CSRF gap (a forged POST needs no token) is being fixed
  separately; this ADR removes what a successful write could reach.
* Enforcement lives in `internal/providers/policy.go`.
