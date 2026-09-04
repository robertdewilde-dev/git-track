# git-track integration specification

This document is the integration contract. Any tool, in any language, can read
and write git-track metadata with plain `git` commands — the `git-track`
binary is a convenience, not a dependency. MCP-capable agents can also use
`git track mcp` (see "MCP server" below).

## Ref layout

Metadata for branch `<branch>` lives at:

```
refs/meta/branches/<branch>
```

The namespace prefix (`refs/meta/branches`) is configurable per repository via
`git config track.namespace` (e.g. `refs/track-meta/branches` for servers where
`refs/meta/*` is reserved, such as Gerrit). Branch names may contain slashes;
the ref is simply the prefix plus the branch name.

Each ref points to a **commit object** whose tree contains exactly one file:

```
meta.json
```

Every state change is a new commit with the previous state as its sole parent.
The first write has no parent. Commit messages describe the change
(e.g. `set state`) but carry no semantics — only `meta.json` does.

Because the state is a commit chain:

- `git log <ref>` shows the full history of changes,
- a push is fast-forward when (and only when) the writer saw the current
  remote state, so conflicting writes fail loudly instead of clobbering,
- `git push --force-with-lease=<ref>:<expected>` is a compare-and-swap.

### Reading metadata (two git calls)

```sh
git fetch origin '+refs/meta/branches/*:refs/meta/branches/*'
git cat-file blob refs/meta/branches/my-branch:meta.json
```

The second command prints the JSON document. If the ref does not exist, the
branch has no metadata.

### Writing metadata (plain git)

```sh
BLOB=$(git hash-object -w --stdin < new-meta.json)
TREE=$(printf '100644 blob %s\tmeta.json\n' "$BLOB" | git mktree)
PARENT=$(git rev-parse -q --verify refs/meta/branches/my-branch) || PARENT=
COMMIT=$(git commit-tree "$TREE" ${PARENT:+-p $PARENT} -m "update")
git update-ref refs/meta/branches/my-branch "$COMMIT" "${PARENT:-0000000000000000000000000000000000000000}"
git push --no-verify --force-with-lease=refs/meta/branches/my-branch:"$PARENT" \
    origin "$COMMIT":refs/meta/branches/my-branch
```

The `update-ref` old-value argument and the `--force-with-lease` expectation
give you the same conflict detection git-track uses. A rejected push means
another writer got there first: fetch, re-read, retry.

## Design choice: snapshots, not an operation log

git-track stores **state snapshots** with compare-and-swap conflict detection.
Concurrent writers do not converge — the loser is rejected (exit `3` for lock
races, `5` for pushes) and must fetch and retry. This is a deliberate
tradeoff, made with git-bug's operation-log model (Lamport clocks + DAG
topology + hash tiebreak, which merges concurrent edits instead of rejecting
them) as the known alternative:

- the metadata is small, coordination runs through the `agent.lockedBy` mutex,
  and a lost race costs one cheap retry;
- snapshots are readable with one `git cat-file`, keeping the two-call
  integration promise above;
- the op-log model is roughly five times the code for convergence this
  use case does not need.

If multi-writer convergence ever becomes a requirement, git-bug's
`doc/model.md` describes the upgrade path.

## Schema

`meta.json` is a JSON object. Current `schemaVersion` is **1**.

```json
{
  "schemaVersion": 1,
  "issue": 42,
  "title": "Refactor auth middleware",
  "state": "in-progress",
  "context": "Extracted token validation into its own package",
  "next": ["write tests", "update docs"],
  "labels": ["backend", "security"],
  "links": ["https://forgejo.local/me/proj/issues/42"],
  "agent": {
    "lastRun": "2026-09-03T10:22:00Z",
    "machine": "robert-desktop",
    "lockedBy": null,
    "lockedAt": "2026-09-03T10:22:00Z",
    "lockTtl": "30m",
    "notes": "freeform scratchpad for agent state"
  },
  "updatedAt": "2026-09-03T10:22:00Z",
  "updatedBy": "robert@robert-desktop"
}
```

Field rules:

| Field | Type | Notes |
|---|---|---|
| `schemaVersion` | integer | Required. Writers must refuse to modify a document with a version higher than they support. |
| `issue` | integer | Optional. |
| `title`, `context` | string | Optional. |
| `state` | string | Optional. Validated against `git config track.states` (comma/space separated); default set: `todo`, `in-progress`, `blocked`, `review`, `done`. |
| `next`, `labels`, `links` | array of strings | Optional. `labels` uses the shared vocabulary (see "Channels and labels"). |
| `agent` | object | Optional. Freeform, plus the lock fields below. |
| `agent.lockedBy` | string or null | `<user>@<machine>:<pid>` while locked. |
| `agent.lockedAt` | string | RFC 3339 time the lock was acquired. |
| `agent.lockTtl` | string | Go duration (`30m`, `2h`). A lock older than its TTL is treated as free by readers. |
| `updatedAt` | string | RFC 3339, set automatically on every write. |
| `updatedBy` | string | `<user>@<machine>`, set automatically on every write. |
| `agent.machine` | string | Set automatically when an `agent` object exists. |

