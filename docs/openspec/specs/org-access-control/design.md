---
status: draft
date: 2026-10-11
implements: [ADR-0003]
---

# SPEC-0003 design: providers, models, and the Library by OIDC group

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
- **Scope requested by default.** Provider rows and Library rules are edited
  at runtime, so whether groups are needed cannot be decided at boot the way
  it could be when only the providers file declared them. burrow asks the
  issuer for the `groups` scope unless `BURROW_OIDC_GROUPS_SCOPE` turns it
  off, giving operators whose issuer rejects unknown scopes one knob.
- **Filtering in the runtime, not the handlers.** `providers.Runtime` gained
  `RowsFor`, `SnapshotFor` and `ConnectFor`, taking the member's groups, and
  the Library follows the same shape: a pure function in `internal/skills`
  slices the catalog from the member's groups and the stored packs. The
  handlers stay one line each, and both visibility rules have one
  implementation that unit tests cover without HTTP or a database.
- **Invisible looks like absent.** A hidden row answers `/connect` with the
  same 404 as an unknown id, and a hidden skill answers the Library list, the
  plugin resolution, and `get_skill` exactly like a name that was never in
  the catalog, so neither surface leaks who else has what.
- **One row per audience is the enforceable pattern.** Model-level groups are
  supported for display, and burrow warns when a row mixes audiences, because
  the row's key still reaches the hidden models.
- **The Library has one access path.** Every surface that serves skills calls
  `Server.library`, and the unfiltered catalog search and lookup were deleted
  rather than left beside it, so a future surface has to opt into the rule
  explicitly instead of accidentally bypassing it. The unfiltered list
  survives only where the rule does not apply: the admin console's matrix.
- **Declared and allow-nothing are two states, so two tables.** "No rule
  written for this group" (the org-wide default) and "a rule that allows
  nothing" must stay distinguishable, because the first means the whole
  catalog and the second means no skills. A single table with one row per
  group would make the second unrepresentable, so `skill_packs` records which
  groups have a rule and `skill_pack_skills` holds the skills; the read has to
  return declared-empty groups as keys for the difference to survive a round
  trip.
- **The matrix posts indices, with a switch per column.** Group names carry
  whatever characters the IdP produced, so there is no safe form-field
  separator; cells post positional indices instead. The per-column declare
  switch exists because without it, saving the page would silently declare a
  rule for every group burrow has ever seen, restricting members an admin
  never touched.
- **The MCP token carries its member.** An MCP request authenticates via
  `LookupMCPToken`, which now yields the member the token belongs to; that
  id rides on the request context so the Library filter resolves the same
  groups a browser session would. An MCP token is its member's view, not a
  god view.
