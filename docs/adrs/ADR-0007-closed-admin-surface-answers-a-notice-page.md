---
status: proposed
date: 2026-10-19
decision-makers: [Joe Stump (pending review)]
related: ADR-0003 (provider access by OIDC groups), ADR-0004 (gateway-derived model lists), SPEC-0004 (provider-admin, amended here), stump.wtf/burrow#32, stump.wtf/burrow#31
---

# ADR-0007: A closed admin surface answers a notice page, not a 403

## Context and Problem Statement

SPEC-0004 REQ "Admin console" scenario "Console closed by default" says that
a deployment with `BURROW_ADMIN_GROUPS` unset answers `/admin` with "403,
exactly like the API". That folds two different facts into one status:

- The deployment did not open the surface. Nothing is wrong with the visitor;
  the operator has not set the variable. The API is closed too, so a signed-in
  member who goes looking for the console is told they are not permitted
  something that does not exist here.
- The gate is open and this member is not in an admin group. This is the
  authorization failure a 403 means.

Issue #32 asks for the first case to read as what it is. The superseded shard-1
branch (#31, closed unmerged) answered a closed gate with a plain 200 "not
enabled" page naming the variable; #30, which landed the full console, recorded
the 403 in the spec instead. Nothing in either thread says which is right, so
this ADR proposes it and the spec scenario is amended to match.

## Decision Drivers

- A status code is an assertion about the request, not about the deployment.
  403 asserts "you may not", which the closed case does not establish.
- Nothing about the closed page may widen what an unauthenticated visitor can
  learn. Today the console's 403 body already names `BURROW_ADMIN_GROUPS`, and
  burrow's own documentation does the same, so a page that names it says
  nothing the code does not.
- The API's answer is unchanged: the write API keeps its 403. The console is a
  browser surface for an operator, and its wording is free to be a sentence.
- Whatever the page says, it writes nothing. The gate check still happens
  before any handler, and a closed surface still cannot be reached past it by
  any method.

## Decision

With `BURROW_ADMIN_GROUPS` unset or empty, a console page for a signed-in
browser is a plain 200 "Admin console not enabled" notice naming the variable
an operator sets, with no navigation (every console route would answer the
same notice) and no state change. An unauthenticated browser is still sent to
sign-in first, so the notice is served to a member burrow has authenticated,
not to the internet. With the gate open, a member outside the admin groups
keeps the API's 403.

## Consequences

- The two cases are distinguishable in a log, a curl, and a support
  conversation. An operator who forgot the variable learns that from the page
  instead of from an authorization error.
- A monitoring probe that treated a non-2xx from `/admin` as "closed" must
  look for the notice instead; nothing else about the surface's behavior
  moves.
- If Joe prefers the old wording, the change is one branch in
  `requireAdminPage`, its test, and this ADR's status; the spec scenario goes
  back to "403, exactly like the API".
