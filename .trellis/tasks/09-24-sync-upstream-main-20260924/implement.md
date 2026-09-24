# Implementation Plan

1. Confirm branch, frozen upstream SHA, remote tip, original `dev` tip, and nine-file worktree state; record the original `dev`-only commits.
2. Start the Trellis task, stash only the existing worktree modifications, and verify the worktree is clean.
3. Merge `upstream/main` normally into `dev` and resolve all conflicts while preserving fork behavior and upstream functionality.
4. Inspect conflict resolutions, verify locale JSON parses, verify no conflict markers remain, and run `git diff --check`.
5. Verify upstream target and original `dev` tip are ancestors; inspect the prompt-cache-expiry runtime wiring and focused tests.
6. Run backend tests, independent RelayKit build/tests with `GOWORK=off`, and frontend test/typecheck/build commands.
7. If checks pass, create the merge commit, push to `origin/dev`, and verify local/remote SHA equality and ancestry.
8. Restore the original stash, verify all nine changes are present and excluded from the merge commit, then record final task validation and journal state.

## Validation commands

- `go test ./...`
- `cd relaykit && GOWORK=off go build ./... && GOWORK=off go test ./...`
- `cd web && bun run test && bun run typecheck && bun run build`
- Focused prompt-cache expiry billing tests identified from the current task artifacts and `service/` tests.
- `git diff --check`, locale JSON parse checks, ancestry checks, and post-push `git ls-remote` comparison.

## Stop conditions

- Do not push if a required check fails without an understood and recorded environment-only cause.
- Do not drop the stash until push verification and restoration checks complete.
- Do not rewrite `dev` or force-push.
