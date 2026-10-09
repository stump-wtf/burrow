---
status: proposed
date: 2026-10-09
decision-makers: [Joe Stump]
---

# ADR-0001: Clean-room, OpenWork-compatible control plane (Go + SQLite + OIDC)

## Context and Problem Statement

The OpenWork desktop app is MIT-licensed, but its cloud backend is EE-licensed and its free self-hosted tier forbids SSO by license. We want the desktop app to sign in with our own OIDC provider, serve our org's skills, and later hand out our own LiteLLM models — without a vendor account or a license conflict. How should burrow be built, and from what sources?

## Decision Drivers

* Single sign-on through the identity provider we already run (Pocket ID), with no per-seat licensing exposure.
* A client contract derived **only** from MIT-licensed desktop client code, so burrow can be public and unencumbered.
* One small thing to operate: single binary, one database file, boring deployment on StumpCloud.
* Deliver value early: sign-in and skills first, models and shared MCP later.

## Considered Options

* Clean-room implementation of the client contract (this ADR)
* Run the upstream EE-licensed backend under its self-hosted tier
* Use the vendor's hosted cloud offering

## Decision Outcome

Chosen option: "clean-room implementation of the client contract", because it is the only option that gives us SSO without a paid license and keeps the code ours.

### burrow is clean-room

The specification is `docs/client-contract.md`, written only from the MIT
desktop client (`apps/app/src`, `apps/desktop/electron`, `apps/server/src`,
`packages/*`). The EE-licensed backend source tree (`ee/`), its API
specification (`packages/docs/openapi.json`), and its generated SDK
(`packages/sdk/src/gen/*`) are **never read** — not to "confirm a detail", not
to compare designs. Where the client leaves behavior unspecified, burrow
designs it and records the design here. burrow brands itself `burrow`; the
upstream product's cloud names never appear in user-facing text beyond
compatibility statements.

### Stack

* **A Go single binary** — matches the rest of the `stump.wtf` products,
  deploys as one artifact, standard library HTTP.
* **SQLite** — one file, no separate database to operate for an
  org-sized control plane.
* **OIDC sign-in (authorization code + PKCE)** — Pocket ID first, any OIDC
  issuer by configuration. Credentials come from the environment at runtime,
  never from the repo.
* **Library content from a git repository of `SKILL.md` skills** (or a local
  directory of `<name>/SKILL.md`), served through the client's marketplace,
  plugin, and config-object reads.
* **Org LLM providers pointing at a LiteLLM gateway** (Stage B).

### Staging

Stage A (sign-in, org, Library skills, the MCP endpoint) first; then B (org
models via LiteLLM); C (shared MCP connections) and D (Automations and
Dashboard) later. Each stage lands against the same contract; stubs the client
polls return stage-appropriate empties until their stage lands.

### Consequences

* Good, because SSO is ours, the license is clean, and the code is ours.
* Good, because the contract document doubles as burrow's API spec, so
  compatibility is testable.
* Bad, because behavior the client does not exercise is ours to design and
  maintain without an upstream reference.
* Bad, because upstream client changes can break compatibility without
  warning; the contract (and its pinned upstream commit) is the only early
  warning we have.

### Confirmation

Every stage-A endpoint has a contract test asserting the exact fields the
client reads (see `internal/httpapi` tests). CI runs `make lint`, `make test`,
and a secret scan as required checks. Re-deriving the contract from a newer
upstream client commit requires re-reading only MIT paths and re-copying the
artifact into `docs/client-contract.md`.

## Pros and Cons of the Options

### Clean-room implementation of the client contract

A from-scratch Go server implementing `docs/client-contract.md`.

* Good, because no EE license is touched, ever; the license risk is eliminated
  rather than managed.
* Good, because the server is exactly as big as what we use — no features we
  must disable or work around.
* Good, because it is public-repo compatible and testable against the
  contract.
* Bad, because unspecified behavior must be designed and owned by us.
* Bad, because upstream client drift is only caught by the contract being
  re-derived.

### Run the upstream EE-licensed backend under its self-hosted tier

* Good, because the implementation already exists and tracks the client.
* Good, because compatibility is the vendor's problem.
* Neutral, because the self-hosted tier exists and runs.
* Bad, because SSO is forbidden by license on the free tier and paid per
  seat beyond it — the exact requirement that motivated burrow.
* Bad, because the EE sources could not even be consulted for a clean-room
  alternative afterwards; the license taints the participants.

### Use the vendor's hosted cloud offering

* Good, because nothing is operated locally.
* Bad, because org data and credentials live with the vendor.
* Bad, because SSO and seats are a recurring cost, and the org's skills and
  model routing are subject to the vendor's roadmap.
* Bad, because it forfeits the LiteLLM integration we already run.

## Architecture Diagram

```mermaid
flowchart LR
    subgraph desktop["Desktop (MIT client)"]
        app[OpenWork app]
        server[local server]
    end
    subgraph idp["Identity"]
        pocket[Pocket ID OIDC]
    end
    subgraph burrow["burrow (Go single binary)"]
        web[sign-in page\nauth code + PKCE]
        api["/api/den/v1 APIs\nme, orgs, library, stubs"]
        mcp["MCP endpoint\nsearch_capabilities,\nexecute_capability, get_skill"]
        db[(SQLite)]
        skills[skills git repo\nname/SKILL.md]
    end
    litellm[LiteLLM gateway\nStage B]

    browser["User's browser"] -->|OIDC| pocket
    app -->|opens sign-in| web
    web -->|authorization code + PKCE| pocket
    web -->|one-time grant, openwork:// link| app
    server -->|Bearer session token| api
    api --> db
    server -->|MCP token| mcp
    mcp --> skills
    api -.->|llm-providers (Stage B)| litellm
```

## More Information

* The client contract: `docs/client-contract.md` (verbatim from the artifact
  `mcp://cairn/s4vJe24V`, derived from the MIT desktop client at upstream
  `dev` `70c948c`).
* Upstream desktop client: https://github.com/different-ai/openwork (MIT).
* Why this exists — the SSO licensing findings: stumpcloud/stumpcloud#566.
