---
status: proposed
date: 2026-10-10
decision-makers: [Joe Stump]
related: ADR-0001 (supersedes its store choice), ADR-0044 (cloud01 edge platform)
---

# ADR-0002: The store is Postgres on the shared cluster

## Context and Problem Statement

ADR-0001 chose SQLite — one file, no database to operate — which was right for
a control plane whose entire state was users, sessions, and tokens. Two things
have changed since. First, the access-control work (#12 skill packs by OIDC
group, #13 per-group provider and model visibility) needs real relational
schema: group-to-resource mappings queried per request, and #13's key-gating
predicate joined against what a member can see. Second, cloud01 already runs a
shared Postgres for the edge stack — the cluster `switchboard` and `cairn` use —
so "no separate database to operate" is no longer the choice on the table. Keep
SQLite and burrow is the only service in the fleet with a bespoke store, and the
access-control schema gets written once for SQLite and again for whatever
replaces it.

When is the right moment to switch? Burrow is days old with one user and
nothing in the database worth keeping. Later means migrating.

## Decision Drivers

* #12 and #13 need relational schema and must not build it twice.
* The cluster already exists and is already operated; a burrow database is a
  provisioning task, not a new service.
* Deployment stays boring: one binary, one URL to configure, secrets from
  OpenBao like every other service on the host.
* The tests must exercise the real store. A fake database would leave the
  access-control predicates untested before they ship.

## Considered Options

* **Postgres on the shared cloud01 cluster** (this ADR)
* Keep SQLite and add a second store for the access-control mappings
* A dedicated Postgres instance for burrow
* Postgres plus a connection pooler (pgbouncer) from the start

## Decision Outcome

Chosen option: "Postgres on the shared cloud01 cluster", because it is the only
option that lets the access-control schema land once, on database software the
fleet already runs and backs up.

### The shape of it

* **pgx's `database/sql` driver**, not `pgxpool` directly. The store's method
  set is unchanged, so handlers do not churn, and `database/sql` keeps the
  pool tunable in one place. Pure Go, so the binary is still `CGO_ENABLED=0`
  and the image is still one static layer.
* **`BURROW_DATABASE_URL`, required.** There is deliberately no SQLite fallback
  and no dual path: a binary that can start against either backend is a binary
  whose backend you have to discover at runtime.
* **No migration.** Nothing in the SQLite file is worth carrying. Say so in the
  PR rather than writing a migrator for one user's sessions.
* **The credential follows the cairn pattern:** the playbook ensures a `burrow`
  database in the shared cluster, renders `BURROW_DATABASE_URL` into the service
  environment, and the password is seeded into OpenBao with `cas 0` so a
  converge never rotates it under a running container. It never appears in the
  repo or the image.

### Testing

`BURROW_TEST_DATABASE_URL` points at a real server and every test creates its
own schema, dropped when it finishes — so suites and parallel tests cannot see
each other's rows, and a test cannot touch the database's public schema. The
tests **fail** when the variable is missing instead of skipping: a suite that
skips quietly still reports green, and that is precisely how cairn shipped a
reaction bug with ~270 tests never running. Locally `make test` starts a
disposable container; CI stands one up on a run-scoped network (see
`.gitea/workflows/ci.yaml` for the runner facts behind that shape).

### Consequences

Burrow gains a deployment dependency it did not have: it will not start without
a database, so the ansible change must land **before** this image is deployed.
The fleet gains one consumer of a database it already operates. Local
development needs Docker (or any reachable Postgres) for `make test`, which is
what every other service on the host already requires. A pooler is deferred —
one service with a 10-connection ceiling does not need one, and adding it is a
configuration change, not a redesign.
