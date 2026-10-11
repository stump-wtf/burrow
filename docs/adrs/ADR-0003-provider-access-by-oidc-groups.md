---
status: proposed
date: 2026-10-10
decision-makers: [Joe Stump]
related: ADR-0001 (org LLM providers), ADR-0002 (Postgres store), ADR-0004 (derived model menus), SPEC-0003 (org-access-control), SPEC-0004 (provider-admin)
---

# ADR-0003: Provider and model access is declared in the providers file, by OIDC group

## Context and Problem Statement

Stage B (SPEC-0002) serves one org-wide provider list: every member sees every
model, and `/connect` hands every member every provider's key. The org now has
models that are not for everyone. Self-hosted Qwen models are for the whole
family; some paid gateway models are for two members; the rest are for one.
That is #13's motivating case, with #12 laying the OIDC-groups foundation.

The gate that actually holds is not burrow's menu. The desktop client takes the
key from `/connect` and calls the LiteLLM gateway itself, so a member can use
any model that key is scoped to, whatever burrow chose to list. Burrow's part
is to show each member the right models and to hand out only keys they may
hold.

Where should "who may see which provider and model" live?

## Decision Drivers

* The key's scope is the real enforcement, and it is set where the key is
  minted: the deployment's converge mints one LiteLLM virtual key per provider
  row, scoped to exactly the models that row lists.
* What burrow shows and what the key permits must not drift apart. A listed
  model the key refuses fails at request time; a hidden model the key permits
  is not actually hidden.
* Shipping this must change nothing for an org that declares no groups.
* Group membership already lives in the identity provider (Pocket ID groups),
  where adding someone to a group is the operator's normal workflow.

## Considered Options

1. **Declare groups in the providers file**, next to the models and the
   `apiKeyEnv` each row's key comes from.
2. **Store group-to-model mappings in Postgres, edited in an `/admin` console**,
   as #12 and #13 first proposed for skills and providers alike.
3. **Per-member keys minted by burrow**, calling the gateway's admin API.

## Decision Outcome

Chosen: **option 1**. A provider row and any of its models may carry `groups`;
burrow records each member's OIDC `groups` claim at sign-in and serves each
member only what their groups allow. Provider access lives in the same file the
deployment derives each row's key scope from, so the menu and the key are
changed together, in one reviewed edit.

The admin console (option 2) has since landed for skills (#12), where there is
no key to keep in step: the group-to-skill mapping lives in the store and is
edited at `/admin/skills`, specified as the Library section of SPEC-0003.
Option 3 would make burrow hold the gateway's admin credential and manage key
lifecycle; nothing here needs that yet.

### Consequences

* Good: one source of truth per provider row for models, audience and key
  scope; a change to who sees what is a reviewed diff, not a console click.
* Good: no groups declared means today's behavior exactly.
* Bad: changing an audience needs a converge and a burrow restart, not a
  console toggle.
* Bad: group changes reach a member at their next sign-in, because groups are
  read from the ID token. Removing someone from a group does not revoke a key
  their client already holds: rotate the row's key for that.
* Neutral: model-level `groups` inside one row narrow what is listed but not
  what that row's key permits. Burrow warns at boot when a row does this; the
  enforceable pattern is one row per audience.

## More Information

Requirements: `docs/openspec/specs/org-access-control/spec.md` (SPEC-0003),
including its Library section — the skills console half of option 2, which
#12 shipped.
Issues: stump.wtf/burrow#12 (foundation and Library skill packs),
stump.wtf/burrow#13 (providers).
