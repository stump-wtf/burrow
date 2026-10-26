---
status: proposed
date: 2026-10-25
decision-makers: [Joe Stump (pending review)]
related: ADR-0003 (provider access by OIDC groups), ADR-0004 (gateway-derived model lists), ADR-0006 (skill access fails closed on group drift), SPEC-0003 (org-access-control, amended here), SPEC-0004 (provider-admin), stump.wtf/burrow#12, stump.wtf/burrow#13
---

# ADR-0009: A model's audience lives inside its row; the key that reaches it stays one

## Context and Problem Statement

Issue stump.wtf/burrow#13 is a table of who may use which model. Some models on
the shared local gateway are the household's, two of them are also Jon's, and
everything else is Joe's alone. On `main` @ 2e2f3dd that table cannot be written
down. A provider row carries one audience and one key, and #12's access control
stops at that row: a member either gets the whole row, every model its key
reaches, or nothing. The only way to express the table is to split one gateway
into several rows, which duplicates a live key across rows and hands any member
in two groups two near-identical entries for one gateway.

SPEC-0003 always worded the finer rule ("a model's audience SHALL be its own
when present, else its provider's"), but nothing implemented it. Model-level
`groups` were retired for derived menus by ADR-0004, and a seed file that still
names them is warned about and dropped. So the spec has an adjective with no
noun behind it, and the admin has no surface that names a model at all.

The uncomfortable part is what a model audience does *not* buy. One row carries
one LiteLLM key, and that key reaches every model the gateway serves for it. A
rule can therefore remove a model from a member's picker without removing it
from what their key can call. That gap predates this ADR — SPEC-0003's REQ
"Key-scope warning" is the log line standing in for it — and it is the open
decision at the end of this document, not something the matrix closes.

## Decision Drivers

- Shipping must change nothing until an admin writes something (SPEC-0003 REQ
  "Compatibility"): an org that never restricted a model keeps today's answer
  byte for byte.
- One rule, used by every face. The listing, the resource snapshot and
  `/connect` must not be able to disagree about who sees a model, because
  `/connect` is the one that hands over a key.
- A security answer cannot wait on the gateway (ADR-0004 keeps `/connect` from
  ever calling one). Whether a member may hold a row's key has to be answered
  from the rules burrow already has.
- An admin edits a mapping, not a deployment (ADR-0006): whole-matrix save, one
  revision, a stale page refused rather than silently merged.
- An audience change must look like a row change to clients: the snapshot is a
  map of id to `updatedAt` (docs/client-contract.md), so a row whose rules
  changed has to have its `updated_at` bumped, and the change needs an audit
  entry naming who made it.

## Considered Options

1. **Row audiences only** (status quo). Cannot express the table. Its
   workaround, one row per audience against one gateway, duplicates a live key,
   duplicates the menu, and shows a member several pickers for one gateway.
2. **Model-level `groups` inside the row's stored JSON.** Reintroduces the shape
   ADR-0004 retired. Every edit is a read-modify-write of a whole row blob: no
   revision guard, no per-model diff, no audit that says which model moved, and
   a row save and an audience save can overwrite one another.
3. **A declared audience per model in its own table, edited as a matrix, with an
   undeclared model inheriting its row's audience** (chosen). A strict superset
   of current behavior, so it ships as a no-op. It expresses the table exactly:
   set the shared gateway's row to Joe's group, then open named models to
   `family` and `jon`.
4. **Fail-closed default, as ADR-0006 chose for the Library.** Would make the
   first rule anywhere a behavior change for everyone else, and would need a
   wildcard rule to say "the rest of this row is Joe's". The table needs no
   such rule: inheritance says it.
5. **Enforcement at the gateway with a key per group.** The only option that
   makes a hidden model unreachable rather than merely unlisted. Deferred; the
   options are laid out under Decision Outcome so the choice is Joe's with the
   cost attached.

## Decision Outcome

Chosen option 3, with the key-scope gap named in the page, in the log and in
this ADR rather than papered over by the picker.

1. Model audiences live in a `model_audiences` table keyed by
   `(provider_id, model_id)`, holding the groups that may see that model. The
   whole table has one revision; a save posts it in full and a page whose
   revision is stale is refused with 409. A provider row that has been deleted
   since the page rendered refuses the save rather than resurrecting a rule.
2. A declared audience **replaces** the row's audience for that model; it does
   not intersect with it. So a restricted row can open one model to a wider
   group, and an open row can hold one model back. A model with no rule follows
   its row, and a row with no rules is exactly what it was before this existed.
3. Who may hold a row's key is computed from the rules and the row's audience
   alone. The gateway menu is consulted only to decide whether the row is served
   at all, and only when a menu is already known. An outage can therefore
   neither withdraw a row from an entitled member nor widen a key to one who is
   not.
4. A member who sees no model of a row is served no row: not in the listing, not
   in the snapshot, and `/connect` answers for that id exactly as for an unknown
   one.
5. Only rows whose rules actually changed get `updated_at` bumped and an audit
   entry, so a client's snapshot diff and the audit log both say "something
   moved here" and stay quiet about the rows that did not.
6. SPEC-0003's key-scope warning is emitted at boot and after each save for
   every row whose models do not all share one audience: the rows where hiding
   a model is not the same as withholding it.

**The open decision, for Joe.** What closes the gap is a key per group, and the
shape of it is a decision this ADR does not make:

- **One row per audience today.** No code: the operator creates a second row
  with its own `apiKeyEnv` pointing at a second LiteLLM virtual key scoped to
  that group's models. Costs duplicate rows and duplicate menus, and a member in
  both groups still sees two entries for one gateway.
- **Several keys on one row** (recommended). The row keeps its key for members
  inside its own audience and carries a group-to-key-env mapping for the models
  opened past it; `/connect` picks the entry for a group the member holds. One
  row, one picker, enforcement at the gateway, and the key-scope warning
  disappears for a mapped row. Costs: the write gate must validate each name
  (ADR-0005), and a member holding several of the row's groups needs a
  deterministic pick, which means the mapping is ordered and the narrowest
  matching entry wins.
- **Burrow on the data path.** Burrow holds the row key and mints a short-lived
  per-request credential scoped to what the member may call. Cleanest
  enforcement, but it puts burrow in the request path, which is not stage B.

### Consequences

- Positive: the household table becomes data an admin edits at `/admin/models`,
  and shipping it changes nothing until someone checks a box.
- Positive: listing, snapshot and `/connect` read one predicate, so the picker
  and the key cannot drift apart the way two implementations would.
- Positive: a gateway that is down answers the access question the same way one
  that is up does.
- Negative: the filtered picker is presentation, not enforcement. A member served
  a row can still reach a hidden model through its key, and this ADR does not
  change that. Until the key decision above lands, a row whose models have
  different audiences is a row where burrow relies on Joe's LiteLLM setup.
- Negative: a new table, a new admin page and a second kind of audience to
  reason about next to the row's. The matrix says which models "follow their
  row" so the two do not have to be cross-referenced by hand.

## More Information

Requirements and evidence: stump.wtf/burrow#13, whose access table Joe posted in
the issue comment of 2026-10-10; built on #12's row-level access control
(ADR-0003, ADR-0006). Out of scope and named there: #4 and #5.

The amended spec is SPEC-0003, which gains REQ "Model audience" and whose REQ
"Visibility" now says where a model's audience is declared instead of naming a
field that never existed in the store. REQ "Key-scope warning" stands as written,
and is now something burrow can actually answer.

Provider ids, model ids and group names are checked the way #12 checks them: no
control characters, no empty audience, no wildcard. A rule naming a model the
gateway does not currently list is kept, so a menu that comes back later finds
its audience already set.
