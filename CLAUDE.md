# git-track — agent guide

Go CLI that stores per-branch metadata (state, context, next steps, agent
scratchpad, lock) as commit objects under `refs/meta/branches/<branch>`.
No working-tree files, no forge dependency; syncs via `git push`/`git fetch`.

## Using the tool as an agent

- Start with `git track overview --json`: every branch, every channel with
  unread counts, the labels — one call.
- Read state: `git track get --json` (whole doc) or `git track get state`.
  Exit `2` + `{"error":...,"code":2}` means no metadata yet — that is normal,
  not a failure.
- Write state: `git track set state in-progress`, `git track set next '["a","b"]'`,
  `git track set agent.notes "..."` (dot paths reach nested fields).
- Before changes another agent might race on: `git track lock --ttl 30m`;
  exit `3` means someone else holds it. `git track unlock` when done.
- Exit `5` on any push means fetch first: `git track fetch`, then retry.
- Talk to other agents: `git track say "finding..." --label bug` posts to
  this branch's channel; `-c main` for the shared coordination channel
  (questions, fan-out), `-c <topic>` for named ones. `git track chat [channel]`
  reads; `git track watch --once --timeout 5m` (MCP: `wait_for_message`)
  blocks until someone replies. Concurrent posts merge automatically.
  `git track chat main --unread` reads only what is new for you (cursor per
  clone and worktree). Define labels/channels once with
  `git track labels set <name> "<meaning>"` / `channels set`.
- Events are the same log: `git track emit tests.failed "3 failures" --data
  '{"count":3}'` posts a typed event (chat is type `chat`); `chat --type X`
  filters; `watch --type X --exec '<cmd>'` runs a local handler per event
  with a CloudEvents envelope on stdin. Plain git can publish too (SPEC.md
  "Plain-git events").
- Find instead of read: `git track search "<text>"` (metadata + all
  channels) or `git track search --label bug` (also commits carrying a
  `Label: bug` trailer; add one with `git commit --trailer "Label: bug"`).
- On a GitHub-backed repo with `gh` installed: `git track import [issue]`
  fills issue/title/context/labels from the issue (number inferred from
  the branch name or its PR). One-way; re-run to refresh.
- Prompt-ready summary: `git track context` (markdown, includes recent chat).
- Preferred for MCP-capable agents: `git track mcp` runs a stdio MCP server
  (tools: get_branch_context, set_field, acquire_lock, ...).
- Full integration contract (ref layout, schema, exit codes, plain-git access):
  [SPEC.md](SPEC.md).

## Codebase map

| Path | What it is |
|---|---|
| `main.go` | Entry point; exits with `cmd.Execute()`'s code. |
| `cmd/` | Cobra subcommands, one file each. `root.go` has exit codes, global flags, the shared `mutateDoc` write path. `mcp.go` is the MCP stdio server (terse descriptions, compact JSON — on purpose). `overview.go`/`search.go`: the token-aware reads. `events.go`: `emit`, the CloudEvents envelope, and the `--exec` handler runner. `import.go`: GitHub issue import via the `gh` CLI (the only forge-touching code; optional at runtime). `watch.go` has the poll loop shared by `watch` and MCP `wait_for_message`. |
| `internal/gitcmd/` | The only place git is invoked. Swappable transport. |
| `internal/store/` | Read/write metadata commits to refs; push/fetch with force-with-lease. `channels.go`: message streams (one commit per message) + shared label/channel definitions. `search.go`: per-worktree read cursors (`<git-dir>/track/cursors/`), cross-channel search, label usage incl. commit trailers. No CLI or output concerns. |
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
- MCP tool descriptions stay terse and results compact: they are paid for in
  tokens on every agent session.
- Triggers (`watch --exec`) are local flags, never repository config: nothing
  pushed to a shared ref may decide what executes on another machine.

## Development

- `make test` — integration tests (require `git` on PATH).
- `make build` / `make cross` — local binary / all six OS-arch targets.
- Design choice: state snapshots + compare-and-swap pushes, **not** an
  operation log. Concurrent writers conflict loudly (exit 3/5) instead of
  merging. Chosen knowingly for solo/agent-lock use; see SPEC.md "Design
  choice" for the git-bug-style upgrade path if that ever changes.
