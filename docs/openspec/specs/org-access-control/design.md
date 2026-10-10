---
status: draft
date: 2026-10-10
implements: [ADR-0003]
---

# SPEC-0003 design: providers and models by OIDC group

- **Groups on the user, written at sign-in.** The callback already upserts the
  user from the ID token; it now writes `groups` in the same statement. Every
  session request reads them with one `GetUser`, so there is nothing to keep
  in step with the session table. A group change waits for the next sign-in,
  the same trade #12 proposed ("read at sign-in"), and cheaper than calling
  userinfo per request.
- **A JSON column, not `TEXT[]`.** The store uses `database/sql` over pgx, and
  a JSON-encoded `TEXT` column round-trips without pulling pgtype into the
  store's signatures. The store has no migration history (ADR-0002), so the
  column arrives with `ADD COLUMN IF NOT EXISTS`.
- **Scope requested only when used.** The `groups` scope is added only when the
  providers file declares groups, so an issuer that rejects unknown scopes is
  unaffected otherwise (the same rule cairn uses for its operator group).
- **Filtering in the runtime, not the handlers.** `providers.Runtime` gains
  `RowsFor`, `SnapshotFor` and `ConnectFor`, taking the member's groups. The
  handlers stay one line each, and the visibility rule has one implementation
  that unit tests cover without HTTP.
- **Invisible looks like absent.** A hidden row answers `/connect` with the
  same 404 as an unknown id, so ids do not leak who else has what.
- **One row per audience is the enforceable pattern.** Model-level groups are
  supported for display, and burrow warns when a row mixes audiences, because
  the row's key still reaches the hidden models.
