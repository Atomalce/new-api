# Design: Sync upstream through afe16c64

## Merge Strategy

Use the repository's two-branch fork model. Fast-forward `main` to the fixed
upstream target, push it, then merge `main` into `dev` with `--no-ff` and stop
before committing so the resolved tree can be reviewed and validated.

## Compatibility Boundaries

### RelayKit extraction

Follow upstream ownership of generic DTOs, relay information, response
conversion, and protocol types in the nested `relaykit` module. Keep the
fork-specific policy orchestration in the host service/relay layers. Resolve
imports and types against RelayKit instead of copying removed host packages
back into the tree.

The policy decision must remain request-scoped and must be applied to the same
usage object that is exposed, billed, and logged. Preserve the original usage
snapshot for cache-inference and audit reconciliation.

### Frontend flattening

Accept the upstream rename from `web/default/*` to `web/*`. Port the one
add/add location conflict to
`web/src/features/system-settings/general/prompt-cache-expiry-settings-section.tsx`
and rely on Git's rename-aware merge for registry, types, and locale changes.
Do not restore the deleted Classic frontend.

### Fork delivery

Leave upstream `docker-compose.yml` intact and retain the fork-owned
`docker-compose.override.yml` plus GHCR `dev` workflow.

## Conflict Resolution Rules

- `relay/channel/openai/relay_responses.go`: combine upstream RelayKit APIs and
  current response flow with the cache-expiry decision/projection hooks; no
  whole-file ours/theirs resolution.
- `go.mod` / `go.sum`: accept the merged module topology and normalize with
  `go mod tidy` in the relevant modules.
- Generated/renamed frontend paths: follow upstream layout, then run the
  repository's Bun scripts instead of restoring obsolete directories.
- Brand, attribution, and protected project metadata remain untouched.

## Validation and Rollback

No live service mutation is required. If compatibility cannot be proven by
build/tests, abort before committing and inspect the merge state. Before a
commit exists, `git merge --abort` is the rollback point; after committing, use
a normal revert rather than history rewriting.
