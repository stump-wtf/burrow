---
status: draft
date: 2026-10-11
implements: [ADR-0003]
---

# SPEC-0003: Org access control (OIDC groups) — providers, models, and the Library

## Overview

Each member sees only the LLM providers, models, and Library skills their OIDC
groups allow, and `/connect` hands a provider's key only to members who can see
it. Groups come from the identity provider's `groups` claim at sign-in. An org
that declares no audiences behaves exactly as the stages before it did.

Provider audiences live where the key's scope is derived from, so the menu and
the key change together: SPEC-0004 moved those declarations from the providers
file onto store rows, and retired model-level groups for derived menus — a
derived model's audience is its row's (ADR-0004).

Library skills have no key to keep in step, so their mapping is edited at
runtime in burrow's own console (#12). Shared MCP connections (#4) follow the
same shape later.

## Requirements

### Requirement: Groups claim

Burrow SHALL request the `groups` scope at sign-in unless the operator opts out
(`BURROW_OIDC_GROUPS_SCOPE`; on by default, because provider rows and Library
rules are edited at runtime and their need cannot be decided at boot). At every
sign-in burrow SHALL record the member's groups from the ID token's `groups`
claim, accepting a JSON array of strings or a single string, and replacing what
was recorded before. Each name SHALL be recorded with surrounding whitespace
removed and once; a name that is empty after that is dropped. Group names are
compared exactly everywhere else, against names that are trimmed when they are
configured, so the recorded claim has to be the same spelling. A missing or
empty claim SHALL record no groups. Groups SHALL be read from that record for
every later request, so a change in the identity provider takes effect at the
member's next sign-in.

#### Scenario: claim recorded at sign-in

- **WHEN** a member signs in with `groups: ["family", "everyone"]`
- **THEN** later requests from that member's session use those two groups

#### Scenario: padded name in the claim

- **WHEN** a member signs in with `groups: ["contractors "]`
- **THEN** the member is recorded in `contractors`, and a Library rule saved
  for the console's `contractors` column applies to them

### Requirement: Group declarations

A provider definition and each of its models MAY carry `groups`, a non-empty
array of non-empty strings. Load SHALL fail on an empty `groups` array or an
empty entry, because "visible to nobody" written as `[]` is indistinguishable
from a mistake.
A row-level entry with leading or trailing whitespace SHALL fail
the same way (SPEC-0004 REQ "Rows in the store"): groups are compared exactly,
so it names an audience no member holds.

### Requirement: Visibility

A model's audience SHALL be its own `groups` when present, else its provider's
`groups` when present, else every member. A member SHALL see a model when the
model has no audience or the member holds at least one of its groups. A
provider row SHALL be served to a member only when the member sees at least one
of its models (a row with no models follows the provider's `groups`), and the
row SHALL list only the models the member sees.

#### Scenario: row-level audience

- **GIVEN** a row with `groups: ["family"]`
- **WHEN** a member without `family` lists providers
- **THEN** the row is absent

#### Scenario: model-level audience

- **GIVEN** a row whose model `m2` has `groups: ["owner"]`
- **WHEN** a member without `owner` lists providers
- **THEN** the row is served without `m2`

### Requirement: Connect follows visibility

`GET /v1/llm-providers/:id/connect` SHALL answer 404, exactly as for an unknown
id, when the member cannot see the row, and SHALL return the key only for a row
the member can see.

### Requirement: Resource snapshot follows visibility

`resources.llmProviders` in `GET /v1/resources` SHALL list only the rows the
member can see, and `resources.marketplaces` SHALL list only the skills the
member can see.

### Requirement: Key-scope warning

Burrow SHALL log a warning at load for each row whose models do not all share
one audience, because the row's single key reaches every model in it: the
narrower models are hidden from listing but not from the key.

## Requirements: the Library

### Requirement: Library audience

The skill catalog is one global, git-sourced list. A member's Library SHALL be
the union of the skills mapped to the groups the member holds that have a rule,
and every surface that serves skills — the Library list, the resource snapshot,
the marketplace and plugin resolutions, the config-object read, the MCP tools,
and the discovery index — SHALL answer from that one slice. A skill the member
may not see SHALL be answered exactly like a skill that does not exist, so the
Library never reveals what someone else has.

#### Scenario: a mapped group sees its pack

- **GIVEN** group `family` is mapped to `code-review`
- **WHEN** a member holding `family` lists the Library, searches, or reads the agent's skill index
- **THEN** `code-review` is present and every other skill is absent from all of them

#### Scenario: two groups union

- **GIVEN** `family` is mapped to `code-review` and `ops` to `deploy-helper`
- **WHEN** a member holding both lists the Library
- **THEN** both skills are present

#### Scenario: hidden looks absent

- **GIVEN** `deploy-helper` is not in the member's allowance
- **WHEN** the member resolves that plugin or calls `get_skill` for it
- **THEN** the answer is the same as for a name that was never in the catalog

### Requirement: Mapping is edited, not deployed

The group-to-skill mapping SHALL live in burrow's store and be editable in the
`/admin` console, gated by the same admin groups as the provider surface.
Membership itself SHALL stay in the identity provider: burrow stores a group's
skill list and never a member list, so adding someone to a pack is adding them
to the group where they already belong. Saving a mapping takes effect on the
next request, because burrow serves the Library itself and no key is involved;
only a change of group membership waits for a sign-in.

A group with no rule and a group whose rule allows nothing SHALL be different
states. A member holding no group that has a rule SHALL keep the whole catalog;
a member whose only group has a rule allowing nothing SHALL see no skills.
Saving SHALL rewrite the groups the page displayed and leave the rest alone.

#### Scenario: switching a group off withdraws its rule

- **GIVEN** group `family` has a rule
- **WHEN** an admin saves with `family`'s switch off
- **THEN** `family` has no rule, and a member whose only group is `family` sees the whole catalog again

### Requirement: Compatibility

With no audience declared anywhere — no provider `groups`, no skill mapping —
every response SHALL be identical to the stages before this spec, and burrow
SHALL ask the issuer for nothing it did not ask for before beyond the `groups`
scope.

## Out of scope

Shared MCP connections by group (#4), and per-member keys minted by burrow
(ADR-0003 option 3).
