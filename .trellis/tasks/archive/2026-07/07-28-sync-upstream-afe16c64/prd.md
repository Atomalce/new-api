# Sync upstream through afe16c64

## Goal

Bring the fork's `dev` branch to the fixed `QuantumNous/new-api` upstream target
`afe16c64cd73853da1eda3bf236f15d69637b4bf` without rewriting history, while
preserving the fork-owned GHCR delivery flow and the implemented Codex Responses
prompt-cache expiry billing policy and admin settings.

## Background

- `dev` is clean and currently ends at merge commit `4fbfafc5`.
- The last mirrored `main` commit is `a63364d1`; the fetched upstream target is
  53 commits ahead of it.
- Upstream extracted DTO/types/protocol conversion into the standalone
  `relaykit` module and flattened the active frontend from `web/default` to
  `web` while removing the legacy Classic frontend.
- A three-way merge preview found one backend content conflict in
  `relay/channel/openai/relay_responses.go` and one frontend rename-location
  conflict for the fork's prompt-cache settings section.
- Existing Trellis tasks for the prompt-cache billing policy remain active and
  are not part of this synchronization task's lifecycle cleanup.

## Requirements

- Fast-forward local `main` to the fixed upstream target, push the mirror to
  `origin/main`, then merge `main` into `dev` with a merge commit.
- Preserve all upstream application, dependency, CI, security, database, and
  frontend changes unless they conflict with an explicit fork contract.
- Port the Codex cache-expiry implementation to RelayKit DTO/type/conversion
  APIs while retaining exact `/v1/responses` eligibility, positive Codex
  identification, `prompt_cache_key` then `Session_id` lineage, Redis no-op and
  fail-open behavior, configurable enabled/cycle settings, billing projection,
  audit fields, and streaming/non-streaming handling.
- Move the fork's prompt-cache settings UI and translations into the flattened
  `web` source tree and keep it reachable from system settings.
- Preserve `docker-compose.override.yml` and the fork GHCR image workflow.
- Do not start, restart, or mutate deployed services, databases, or production
  Redis during the repository synchronization.
- Commit, archive only this sync task, record the session, push `dev`, and
  verify the remote SHA.

## Acceptance Criteria

- [x] `main`, `origin/main`, and the fixed upstream target resolve to the same
      commit.
- [x] `git merge-base --is-ancestor afe16c64 dev` succeeds with no unresolved
      paths or conflict markers.
- [x] Focused cache-expiry tests cover owner, in-cycle, fail-open, terminal SSE,
      response projection, billing usage, audit, and configurable TTL behavior.
- [x] Backend formatting, module consistency, build, full tests, and focused
      cache-expiry race tests pass for both the root module and RelayKit where
      applicable; any full-repository vet baseline from the fixed upstream
      target is reproduced and documented separately.
- [x] Frontend frozen dependency install, format/copyright checks, typecheck,
      tests, production build, and changed-file lint pass from `web`; any
      full-repository lint baseline is documented separately.
- [x] GHCR override/workflow and the cache settings UI remain present.
- [x] `git diff --check` reports no whitespace errors.
- [x] The verified merge and Trellis lifecycle commits are pushed to
      `origin/dev`, and remote `dev` equals local `HEAD`.

## Out of Scope

- Live provider requests or production cache-cycle validation.
- Altering the product policy of the two existing prompt-cache tasks.
- Archiving or rewriting those existing feature tasks.
- Force-pushing or rebasing either fork branch.
