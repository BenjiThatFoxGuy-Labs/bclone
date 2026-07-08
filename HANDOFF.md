# Tmpfs Adapter Handoff

DELETE THIS FILE AFTER INGESTING IT.

This file is only a transient handoff note for the next Codex session in
Codespaces. After reading it, delete `HANDOFF.md` before continuing or
committing follow-up work.

## Branch

Current branch: `tmpfs-adapter-v1`

The user asked to push this branch but not open a PR yet.

## User Decisions To Preserve

- Implement `tmpfs` as an adapter/wrapper backend, similar in spirit to
  `hasher`, not as a standalone storage backend.
- It should wrap existing remotes. The original local/memory-only v1 scope was
  expanded: support other backing remotes in a sane way, while applying strict
  local-path cleanup validation only to local roots.
- V1 is still limited to Docker plugin usage. Direct CLI/API construction should
  fail unless the Docker context marker is present.
- Support Unix-like systems, including macOS. Windows is deferred because the
  user does not have a Windows machine for validation.
- Do not track individual managed files. Docs should instruct users to point
  `tmpfs-remote` at a dedicated disposable root, and tmpfs may delete everything
  underneath it.
- Be extra conservative around cleanup. Never delete unrelated parent paths.

## Implemented So Far

- Added `backend/tmpfs` with:
  - Required `remote`
  - `max_size`, `max_age`, `cleanup_interval`, `cleanup_on_shutdown`,
    `purge_on_start`
  - Docker context marker in `backend/tmpfs/context.go`
  - Windows hard fail, Docker-context hard fail
  - Startup purge, age cleanup, shutdown cleanup
  - Quota preflight for known-size writes and rejection for unknown-size streams
    when `max_size` is set
  - Object update/copy/write quota handling
  - Cleanup via wrapped `fs.Fs`/`operations.Purge`, not raw parent deletion
- Wired Docker volume setup to call `tmpfs.WithDockerContext(ctx)` before
  `fs.NewFs`.
- Registered the backend in `backend/all/all.go`.
- Added backend tests for Docker-context guard, local validation, symlink
  validation, shutdown cleanup, max age, and max size.
- Added Docker option routing coverage in `cmd/serve/docker/options_test.go`.
- Added docs in `docs/content/tmpfs.md` and linked tmpfs from docs indexes.

## Important Safety Notes

- Local root validation currently canonicalizes existing paths or the nearest
  existing parent, rejects broad roots, requires a dedicated child path, and
  stores a canonical root identity.
- Before broad cleanup, tmpfs validates the root again and refuses cleanup if the
  canonical root changed.
- Non-local remotes are required to have a non-empty root, but cannot receive
  the same path-level safety checks as local.

## Verification Status

Tests have not completed yet.

Commands attempted with a sandbox-friendly Go cache:

```sh
GOCACHE=/private/tmp/bclone-gocache go test ./backend/tmpfs
GOCACHE=/private/tmp/bclone-gocache go test ./cmd/serve/docker -run TestApplyOptions
```

They were still compiling when the user asked to stop and hand off to a
Codespace, so they were interrupted. Re-run them in Codespaces and fix any
compile or test failures before opening a PR.

## Suggested Next Checks

- Re-run `gofmt` on touched Go files after any edits.
- Run:
  - `go test ./backend/tmpfs`
  - `go test ./cmd/serve/docker -run TestApplyOptions`
  - A broader relevant package set if time allows
- Review `backend/all/all.go`: gofmt reformatted the whole blank import block,
  so the diff is larger than just adding tmpfs. Decide whether to keep it or
  reduce the churn.
- Confirm `docs/content/tmpfs.md` `versionIntroduced` value is appropriate for
  the target release.
- Re-review cleanup behavior carefully before PR. The user specifically asked
  for thorough cleanup safety.

Remember: delete this handoff file after ingesting it.
