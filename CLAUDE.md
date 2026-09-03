# git-track — agent guide

Go CLI that stores per-branch metadata (state, context, next steps, agent
scratchpad, lock) as commit objects under `refs/meta/branches/<branch>`.
No working-tree files, no forge dependency; syncs via `git push`/`git fetch`.

## Using the tool as an agent

- Read state: `git track get --json` (whole doc) or `git track get state`.
  Exit `2` + `{"error":...,"code":2}` means no metadata yet — that is normal,
  not a failure.
- Write state: `git track set state in-progress`, `git track set next '["a","b"]'`,
  `git track set agent.notes "..."` (dot paths reach nested fields).
- Before changes another agent might race on: `git track lock --ttl 30m`;
  exit `3` means someone else holds it. `git track unlock` when done.
- Exit `5` on any push means fetch first: `git track fetch`, then retry.
- Prompt-ready summary: `git track context` (markdown).
- Preferred for MCP-capable agents: `git track mcp` runs a stdio MCP server
  (tools: get_branch_context, set_field, acquire_lock, ...).
- Full integration contract (ref layout, schema, exit codes, plain-git access):
  [SPEC.md](SPEC.md).

## Codebase map

| Path | What it is |
|---|---|
| `main.go` | Entry point; exits with `cmd.Execute()`'s code. |
| `cmd/` | Cobra subcommands, one file each. `root.go` has exit codes, global flags, the shared `mutateDoc` write path. `mcp.go` is the MCP stdio server. |
| `internal/gitcmd/` | The only place git is invoked. Swappable transport. |
| `internal/store/` | Read/write metadata commits to refs; push/fetch with force-with-lease. No CLI or output concerns. |
| `internal/schema/` | Versioned document, validation, dot paths, unknown-field passthrough. |
| `internal/lock/` | Lock identity (`user@machine:pid`), TTL expiry, actor comparison. |
| `internal/hooks/` | post-checkout / pre-push install with chaining. |
| `integration_test.go` | The real test suite: builds the binary, drives it against temp bare remotes and clones. |

## Invariants (do not break)

- Never write to the working tree; never create commits on `refs/heads/*`.
- Unknown JSON fields must round-trip through every write.
- A doc with `schemaVersion` > `schema.CurrentVersion` is readable, never writable (exit 4).
- Hooks must never fail a git operation.
- Exit codes 0–5 are a published contract.
- Startup must stay well under 10ms (runs on every checkout).

## Development

- `make test` — integration tests (require `git` on PATH).
- `make build` / `make cross` — local binary / all six OS-arch targets.
- Design choice: state snapshots + compare-and-swap pushes, **not** an
  operation log. Concurrent writers conflict loudly (exit 3/5) instead of
  merging. Chosen knowingly for solo/agent-lock use; see SPEC.md "Design
  choice" for the git-bug-style upgrade path if that ever changes.
