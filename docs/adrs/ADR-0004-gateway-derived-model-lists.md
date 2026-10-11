---
status: accepted
date: 2026-10-10
decision-makers: [Joe Stump]
related: ADR-0001 (org LLM providers), ADR-0003 (provider access by OIDC groups), ADR-0002 (Postgres store), SPEC-0002 (stage-b-org-models), SPEC-0003 (org-access-control), SPEC-0004 (provider-admin)
---

# ADR-0004: The model menu is derived from the gateway; provider rows live in the store

## Context and Problem Statement

ADR-0003 made provider and model visibility per-member, but kept the model
list itself in `BURROW_PROVIDERS_FILE`: burrow lists exactly the models the
deployment wrote into the file, and the same converge mints each row's
LiteLLM virtual key scoped to exactly that list. Writing providers was
therefore excluded from the planned admin console — the console could change
the menu but not the key, and menu-vs-key drift was the stated reason to keep
providers in the file (stumpcloud/ansible `gva.yaml` +
`playbooks/services/burrow.yaml` being the only writer).

Issue stump.wtf/burrow#22 asked for a write API so automation can manage
providers. That surfaces the drift question directly: an API that writes a
model list burrow then lists, while the key's allowlist lives in LiteLLM,
reintroduces exactly the drift ADR-0003 rejected the console for. Reconciling
the two would mean burrow holding a LiteLLM admin credential — ADR-0003's
rejected option 3.

Joe's observation dissolves the problem: burrow's menu is not the gate. The
key is. The desktop client takes the row's key from `/connect` and calls the
gateway itself; LiteLLM already answers "what can this key use" at
`GET /v1/models`. If burrow derives each row's menu from the row's own key,
the menu is the key's scope by construction — no reconciliation, no drift, no
admin credential, and adding a model to the org picker becomes an ordinary
LiteLLM operation.

## Considered Options

1. **Reconciliation**: the write API edits a stored model list and also
   updates the row's LiteLLM key allowlist through the gateway's admin API.
   Menu and key agree at the cost of burrow holding a credential that can
   mint and widen any key.
2. **Gateway-derived menus** (chosen): the store keeps the row — identity,
   `baseURL`, `apiKeyEnv`, audience — and the model list comes from the row's
   own key calling `GET {baseURL}/models`, cached. Burrow stores only display
   name overrides.
3. **Status quo**: no write API; the inventory file stays the only writer.
   Keeps the audience-changes-need-a-converge cost ADR-0003 already lists as
   its main drawback, and gives automation nothing.

## Decision Outcome

Chosen: **option 2**.

- The Postgres store's `llm_providers` table is the source of truth for
  provider rows (id, providerId, name, baseURL, apiKeyEnv, `groups`, name
  overrides). `BURROW_PROVIDERS_FILE` becomes a first-boot seed, consulted
  only when the table is empty, so an existing deployment adopts this with no
  behavior change and no data migration.
- A row's menu is its key's view of itself, cached per row (default 60s TTL,
  serve-stale-on-error). When the derived set changes, the row's `updated_at`
  advances so the client's resource-snapshot poll notices the new menu — the
  same mechanism ADR-0003's file mtime provided, now event-driven.
- Per-MODEL `groups` (SPEC-0003) are superseded for derived menus: a derived
  model has no audience beyond its row's, and the enforceable pattern was
  already one row per audience (ADR-0003's key-scope warning). A seed file
  carrying model-level groups applies the row audience and warns at boot.
  `MixedAudienceRows` retires with this ADR.
- Audience (`groups`) stays burrow's, in the store, edited through the admin
  API. Enforcement of WHO may use a model remains LiteLLM's (the key), and of
  WHICH key a member receives remains burrow's (the groups gate on
  `/connect`). The two halves still cannot drift because one row is one key
  and the menu is that key's scope.

### Consequences

* Good: adding or removing a model in the org's picker is a LiteLLM edit;
  burrow shows it within one cache TTL without a converge, a restart, or any
  burrow-side write at all.
* Good: the write API covers exactly the state burrow owns (rows, audiences,
  name overrides) and needs no gateway credential.
* Good: menu-vs-key drift is structurally impossible, not merely policed.
* Bad: the menu depends on the gateway being reachable at least once per row;
  a gateway outage freezes menus at their last known state (burrow never
  blocks `/connect` on the gateway).
* Bad: model display names need overrides (the OpenAI-compatible listing
  returns ids); unlisted ids display as their id.
* Neutral: rows become mutable at runtime, so sign-in can no longer decide at
  boot whether to request the `groups` scope; burrow requests it always
  (Pocket ID accepts it; an issuer that rejects unknown scopes opts out with
  `BURROW_OIDC_GROUPS_SCOPE=false`, replacing SPEC-0003's
  declare-in-the-file condition).

## More Information

Requirements: `docs/openspec/specs/provider-admin/spec.md` (SPEC-0004);
amends SPEC-0002 REQ "Provider definitions"/"Provider rows" and SPEC-0003 REQ
"Group declarations" (model-level `groups`) and REQ "Compatibility".
Issue: stump.wtf/burrow#22.
