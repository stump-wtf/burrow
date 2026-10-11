---
status: draft
date: 2026-10-10
implements: [ADR-0003]
---

# SPEC-0003: Org access control (OIDC groups) — providers and models

## Overview

Each member sees only the LLM providers and models their OIDC groups allow, and
`/connect` hands a provider's key only to members who can see it. Groups come
from the identity provider's `groups` claim at sign-in. Access is declared in
the providers file (SPEC-0002), next to the models and key each row serves
(ADR-0003). An org that declares no groups behaves exactly as Stage B.

SPEC-0004 moved these declarations from the file onto store rows, and retired
model-level groups for derived menus: a derived model's audience is its row's.

Skills (the Library) and shared MCP connections are out of scope here; they
follow in later sections of this spec (stump.wtf/burrow#12, #4).

## Requirements

### Requirement: Groups claim

When the providers file declares `groups` anywhere, burrow SHALL request the
`groups` scope at sign-in. At every sign-in burrow SHALL record the member's
groups from the ID token's `groups` claim, accepting a JSON array of strings or
a single string, and replacing what was recorded before. A missing or empty
claim SHALL record no groups. Groups SHALL be read from that record for every
later request, so a change in the identity provider takes effect at the
member's next sign-in.

#### Scenario: claim recorded at sign-in

- **WHEN** a member signs in with `groups: ["family", "everyone"]`
- **THEN** later requests from that member's session use those two groups

### Requirement: Group declarations

A provider definition and each of its models MAY carry `groups`, a non-empty
array of non-empty strings. Load SHALL fail on an empty `groups` array or an
empty entry, because "visible to nobody" written as `[]` is indistinguishable
from a mistake.

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
member can see.

### Requirement: Key-scope warning

Burrow SHALL log a warning at load for each row whose models do not all share
one audience, because the row's single key reaches every model in it: the
narrower models are hidden from listing but not from the key.

### Requirement: Compatibility

With no `groups` declared anywhere, every response SHALL be identical to
SPEC-0002, and burrow SHALL NOT request the `groups` scope.

## Out of scope

The `/admin` console and group mappings for skills (#12), shared MCP
connections (#4), and per-member keys minted by burrow (ADR-0003 option 3).
