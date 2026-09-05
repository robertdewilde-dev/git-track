# git-track

Branch-scoped work metadata stored inside Git itself. No files in the working
tree, no GitHub dependency — state lives in `refs/meta/branches/<branch>` as a
chain of commits and syncs across machines with normal `git push` / `git fetch`.

Built for agentic workflows: every read command has `--json`, exit codes are
stable, a built-in MCP server exposes the metadata as native agent tools, and
any tool in any language can consume the data with two plain `git` calls (see
[SPEC.md](SPEC.md)). Agents working in this repo should start with
[CLAUDE.md](CLAUDE.md).

> **Prior art:** if you want a full distributed issue tracker (comments,
> identities, forge bridges), use [git-bug](https://github.com/git-bug/git-bug).
> git-track is the inverse shape: metadata keyed *by branch* — agent context,
> next steps, and a distributed lock — not issues as first-class entities.

## Install

```sh
go install github.com/robertdewilde-dev/git-track@latest
```

or build locally:

```sh
make build          # ./bin/git-track
make install        # go install
make cross          # all six linux/darwin/windows × amd64/arm64 binaries
```

Put `git-track` on your `PATH` and git exposes it as `git track`. `make
build`/`make install` also add a `track` symlink to the same binary, so
`track say "..."` and `git track say "..."` are the same command.

## Quick start

```sh
cd your-repo
git track init                          # refspecs + hooks (undo: git track init --undo)
git track doctor                        # is the remote's ref namespace usable?

git track set issue 42
git track set title "Refactor auth middleware"
git track set state in-progress
git track set next '["write tests", "update docs"]'

git track show
git track push                          # or just `git push` — the hook rides along
```

On another machine:

```sh
git track fetch
git track show
```

## Commands

```
git track init                     Configure refspecs + install hooks (--undo reverses)
git track set <key> <value>        Set a field (dot paths: agent.lockedBy)
git track set --from-json <file|-> Replace whole document (validated)
git track get [key]                Read one field or whole doc
git track unset <key>              Remove a field
git track import [issue]           Pull a GitHub issue into this branch (needs gh)
git track show [branch]            Human-readable summary
git track overview                 One-screen digest: branches, channels + unread, labels
git track list                     Table of all branches with metadata
git track log [branch]             History of metadata changes
git track lock / unlock            Acquire/release the agent lock
git track say <msg>                Post to a channel (-c main | -c <name>; default: this branch)
git track emit <type> [msg]        Post a typed event (--data JSON, --subject) to a channel
git track chat [channel]           Read a channel (--unread: only what's new; --type to filter)
git track watch [channel...]       Stream events live (--once --timeout awaits; --type --exec triggers)
git track search [text] [--label]  Find text/labels across metadata, chat, and commits
git track channels                 List channels (set/unset define, delete removes)
git track labels                   Label vocabulary with usage counts (show <name> lists uses)
git track push [branch]            Push metadata refs (--all for every branch)
git track fetch                    Fetch metadata refs
git track prune                    Delete metadata for branches that no longer exist
git track rename <old> <new>       Move metadata after a branch rename
git track squash [branch]          Collapse metadata history to one commit
git track worktree                 Show metadata for every active worktree
git track context [branch]         Markdown block for agent prompts
git track doctor                   Probe the remote for namespace support
git track mcp                      Run an MCP stdio server for agents
```

Global flags: `--json`, `--branch <name>`, `--quiet`, `--no-color`.

## For agents

- `git track overview --json` — start here: every branch, every channel with
  its unread count and last message, and the labels, in one call.
- `git track get --json` / `git track list --json` — machine output on stdout,
  valid JSON even on error (`{"error": "...", "code": 2}`).
- `git track context` — a prompt-ready markdown summary of the branch.
- `git track chat main --unread` — only what arrived since you last read it
  (cursor is per clone and per worktree, kept in the git dir).
- `git track search "token refresh"` / `--label bug` — find things without
  loading whole channels.
- `git track mcp` — a Model Context Protocol server over stdio. Example
  Claude Code registration:

  ```sh
  claude mcp add git-track -- git-track mcp
  ```

  Tools: `get_overview`, `get_branch_context`, `get_context_markdown`,
  `list_branches`, `set_field`, `unset_field`, `acquire_lock`,
  `release_lock`, `say`, `read_chat`, `wait_for_message`, `search`,
  `import_issue`, `list_channels`, `list_labels`, `define_label`,
  `define_channel`. Results are compact JSON and descriptions are terse: every word costs
  tokens on every session.

## Import from GitHub

One-way, per branch, a refresh on re-run. It goes through the GitHub CLI
(`gh`), so nothing else in git-track needs a token or a forge:

```sh
git checkout -b 42-refactor-auth
git track import                      # number from the branch name (or pass it: import 42)
git track show
```

| GitHub | git-track |
|---|---|
| number | `issue` |
| title | `title` |
| body | `context` |
| labels | `labels` (label descriptions become shared definitions) |
| url | `links` |
| CLOSED | `state: done` (OPEN sets `todo` only when state is unset) |

Without a number it uses the branch's `issue` field, then a number in the
branch name, then the issue the branch's open PR closes. Fields GitHub
doesn't know about (`next`, `agent.*`, custom ones) are left alone. No
write-back, no bulk import: other sources are a one-liner away through
`git track set --from-json -`.

## Channels: `say`, `chat`, `watch`

Agents (and you) can talk to each other — findings, decisions, questions,
progress — through channels stored in git, one commit per message under
`refs/meta/channels/<name>`:

```sh
git track say "auth refactor done; token tests still failing" --label finding
git track chat                        # read this branch's channel
git track say -c main "anyone free to review #42?" --label question
git track chat main                   # the shared coordination channel
git track say -c android "emulator flaky on API 35" --label bug
```

Three kinds of channel, one namespace:

- **`main`** — always there. Coordination, fan-out, questions: anything not
  tied to one branch lands here.
- **`branches/<branch>`** — every branch's own channel; the default for `say`
  and `chat`.
- **named** (`android`, `planning`) — topics that span branches.

Posting pushes immediately, and concurrent posts from other machines merge
automatically (messages are replayed onto the remote tip, never lost).
Offline posts stay local and merge on the next `say`, `watch`, or
`git track push --all`.

### Live: `watch`

Reading is pull-based, but you don't have to poll by hand:

```sh
git track watch                       # this branch + main, like tail -f
git track watch --all --json          # every channel, one JSON object per line
git track watch main --once --timeout 5m   # block until someone replies
```

`watch` polls the remote with one cheap `ls-remote` per interval (default
2s) and fetches only when a channel changed. Whatever exists when you start
counts as seen; `--tail 5` prints recent history first. In MCP the same loop
is the `wait_for_message` tool: an agent says something, then waits for the
reply — that's live agent-to-agent conversation with nothing but a git
remote in between.

### Events and triggers: `emit`, `watch --exec`

Every message is an event; a chat message is the event type `chat`. So the
same log, sync, `chat`, `watch`, and `search` serve typed events with a JSON
payload:

```sh
git track emit tests.failed "3 failures" -c main --data '{"count":3}' --subject feat/x
git track chat main --type tests.failed
```

Consumers see every event as a [CloudEvents 1.0](https://cloudevents.io)
envelope, and `watch --exec` is the trigger: a shell command run per event,
envelope on stdin, `TRACK_TYPE`/`TRACK_DATA`/`TRACK_CHANNEL`/... in the
environment. Handlers run one at a time, in order; a failing one is
reported, not fatal.

```sh
git track watch --all --type tests.failed --exec 'jq -r .data.count | xargs notify'
git track watch main --type review.request --exec './scripts/review.sh'
```

Any tool in any language can publish without the binary — one commit with
trailers plus one ref update (SPEC.md "Plain-git events") — and CI can post
results back with `emit` and read them with `chat`. Triggers are local on
purpose: what runs on a machine is that machine's flag, never something
pushed to the repository. This is the whole event layer: git is the bus,
the ref is the stream, cursors are consumer offsets; engines (Actions,
Temporal, n8n) sit on the other side of `--exec` or `watch --json`.

Merged branches need no cleanup — channels are independent refs; a branch's
channel stays as history and `git track prune` removes it once the branch is
gone (`main` and named channels are never pruned). `git track channels
delete <name>` removes a channel locally and remotely; it's a plain ref
delete, no consensus needed.

Labels are a shared, optional vocabulary — define one once with an
explanation and every machine sees it:

```sh
git track labels set bug "Something is broken for users"
git track channels set planning "Cross-branch planning notes"
git track labels                      # list with meanings
```

The same labels classify branch metadata (`git track set labels '["bug"]'`),
messages, and ordinary git commits — add a trailer and the commit joins the
vocabulary (git-track only reads it, it never writes your commits):

```sh
git commit -m "fix token refresh loop" --trailer "Label: bug"
git track labels                      # bug: 1 branch, 3 messages, 1 commit
git track labels show bug             # every branch, message, and commit carrying it
```

Undefined labels still work; you just get a hint.

## Staying cheap to read

Agents pay for every token they read, so reads are sized to what changed:

```sh
git track overview                    # branches, channels with unread counts, labels
git track chat main --unread          # only what's new since you last read it here
git track search "emulator"           # metadata + all channels, one git log
git track search --label bug          # + commits with a "Label: bug" trailer
```

Read cursors live in the git dir, per clone and per worktree, and are never
synced. Nothing is indexed or stored: the refs are the index, so there is
nothing to keep consistent.

## Locking

`git track lock` sets `agent.lockedBy` to `<user>@<machine>:<pid>` and pushes
with `--force-with-lease` — the push is the compare-and-swap, so two agents
racing for the same branch resolve cleanly: the loser exits `3`.

```sh
git track lock --ttl 30m     # auto-expires; a stale lock never wedges the branch
git track lock --force       # steal a lock
git track unlock
```

Writes to a branch locked by a different actor exit `3` unless `--force` is
passed.

## Exit codes

`0` success · `1` error · `2` no metadata · `3` lock held · `4` schema too new
· `5` ref conflict (fetch and retry). Details in [SPEC.md](SPEC.md).

## Configuration

```sh
git config track.states "todo,doing,done"            # allowed states
git config track.namespace refs/track-meta/branches  # if the server reserves refs/meta/*
git config track.remote upstream                     # metadata remote (default origin)
```

## Server support

Works against bare repos, Gitea/Forgejo, and GitLab. GitHub rejects custom ref
namespaces — run `git track doctor` to check yours. Gerrit reserves
`refs/meta/*`; use `track.namespace` to relocate.

## Design notes and limits

- **Snapshots + compare-and-swap, not an operation log.** Concurrent writers
  conflict loudly (exit `3`/`5`) instead of merging; coordination is the
  lock's job. Chosen deliberately over git-bug's Lamport-clock model — see
  SPEC.md "Design choice".
- Never writes to the working tree; never creates commits on `refs/heads/*`.
- Unknown JSON fields written by newer versions round-trip intact; a document
  with a newer `schemaVersion` is readable but refused for writes (exit `4`).
- Hooks are non-fatal and chain to any pre-existing hooks — a broken metadata
  ref never blocks a normal git operation.
- Shallow CI clones see no metadata (exit `2`) and must treat that as normal.
- Detached HEAD has no branch key: commands refuse unless `--branch` is given.
- Startup is a few milliseconds, safe to run on every checkout.

## Development

```sh
make test    # integration tests against real temporary repos
make vet
```
