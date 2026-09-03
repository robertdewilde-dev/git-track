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

Put `git-track` on your `PATH` and git exposes it as `git track`.

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
git track show [branch]            Human-readable summary
git track list                     Table of all branches with metadata
git track log [branch]             History of metadata changes
git track lock / unlock            Acquire/release the agent lock
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

- `git track get --json` / `git track list --json` — machine output on stdout,
  valid JSON even on error (`{"error": "...", "code": 2}`).
- `git track context` — a prompt-ready markdown summary of the branch.
- `git track mcp` — a Model Context Protocol server over stdio. Example
  Claude Code registration:

  ```sh
  claude mcp add git-track -- git-track mcp
  ```

  Tools: `get_branch_context`, `get_context_markdown`, `list_branches`,
  `set_field`, `unset_field`, `acquire_lock`, `release_lock`.

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