**Unknown fields must round-trip intact.** A writer must read the full
document, modify only the fields it understands, and write everything back.
Never drop data written by a newer version.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | General error (invalid input, validation failure, not a repo, …) |
| 2 | No metadata for this branch (or field not present) |
| 3 | Lock held by another actor |
| 4 | Document `schemaVersion` newer than the binary supports |
| 5 | Ref conflict (non-fast-forward); fetch and retry |

## Output streams

- With `--json`, machine-readable JSON goes to **stdout** and nothing else does.
  Human messages go to **stderr**.
- `--json` output is valid JSON even on failure:
  `{"error": "<message>", "code": <exit code>}`.

## Locking protocol

1. Read the current document and metadata commit `P`.
2. If `agent.lockedBy` is set, not yours, and not expired per `lockTtl` → the
   lock is held (exit 3).
3. Write a new commit (parent `P`) with `agent.lockedBy = <user>@<machine>:<pid>`
   and `agent.lockedAt = now`.
4. Push with `--force-with-lease=<ref>:P`. Rejection means another actor won
   the race: roll the local ref back and report the lock as held (exit 3).

The push is the mutex: only one writer's lease against `P` can succeed.

## Channels and labels

Channels are async message streams for agents (and humans) to leave findings,
decisions, and progress notes — a timeline, not a document. Each channel lives
at:

```
refs/meta/channels/<name>
```

(the prefix follows `track.namespace`: its parent plus `/channels`). **One
commit per message**: the commit message is the message. `git log
refs/meta/channels/android` literally is the chat log. Message commits have an
empty tree and are formatted as a body, a blank line, then git trailers:

```
Emulator is flaky on API 35; pinning to API 34 for now.

Label: bug
Label: android
By: robert@robert-desktop
```

`Label:` may repeat; `By:` identifies the actor (the git author is a
fallback). Channel names are ref-relative paths in one namespace:

| Channel | Purpose |
|---|---|
| `main` | Always exists (listed even before its first message). Shared coordination: questions, fan-out, announcements. |
| `branches/<branch>` | The branch's own channel; default for `say`/`chat`/`context`. Moved by `git track rename`, removed by `git track prune` once the branch is gone. |
| any other name | Named topic channel spanning branches (`android`, `planning`). Never pruned. |

`git track say` posts, `git track chat` reads (`--since <sha>` for
catch-up), and `git track watch` streams: it polls the remote with one
`ls-remote` on the channel namespace per interval (no object transfer),
fetches a channel only when its tip changed, and prints the messages that
landed after the watch started. `--once` + `--timeout` turn it into a
blocking wait for a reply; the MCP `wait_for_message` tool is the same loop.
There is no push notification — git has no post-fetch hook and server-side
hooks cannot reach clients — so liveness equals the poll interval.

`git track channels delete <name>` deletes the channel ref locally and on the
remote. No consensus: it is a plain ref delete by anyone with push access.
Other clones keep their local copy; a later post from such a clone recreates
the channel with its old history. Merging a branch needs no channel work —
channels are independent refs.

Posting appends a commit to the channel ref (CAS on the old tip) and pushes
immediately with `--force-with-lease`. On rejection the poster fetches the
remote tip and **replays its local-only message commits on top of it** —
concurrent posts merge instead of conflicting; a message, once committed, is
never lost. If the remote is unreachable the message stays local and merges on
the next `say` or `git track push --all`.

Label and channel **definitions** — a shared, optional vocabulary, each name
mapped to a one-line description — live in a single document `defs.json` on:

```
refs/meta/defs/all
```

```json
{
  "schemaVersion": 1,
  "labels":   { "bug": { "description": "Something is broken for users" } },
  "channels": { "planning": { "description": "Cross-branch planning notes" } }
}
```

The same labels classify branch documents (the `labels` field), messages
(`Label:` trailers), and **ordinary git commits**: a commit whose message
carries a `Label: <name>` trailer (`git commit --trailer "Label: bug"`) is
part of the vocabulary too. git-track never writes such commits (it never
touches `refs/heads/*`); it only reads them. Definitions are documentation,
not enforcement: undefined labels and channels work; the CLI prints a stderr
hint when a label is used that has no definition while a vocabulary exists.

`git track labels` lists every label that is defined *or* in use, with
counts of branches, messages, and commits carrying it; `git track labels
show <name>` (= `git track search --label <name>`) lists those uses.

### Read cursors, overview, search

Reads are meant to cost tokens in proportion to what changed, not to what
exists:

- **Cursors.** Every unfiltered `chat` read (and `watch`, and MCP
  `read_chat`/`wait_for_message`) records the channel tip in
  `<git-dir>/track/cursors/<channel>`. `chat --unread` returns only what
  landed after it. Because the path resolves through `git rev-parse
  --git-path`, cursors are per clone *and per worktree*; they are never
  synced. A poster whose cursor was at the tip has their own post marked
  read. A cursor pointing at a pruned object counts as "never read".
