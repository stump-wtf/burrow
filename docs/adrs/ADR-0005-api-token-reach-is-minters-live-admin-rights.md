---
status: proposed
date: 2026-10-16
decision-makers: [Joe Stump (pending review)]
related: ADR-0003 (provider access by OIDC groups), ADR-0002 (Postgres store), ADR-0004 (derived model menus), SPEC-0004 (provider-admin, amended here), stump.wtf/burrow#37
---

# ADR-0005: An API token's reach is its minter's live admin rights, and it is minted only by a session

## Context and Problem Statement

SPEC-0004 gave the admin surface two credentials: a member's session, gated by
`BURROW_ADMIN_GROUPS`, and an `llm-admin` API token. It gated both the surface
and the token-mint routes with the same check, and it recorded the audit actor
as the user id for a session or the caller-chosen token name for a token.
Issue stump.wtf/burrow#37 reproduced three consequences on `main` @ 832bca5: a
leaked token could mint a successor that outlived its own revocation; a token
kept working after the member who minted it lost the admin group; and a token
named `alice-sub-1` wrote audit rows indistinguishable from alice's own.

The root of all three is the same: the credential carried authority that was
never its own. Minting is the act that extends the admin surface, and a
credential the surface already accepted could perform it.

## Decision Drivers

- A leaked token's blast radius must be bounded by something other than the
  TTL and someone noticing in time to revoke it.
- Automation holding a token must keep working, and must keep failing with
  `invalid_token` rather than a session-terminating code.
- Audit rows are the incident record: whoever reads one must know which kind of
  principal acted, and a row must not be forgeable by picking a name.
- The store keeps only hashes, and a revoked token's row is deleted, so nothing
  written later can reconstruct a token that no longer exists.

## Considered Options

1. **Keep the token name as the actor** (the status quo). Rejected: the name is
   caller-chosen, so it collides with the member ids it shares a column with.
2. **Rewrite existing rows to the new format.** Rejected: a pre-existing row
   holding `alice-sub-1` cannot be told apart from alice's own session rows
   without its `api_tokens` row, and revoked tokens are deleted. A migration
   would print a guess as fact, which is worse than an honest legacy value.
3. **Add actor columns** (`actor_kind`, `actor_user_id`, a token foreign key).
   Rejected: one TEXT column of one admin-only table does not warrant a schema
   change, and a foreign key to `api_tokens` would be deleted along with the
   token, taking the trail's meaning with it.
4. **Auto-revoke a demoted member's tokens.** Rejected: it silently breaks
   automation, destroys the row that audit rows point at, and cannot be undone
   by putting the member back in the group.
5. **A `tokens-admin` scope for token minting.** Rejected: minting from a token
   still lets the surface extend itself, and it introduces a scope whose meaning
   arrives later — the pattern the issue flags under defect 1.
6. **Session-only minting, live minter check, kind-prefixed actors.** Chosen.

## Decision Outcome

**Token management is a member's decision.** `GET`, `POST`, and
`DELETE /v1/admin/api-tokens` accept a session only. A valid `bat_` bearer that
arrives there is answered `403 session_required`: it did authenticate, it is
simply not the kind of credential that may extend the surface. Every other
admin route keeps accepting both credentials.

**A token borrows, it does not own.** Every token-authenticated admin request
re-reads the member who minted it and applies the `BURROW_ADMIN_GROUPS` check to
that member's recorded groups, exactly as a session's request does. A token
whose minter holds no admin group is answered `403 admin_required`. Demotion
denies; it does not revoke: the token row survives, a promotion put back
restores it, and revoking stays an admin's deliberate act.

**An audit actor names its kind.** A session writes `user:<sub>`. A token writes
`token:<name>#<hash>@user:<sub>`, where `<hash>` is the first 12 hex characters
of the token's own SHA-256 hash — the part no caller chooses — and `<sub>` is
the member whose admin rights it borrows. Rows written before this change keep
their bare values: an actor with no kind prefix is one of those, or the boot
seed's `seed`, and no new row can ever read like either.

### Consequences

- Automation that rotates its own token must be handed a new one by a member.
  That is the intended break, and it is the only user-visible behavior change
  for callers holding a legitimate token.
- Unsetting `BURROW_ADMIN_GROUPS` now closes the surface to tokens as well:
  nobody is an admin, so no token has an admin minter. Tokens minted while no
  groups were configured — which nothing can mint, since minting needs an admin
  session — are unreachable.
- Reading the audit trail means parsing a prefix. `GET /v1/admin/audit` answers
  the column verbatim; a client that groups rows by principal keys on the kind.
- One more store read per token-authenticated admin request, on the same path
  that already reads the token row and, for sessions, the member record.

## More Information

Evidence and reproduction: stump.wtf/burrow#37, found while reviewing #24.
The amended requirements are SPEC-0004 REQ "Admin authorization" and REQ
"Audit". Scope validation at mint (rejecting a scope that means nothing yet) is
deliberately not part of this decision: with minting session-bound and each use
re-checked, an unknown scope grants nothing, and adding meaning to a scope name
later is a decision of its own.
