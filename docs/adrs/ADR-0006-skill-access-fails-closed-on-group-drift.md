---
status: proposed
date: 2026-10-16
decision-makers: [Joe Stump (pending review)]
related: ADR-0003 (provider access by OIDC groups), SPEC-0003 (org-access-control, amended here), stump.wtf/burrow#12, stump.wtf/burrow#33, stump.wtf/burrow#39
---

# ADR-0006: The Library fails closed on group drift

## Context and Problem Statement

SPEC-0003 REQ "Library audience" as originally written says a member holding
no group that has a rule keeps the whole catalog (#12 decision 2: writing a
rule for one group must not hide the Library from everyone else). Issue #39
reproduced on `main` that this clause makes the filter fail open on every
path along which group data drifts away from the rules an admin wrote:

- A brand-new IdP group cannot be restricted before its first member signs
  in: the matrix's columns come from groups burrow has seen, and the form has
  no way to name a group that has not signed in yet, so the first member of
  that group gets the full Library — bodies included over MCP — before an
  admin can act.
- A sign-in whose ID token carries no `groups` claim (IdP config drift, or
  `BURROW_OIDC_GROUPS_SCOPE=false`) records no groups, and a member with no
  groups holds no declared group, so that member sees everything.
- An IdP rename or re-case of a declared group leaves the rule naming a name
  nobody signs in under and the member holding a name no rule declares, which
  lifts the restriction at the member's next sign-in.

Providers fail closed on the same inputs — a member who holds none of a
model's groups simply does not see the model — and nothing about any of the
three is logged, so the Library is both wider and quieter than the rest of
the system under exactly the drift an identity provider actually produces.

## Decision Drivers

- The declared-group matrix of #12 decision 2 is the model; it is not on
  trial here. What is undetermined is only what an undeclared group, an empty
  claim, and a rename get.
- One rule, every surface: the Library list, the resource snapshot, the
  marketplace and plugin resolutions, the config-object read, the MCP tools,
  and the discovery index all read one filter, so the fix must live in that
  filter and cannot be a surface-by-surface patch.
- Failing closed must not mean failing invisible: a member quietly locked out
  is the same outage as a quiet leak, and an admin learns about either only
  if the system says so.
- An org that has never touched the matrix sees no change, so shipping the
  software is still nothing (REQ "Compatibility").

## Considered Options

- Console-only fix (issue option 1): an "add group by name" input. It closes
  the pre-declaration gap but leaves the empty claim and the rename lifting
  every restriction; a half fix.
- Case-insensitive group matching, so a re-case keeps matching its rule. It
  contradicts SPEC-0003 REQ "Groups claim", which says names compare exactly,
  everywhere, on both surfaces — a re-case that silently re-binds a rule is
  also a rename of a different group silently landing on someone else's
  allowance. Drift should surface, not be papered over.
- An org-wide default rule ("members in no declared group see: ..."), the
  issue's option 3. It is the most expressive answer and the only way to say
  "everyone else sees everything" once restrictions exist. It is also a new
  store table, new console surface, and new spec text — a decision of its own
  that no thread has made. Deferred, and named in More Information.

## Decision Outcome

Chosen option: the fail-closed default, with the console's declare-by-name
input as its operator affordance and the drift made visible in log and page.

1. Once any rule exists, a member holding no declared group sees no skills.
   While no rule exists anywhere the catalog stays whole, which keeps REQ
   "Compatibility" untouched. Undeclared groups, an empty or missing `groups`
   claim, and renames and re-cases all land here, because all three are the
   same state: the member holds no group that has a rule.
2. A group is declarable by name in the console before any of its members has
   signed in; declaring writes a rule that allows nothing, which the reloaded
   matrix edits like any other column.
3. Every sign-in that holds no declared group while rules exist is logged,
   and the matrix page names every signed-in group that no rule declares.
4. A rename does not silently re-bind: the old name shows as a rule nobody
   signs in under, the new name shows as a group with no rule, and the rule
   moves only when an admin says so.

### Consequences

- Positive: the three reproduced fail-open paths answer with an empty slice
  on every face at once, mirroring how providers already answer the same
  inputs; the declared-group union for members the matrix names is unchanged,
  byte for byte.
- Positive: drift is visible — `skills: sign-in holds no declared group` in
  the log, a named badge on the matrix page — so the fail-closed default
  cannot quietly turn into a lockout nobody understands.
- Negative: a change in behavior for deployments that relied on "family is
  mapped, everyone else sees everything". They must now declare the groups
  that keep the Library; the matrix lists the signed-in groups they have to
  declare, and withdrawing the last rule restores the old behavior org-wide.
- Negative: switching a group's column off no longer returns its members to
  the whole catalog while other rules exist — it returns the group to
  "undeclared", which now means nothing. An admin who means "nobody in this
  group, ever" switches it off with nothing checked instead, which was
  already the distinction the store keeps.

## More Information

Evidence and reproduction: stump.wtf/burrow#39, found in the #33
retrospective; all three paths reproduced on `main` @ 832bca5.

The amended requirements are SPEC-0003 REQ "Mapping is edited, not deployed"
and its scenario "switching a group off withdraws its rule"; REQ
"Compatibility" and REQ "Groups claim" stand as written.

The org-wide default rule stays open as its own decision: the honest residual
gap of this ADR is that "every group nobody has declared yet" can only be
answered with "nothing" — an org that wants it answered with a pack of its
choosing needs the default-rule feature, not this one.