- **`git track overview`** (MCP `get_overview`) is the one-call digest:
  every branch with metadata (state, issue, title, labels, lock), every
  channel with message count, unread count, and last message, and the label
  vocabulary. Generated on demand from local refs — nothing is stored, so
  there is no index to keep consistent.
- **`git track search [text] [--label <name>]`** (MCP `search`) runs one
  `git log --grep` across all channel refs (case-insensitive literal text
  and/or an anchored `^Label: <name>$` line) plus a walk over branch
  documents (any string field; the match reports the dot path in
  `matched`). With `--label`, commits on local and remote-tracking branches
  carrying the trailer are included too. Text without a label does not
  search commit messages — that is `git log --grep`.
- MCP tool results are compact JSON (no indentation).

**Sync path:** channel and defs refs are deliberately *not* added to the
configured refspecs. A channel ref with unsent local messages is not
fast-forwardable, and a configured non-forced refspec would make plain
`git fetch` exit non-zero — so these refs sync only through the tool:
`say` (auto-push), `watch` (syncs unsent messages on channels it watches),
`git track push --all`, and `git track fetch` (which fast-forwards channels,
force-updates defs, and reports channels holding unsent local messages
instead of clobbering them).

## MCP server

`git track mcp` speaks the Model Context Protocol over stdio (JSON-RPC 2.0,
one message per line, protocol version `2024-11-05`). Register it in an agent
client as command `git-track`, args `["mcp"]`, run from inside the repository.

Tools: `get_overview`, `get_branch_context`, `get_context_markdown`,
`list_branches`, `set_field`, `unset_field`, `acquire_lock`, `release_lock`,
`say`, `read_chat` (`unread` flag), `wait_for_message`, `search`,
`list_channels`, `list_labels`, `define_label`, `define_channel`.
Branch-scoped tools accept an optional `branch` argument, defaulting to the
checked-out branch. Results are compact JSON; errors come back as tool
results with `isError: true` and include the CLI exit code. Tool
descriptions are kept terse on purpose: they are loaded into the agent's
context on every session.

## Sync configuration

`git track init` adds to `.git/config` (values it adds are recorded under
`track.init.added` so `git track init --undo` can restore the config exactly):

```ini
[remote "origin"]
    fetch = +refs/meta/branches/*:refs/meta/branches/*
    push = refs/heads/*
    push = refs/meta/branches/*
```

`push = refs/heads/*` is only added when the remote has no push refspecs yet —
an explicit push refspec overrides `push.default`, so without it normal
`git push` would stop pushing branches.

Hooks installed (respecting `core.hooksPath`; a pre-existing hook is renamed to
`<name>.pre-git-track` and chained, never overwritten):

- `post-checkout` — prints the metadata summary on branch switch; silent when
  there is none; never fails.
- `pre-push` — pushes metadata refs alongside (`git track push --all`), unless
  the outgoing push already carries them via the refspec; never fails.

## Configuration keys

| Key | Default | Meaning |
|---|---|---|
| `track.namespace` | `refs/meta/branches` | Ref prefix for metadata. |
| `track.states` | `todo,in-progress,blocked,review,done` | Allowed `state` values. |
| `track.remote` | `origin` | Remote used for push/fetch/lock. |

## Known limitations and edge cases

- **Shallow / partial clones (CI):** `git clone --depth 1` does not fetch
  custom refs. Readers must treat exit `2` / a missing ref as "no metadata",
  not an error. Run `git track fetch` (or the fetch refspec) to populate.
- **Detached HEAD:** metadata is keyed by branch name, so a detached worktree
  has no implicit key. Commands refuse with exit `1`; pass `--branch <name>`
  explicitly. The `post-checkout` hook stays silent.
- **Worktrees share the ref store:** all worktrees of a repository see the
  same metadata; "per-worktree" and "per-branch" state are the same thing.
- **Branch rename/delete:** `git branch -m` does not move the metadata ref —
  run `git track rename <old> <new>`. Deleted branches leave orphaned refs;
  `git track prune` cleans them up.
- **Object growth:** every write creates a commit+tree+blob. Chatty agents
  should batch writes, and `git track squash` collapses a branch's metadata
  history to a single commit. Channel messages are one commit each by design
  (the history *is* the data); message commits share the empty tree, so growth
  is one small object per message.
- **No trust model:** anyone with push access can rewrite metadata history.
  Sign metadata commits if that matters; out of scope here.

## Server compatibility

Bare repos, Gitea/Forgejo, and GitLab accept custom ref namespaces. GitHub
rejects most of them. Gerrit reserves `refs/meta/*`. `git track doctor` probes
the remote by pushing and deleting a throwaway ref under the namespace and
reports whether it is usable; switch namespaces with `git config
track.namespace` and re-run `git track init` if not.
