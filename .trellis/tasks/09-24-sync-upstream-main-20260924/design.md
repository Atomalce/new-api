# Design

## Branch and history

Merge the frozen `upstream/main` commit `6c14c0762` into the current `dev` branch with a normal merge commit. Do not rewrite history. Preserve all 23 commits currently unique to `dev` and confirm the full pre-merge `dev` tip remains an ancestor after the merge.

## Worktree protection

The nine pre-existing modified Trellis files are unrelated to upstream integration. Stash them before starting the merge, keep the stash until all post-push checks pass, then restore it and verify the same files remain modified and are not part of the merge commit.

## Conflict handling

The merge preflight identified conflicts in `AGENTS.md`, `relay/channel/gemini/relay_responses.go`, `relay/channel/openai/relay_responses.go`, `service/billing_usage.go`, and seven locale JSON files. Resolve each against the merge base and both sides' behavior. The local prompt-cache expiry billing must remain wired through all Responses paths; upstream billing usage canonicalization and relay conversions must also remain intact. Locale files must retain both sets of keys with valid JSON. Keep upstream project identifiers and governance rules.

## Delivery and rollback

Run required validation before creating the merge commit. Do not push if validation has an unexplained failure. Push only the resulting merge commit to `origin/dev`, then verify the remote SHA. Do not force-push or reset. If merge resolution or validation cannot be completed, preserve the branch state and report the exact blocker; keep the stash intact.
