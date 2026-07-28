# Implementation Plan: Sync upstream through afe16c64

1. Load Trellis, backend, frontend, billing, and fork-maintenance contracts.
2. Fast-forward `main` to `afe16c64`, push and verify `origin/main`, then merge
   `main` into `dev` without auto-committing.
3. Resolve the OpenAI Responses conflict around upstream RelayKit interfaces
   and the fork cache-expiry state machine.
4. Place the prompt-cache settings section in the flattened `web` tree and
   audit all registry, type, translation, and static-key references.
5. Normalize root/RelayKit modules and frontend dependencies; inspect generated
   and deleted paths for obsolete `web/default`, `web/classic`, `dto`, `types`,
   or host `service/relayconvert` assumptions.
6. Run focused cache tests first, then the full backend and frontend quality
   gates required by current upstream CI.
7. Audit merge ancestry, fork delivery files, conflict markers, whitespace, and
   final status; create one merge commit.
8. Archive only this sync task, record the work commit in the journal, push
   `dev`, and compare the remote SHA to local `HEAD`.

## Validation Commands

```bash
gofmt -w <changed Go files>
go mod tidy && GOWORK=off make test && go build ./...
go test -race <cache-expiry tests in service/openai/gemini>
(cd relaykit && GOWORK=off go mod tidy && go vet ./... && go build ./... && go test ./...)
(cd web && bun install --frozen-lockfile && bun run format:check && bun run copyright:check)
(cd web && bun run typecheck && bun test && bun run build)
(cd web && bunx oxlint -c .oxlintrc.json <changed frontend files>)
git diff --check
git merge-base --is-ancestor afe16c64 dev
```

Full-repository `go vet ./...` and `bun run lint` are also run as diagnostic
baselines. A failure that is not introduced by the merge resolution must be
recorded in `validation.md` with its scope and comparison evidence instead of
being hidden or expanded into an unrelated cleanup.

## Risky Files

- `relay/channel/openai/relay_responses.go`: direct content conflict and terminal
  streaming usage ownership.
- `relay/common/relay_info.go`, `service/billing_usage.go`, and
  `service/text_quota.go`: upstream type relocation can silently split response,
  billing, and log decisions even when compilation succeeds.
- `web/src/features/system-settings/**` and locale files: rename-aware merge
  must preserve discoverability and translated settings fields.
- `go.mod`, `go.sum`, `relaykit/go.mod`, and `relaykit/go.sum`: module boundary
  changes must remain tidy and reproducible.
