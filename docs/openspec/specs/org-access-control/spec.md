---
status: draft
date: 2026-10-11
amended:
  - 2026-10-16 (ADR-0006, stump.wtf/burrow#39: the Library fails closed on group drift)
  - 2026-10-25 (ADR-0009, stump.wtf/burrow#13: model audiences inside a row)
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
file onto store rows, and retired model-level groups for derived menus, so a
model that declares nothing has its row's audience (ADR-0004). A model that
wants a different audience from its row declares one, in the store and in the
console, not as a field of the row (ADR-0009, REQ "Model audience").

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

A model's audience SHALL be its own declared audience when present (REQ "Model
audience"), else its provider's `groups` when present, else every member. A
member SHALL see a model when the model has no audience or the member holds at
least one of its groups. A
provider row SHALL be served to a member only when the member sees at least one
of its models (a row with no models follows the provider's `groups`), and the
row SHALL list only the models the member sees.

#### Scenario: row-level audience

- **GIVEN** a row with `groups: ["family"]`
- **WHEN** a member without `family` lists providers
- **THEN** the row is absent

#### Scenario: model-level audience

- **GIVEN** a row whose model `m2` declares the audience `["owner"]`
- **WHEN** a member without `owner` lists providers
- **THEN** the row is served without `m2`

### Requirement: Model audience

A model whose audience differs from its row's SHALL declare it in the store, per
provider row and model id, and the declaration SHALL be editable as a matrix of
the models burrow derives against the groups burrow has seen. A declaration
SHALL replace the row's audience for that model rather than intersect it, so one
rule can narrow an open row or open one model of a restricted row. A model that
declares nothing SHALL follow its row, and an org where nothing is declared
SHALL behave exactly as it did before model audiences existed.

Saving SHALL cover the whole table against one revision, the way REQ "Mapping is
edited, not deployed" words it for the Library: a page whose revision is out of
date SHALL be refused with 409, an empty audience or a group name carrying
control characters SHALL be refused with 400, and a provider row the page names
that no longer exists SHALL refuse the save. A row whose declarations changed
SHALL have its `updated_at` bumped and an audit entry naming the actor written;
a row whose declarations did not change SHALL NOT.

Whether a member may hold a row's key SHALL be decided from these declarations
and the row's `groups` alone, never from a gateway answer; a menu burrow already
has SHALL only decide whether the row is served at all.

#### Scenario: a rule narrows one model of an open row

- **GIVEN** a row with no `groups` whose model `m1` declares `["owner"]`
- **WHEN** a member who holds neither the row's audience nor `owner` lists providers
- **THEN** the row is served without `m1`

#### Scenario: a rule opens one model of a restricted row

- **GIVEN** a row with `groups: ["owner"]` whose model `m2` declares `["family"]`
- **WHEN** a member holding only `family` lists providers and connects to the row
- **THEN** the row is served with `m2` and `/connect` returns its key

#### Scenario: a member who sees no model is served no row and no key

- **GIVEN** a row with `groups: ["family"]` whose every model declares an audience that does not include `family`
- **WHEN** a member holding only `family` lists providers and then calls `/connect` for the row
- **THEN** the row is absent and `/connect` answers 404, as it does for an unknown id

#### Scenario: a gateway that answers nothing changes no answer

- **GIVEN** a row whose models declare audiences, and a gateway whose menu burrow cannot fetch
- **WHEN** a member entitled to the row lists providers and calls `/connect`
- **THEN** the row is served, filtered by the same rules, and its key is returned,
  and a member who is not entitled is still served nothing

#### Scenario: withdrawing a rule returns the model to its row

- **GIVEN** a matrix saved with every box unchecked for a model that had a rule
- **WHEN** the save succeeds
- **THEN** the model carries no declaration, follows its row, and the audit entry
  names who saved it

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
states. Once any rule exists, a member holding no group that has a rule SHALL
see no skills — the fail-closed default, which replaces the original "keep the
whole catalog" because a group no rule names is exactly what a rename, a lost
claim, or a not-yet-declared group looks like, and it must not be the widest
door in the system (ADR-0006). While no rule is written anywhere every member
SHALL keep the whole catalog, so REQ "Compatibility" stands untouched. A member
whose only group has a rule allowing nothing SHALL see no skills — the same
answer as the fail-closed default, for a different reason. Saving SHALL rewrite
the groups the page displayed and leave the rest alone.

The console SHALL make the fail-closed default visible: every group a member
has signed in under that no rule names SHALL be named on the matrix page, and
a sign-in that holds no declared group while any rule exists SHALL be logged.
A group SHALL be declarable by name from the console, before any of its
members has signed in.

#### Scenario: switching a group off withdraws its rule

- **GIVEN** group `family` has a rule
- **WHEN** an admin saves with `family`'s switch off
- **THEN** `family` has no rule; while another rule exists a member whose only
  group is `family` sees no skills, and once it was the org's last rule,
  everyone sees the whole catalog again

#### Scenario: a new group is restricted before its first sign-in

- **GIVEN** some rule exists
- **WHEN** a member holding only the undeclared `newteam` lists the Library
- **THEN** they see no skills, `newteam` is named on the matrix page, and an
  admin can declare `newteam` by name and give it a pack without waiting for
  any of its members to sign in

#### Scenario: a sign-in with no groups claim fails closed

- **GIVEN** some rule exists
- **WHEN** a member signs in whose ID token carries no `groups` claim
- **THEN** they see no skills, and the sign-in is logged

#### Scenario: a rename does not silently re-bind its rule

- **GIVEN** group `ops` has a rule and the identity provider renames it to `Ops`
- **WHEN** the member recorded under `ops` signs in under `Ops`
- **THEN** they see no skills, the sign-in is logged, and the console names
  both `ops` as a rule nobody signs in under and `Ops` as a signed-in group
  with no rule
- **AND** the rule follows to `Ops` only once an admin declares that name

### Requirement: Compatibility

With no audience declared anywhere — no provider `groups`, no skill mapping —
every response SHALL be identical to the stages before this spec, and burrow
SHALL ask the issuer for nothing it did not ask for before beyond the `groups`
scope.

## Out of scope

Shared MCP connections by group (#4), per-member keys minted by burrow
(ADR-0003 option 3), and the per-group virtual key that would make a hidden
model unreachable rather than merely unlisted (the open decision in ADR-0009).
