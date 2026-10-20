---
status: proposed
date: 2026-10-20
decision-makers: [Joe Stump (pending review)]
related: ADR-0002 (Postgres store), ADR-0003 (provider access by OIDC groups), ADR-0005 (API-token reach is its minter's live admin rights), SPEC-0001 (stage-a-control-plane, amended here), SPEC-0003 (org-access-control), stump.wtf/burrow#41
---

# ADR-0008: Revocation is a session family's cascade, and one session-only admin action

## Context and Problem Statement

Issue stump.wtf/burrow#41 reproduced two gaps on `main` @ 832bca5. Sign-out
deletes exactly one `sessions` row: a session a desktop handoff derived from
it keeps answering `/v1/me`, and every MCP token the family minted keeps
serving skill bodies over `tools/call` for the rest of its 24 h — signing out
of the browser leaves the desktop signed in. And there is no admin path to
end a member: removing one takes raw SQL, and their stored OIDC groups keep
being served until every session expires naturally.

The session-lifetime fix (burrow#45, on `main`) already bounds how long a
family lives — no generation outlives the sign-in it descends from. Nothing
yet ends a family early, and nothing ends a member at all.

## Decision Drivers

- Sign-out must mean "this sign-in is over", on every surface the sign-in
  reached, with no new state to keep in sync.
- Enforcement belongs in the store, not in a lookup that remembers to check
  a parent: a revocation path that runs after an outage, or from a second
  process, must not have to know the family rules.
- The admin path must not become a second admin surface that a leaked
  automation token can pull (ADR-0005's rule from burrow#37).
- Removing a member's *row* is not revocation: audit actors are member ids,
  and the account may exist again at the issuer tomorrow.
- Databases already running must converge on boot like every schema change
  here does (ADR-0002).

## Considered Options

1. **Check parents at lookup time** (walk `parent_session_hash` on every
   session and MCP-token read). Rejected: it puts revocation logic in four
   lookups instead of one schema fact, and every future credential kind must
   remember to re-implement the walk.
2. **One family id per session** (a shared `family_id` column instead of a
   parent pointer). Rejected: it needs the tree flattened into one value at
   mint time, it loses which session is whose parent (the thing the lifetime
   cap already reasons over), and a cascade over `parent_session_hash` is the
   same DELETE with no parallel bookkeeping.
3. **Delete the user row for removal.** Rejected: `users.id` is the audit
   actor and the OIDC subject; deleting it rewrites history and breaks the
   account that reappears at the issuer.
4. **A CLI admin subcommand.** Rejected for now: burrow has no operational
   CLI (boot and the reaper are its only subcommands), a CLI is another
   credential-holding binary on another box, and the admin HTTP surface
   already has the gate, the gate's config, and the audit actor vocabulary.
5. **Cascade through the family, plus one session-only admin route.**
   Chosen.

## Decision Outcome

**The schema owns the family.** A handoff grant records the session that
minted it (`grants.parent_session_hash`), the session it buys carries that
parent (`sessions.parent_session_hash`), and `mcp_tokens` carries the session
that minted the token — all three foreign keys `ON DELETE CASCADE`. Sign-out
stays one DELETE of the presented row, and the cascade ends every session and
token under it, to any depth. Revocation is downward only: signing out the
desktop does not sign out the browser it descended from.

**Upgraded databases are familyless until they re-sign-in.** Rows that existed
before the columns cannot have a parent nobody recorded; the guarded changes
add the columns with NULL, and the first fresh sign-in after an upgrade is the
first family. The migration does not guess parents.

**Admin revoke is an HTTP route behind `requireAdminSession`.**
`POST /v1/admin/members/{id}/revoke` deletes the member's sessions (the
cascade takes their families and minted tokens), their unexchanged grants,
their MCP tokens and their `api_tokens`, and clears their stored group
snapshot so nothing keeps being served from groups the issuer no longer
asserts; their next sign-in re-reads the claim (SPEC-0003 REQ "Groups claim").
The member row survives — this is revocation, not deletion. It is
session-only like token management: a `bat_` bearer is answered
`403 session_required`, because provider-scope automation must not be able to
end everyone's sessions. The response counts what it removed, and the call is
logged with actor and target together; self-lockout is legal.

## Consequences

- `sessions.parent_session_hash` gives sessions a self-referential cascade;
  sign-out of a root in a deep chain is one statement, not a loop.
- An MCP token minted by a handoff-derived session dies with either
  generation — and with the root — which is exactly what "sign out means
  signed out" requires.
- The admin surface grows one route with no audit table of its own; the log
  line is the record. If incident review later needs queryable revoke
  history, that is a new table and a new decision, not a silent extension of
  this one.
- Revoke does not touch the issuer: burrow ends what it authenticates; the
  IdP ends the account.
