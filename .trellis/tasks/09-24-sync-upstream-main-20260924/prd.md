# Merge upstream main into dev

## Goal

Merge the frozen `upstream/main` target `6c14c0762` into `dev`, preserve all fork commits and behavior, validate the resulting tree, create a merge commit, and push it to `origin/dev`.

## Requirements

- Protect the nine existing uncommitted Trellis file changes with a temporary stash; do not include them in the merge commit, and restore them after push verification.
- Use a normal non-rewriting merge. Do not reset, rebase, force-push, or replace the `dev` branch with upstream.
- Resolve conflicts by retaining both sides' intended behavior, with explicit review of `AGENTS.md`, Responses relay files, billing usage logic, and all locale conflicts.
- Preserve the 23 commits unique to `dev`, including prompt-cache-expiry billing and its admin configuration, deployment files, and Trellis history.
- Run backend, RelayKit, frontend, and focused regression checks appropriate to the merged tree before pushing.
- After pushing, verify the frozen upstream target and the merge commit are present on `origin/dev`; then restore and verify the pre-existing worktree changes.

## Acceptance Criteria

- [ ] `6c14c0762` is an ancestor of `dev`, and all pre-merge `dev`-only commits remain ancestors.
- [x] The merge has no unresolved conflict markers, and `git diff --check` passes.
- [x] Backend, RelayKit, frontend type/build/test checks and focused prompt-cache billing checks pass.
- [ ] A merge commit is created and pushed to `origin/dev`; local and remote `dev` point to the same commit.
- [ ] The nine pre-existing uncommitted Trellis changes are restored and remain outside the merge commit.
- [ ] No production service, database, Redis instance, or real upstream provider is contacted.

## Notes

- Keep `prd.md` focused on requirements, constraints, and acceptance criteria.
- Lightweight tasks can remain PRD-only.
- For complex tasks, add `design.md` for technical design and `implement.md` for execution planning before `task.py start`.

## Validation Record

- `GOWORK=off go test ./...` passed.
- `cd relaykit && GOWORK=off go build ./... && GOWORK=off go test ./...` passed.
- `cd web && npx --yes bun install --frozen-lockfile` completed without changing the lockfile.
- `cd web && npx --yes bun x vitest run --maxWorkers=4` passed: 166 files, 2096 tests.
- `cd web && npx --yes bun run typecheck` passed.
- `cd web && npx --yes bun run build` passed.
- Conflict-marker scan, `git diff --cached --check`, and JSON parsing for all seven frontend locales passed.
- Gemini prompt-cache stream regression, including abnormal EOF, passed; service prompt-cache and Responses usage tests passed.
- `format:check` and `copyright:check` report upstream frontend files outside the conflict-resolution edits. No repository-wide rewrite was performed.
