---
status: draft
date: 2026-10-09
implements: [ADR-0001]
---

# SPEC-0002: Stage B org models (LiteLLM providers)

## Overview

Fills `/v1/llm-providers` with the org's real providers so the OpenWork
desktop client syncs org models into its local config. Providers point at the
org's LiteLLM gateway through the openai-compatible adapter; credentials are
served only on `/connect` and come from the environment (OpenBao in
deployment), never from a file in the repo.

SPEC-0004 later moved these rows into the store and derived each row's model
menu from the row's own gateway key. The row shape described here is what the
client reads, unchanged; where the models come from is not part of the
contract.

## Requirements

### Requirement: Provider definitions

Providers SHALL be defined in a JSON file named by `BURROW_PROVIDERS_FILE`
(optional; unset serves none, preserving the Stage A empties). Each
definition SHALL carry `id` (matching `/^lpr_/i`; SPEC-0004 REQ "Rows in the
store" bounds its charset and length), `providerId`, `name`,
`baseURL` (absolute http(s)), `apiKeyEnv` (an environment variable name), and
`models[]` where every model has `id`, `name`, and an optional `config`
object. Load SHALL fail the whole file on any violation — duplicate ids
included — because the client's sync drops the entire provider list on one
bad row, and a bad definition must stop burrow at boot rather than poison
every poll.

### Requirement: Provider rows

`GET /v1/llm-providers` (and `?scope=usable`) SHALL return
`{"llmProviders":[{id, providerId, name, source, providerConfig, hasApiKey,
models[{id,name,config}], updatedAt}]}`. `source` SHALL be exactly `custom`
(never `openwork`, which makes the client set vendor-hosted env vars).
`providerConfig` SHALL be
`{"npm":"@ai-sdk/openai-compatible","options":{"baseURL":<baseURL>},"env":[<apiKeyEnv>]}`.
`hasApiKey` SHALL be true exactly when the provider's environment variable is
set to a non-empty value. `updatedAt` SHALL be the definition file's mtime.

### Requirement: Connect

`GET /v1/llm-providers/:id/connect` SHALL return
`{llmProvider:{…row…, apiKey}}` with `apiKey` present only when the key is
provisioned; an unknown id SHALL 404. The key value SHALL be read from the
environment at request time and never persisted.

### Requirement: Resource snapshot

`GET /v1/resources` SHALL carry `resources.llmProviders` as an id→updatedAt
map so the client notices provider changes; with no providers configured it
SHALL be an empty map.

## Out of scope

`/v1/inference-providers` (the AI Gateway surface) stays a 404 — the client
treats it as "none". Per-member credentials and access teams stay out until
the org model grows them.
