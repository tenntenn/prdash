# CLI and integration reference

Use the [prdash Skill](../skills/prdash/SKILL.md) for the normal agent workflow. This reference is for manual operation and integration authors.


### Starting the Web Dashboard & Using the CLI

```console
$ prdash                               # auto-detects git repo, worktrees, and open GitHub PRs
$ prdash status                        # show repository summary (worktrees, sessions, PRs)
$ prdash sync                          # refresh git worktrees and open GitHub PRs
$ prdash wait --session controller     # block until a human instruction arrives
$ prdash instructions --session sess-1 # list unhandled instructions for a session
$ prdash report --pr 42 -m "Fixed CI"  # legacy status report; does not acknowledge instructions
$ prdash send --pr 42 -m "Run tests"   # queue an instruction for a PR session
```

Repository state is persisted automatically in `$XDG_STATE_HOME/prdash/state-<owner>__<name>.json` (or `~/.local/state/prdash/...`), so the dashboard, CLI and MCP can share the selected file. Updates use a cross-process file lock and reload the current revision before mutation. `prdash instruction -server` remains a convenient way for workers to use a running server.

On startup (and whenever **Sync GitHub & Worktrees** is clicked in the header, `prdash sync` is run, or `sync_repository` is called via MCP), `prdash` automatically:
1. Detects the repository (`owner/name`, default branch, local root) via `git`.
2. Discovers local git worktrees via `git worktree list --porcelain` and checks working tree status.
3. Fetches open pull requests from GitHub via `gh pr list`, binds them to matching worktrees by branch, and infers stacked PR chains (`Stack 1/N → 2/N → ...`) from `baseRefName` and `headRefName`.

`prdash` binds an available local port and prints the dashboard URL:

```text
prdash listening on http://127.0.0.1:<port> (repository: owner/name)
```

### Running as an MCP Server

The same core logic can be exposed as a JSON-RPC 2.0 Model Context Protocol (MCP) server over stdio:

```json
{
  "mcpServers": {
    "prdash": {
      "command": "prdash",
      "args": [
        "-mcp",
        "-repo",
        "owner/name",
        "-state",
        "/tmp/prdash-state.json"
      ]
    }
  }
}
```

The web and MCP processes may share the same canonical `-state` path. State mutations and instruction claims are serialized across processes using that file's sidecar lock. All writers must use this version of prdash and the same state file.

### CLI Subcommands and Flags

```console
$ prdash [flags]                        # start the web dashboard (or MCP server with -mcp)
$ prdash skill [--list | --install <dir> [--force]]
$ prdash --version
```

| Flag | Default | Description |
| :--- | :--- | :--- |
| `-repo` | *(auto-detected)* | Target repository in `owner/name` format (detected from `git remote` if omitted) |
| `-dir` | `.` | Local git repository directory to inspect |
| `-no-sync` | `false` | Skip automatic `git worktree` and `gh pr list` discovery on startup |
| `-addr` | `127.0.0.1:0` | HTTP listen address (`:0` selects a free port automatically) |
| `-state` | *(repository-scoped state file)* | JSON state path; use `off` for in-memory operation |
| `-mcp` | `false` | Run as a JSON-RPC 2.0 Model Context Protocol (MCP) server over stdio |
| `-model-pine` | `claude-opus-4-6` | Concrete model identifier for the **Pine (High)** preset |
| `-model-bamboo` | `claude-sonnet-4-6` | Concrete model identifier for the **Bamboo (Standard)** preset |
| `-model-plum` | `claude-haiku-4-5` | Concrete model identifier for the **Plum (Fast)** preset |
| `-version` | `false` | Print version and exit |

### Using as a Go Package (`github.com/tenntenn/prdash/prdash`)

```console
$ go get github.com/tenntenn/prdash/prdash@latest
```

```go
package main

import (
	"net/http"

	"github.com/tenntenn/prdash/prdash"
)

func main() {
	svc := prdash.NewService(prdash.Options{
		Repository: "owner/repo",
		StateFile:  "/tmp/prdash-state.json",
	})
	http.ListenAndServe("127.0.0.1:0", prdash.NewHandler(svc))
}
```


### Claim and report an individual instruction

Connect HTTP workers to the running dashboard server. MCP/Go clients may also
share its persistent state file; claims are serialized across these processes.
Read queued IDs with `GET /api/sessions/<session>/instructions`, then:

```sh
prdash instruction claim -server http://127.0.0.1:8080 -session sess-1 -id smsg-123 -consumer worker-1
# Save instruction.attempt, instruction.target_commit and claim_token from JSON.
prdash instruction result -server http://127.0.0.1:8080 -session sess-1 -id smsg-123 -consumer worker-1 -attempt 1 -target-commit <claimed-commit> -claim-token <token> -outcome completed -status waiting_for_human -m "Tests passed"
prdash instruction get -server http://127.0.0.1:8080 -session sess-1 -id smsg-123
```

A claim captures the instruction's assigned session, target commit, consumer and
attempt. PR targets come from GitHub's head SHA (or an explicitly assigned local
worktree); if unknown, supply `-target-commit` when claiming. Results refer to that
input commit, even if execution produces a new commit. Only the latest instruction
for the still-assigned session and matching current target can change PR status;
stale results remain in history. Results never infer CI success or resolve conflicts.

States are `queued`, `delivered`, `running`, `completed` and `failed`.
`instruction ack` acknowledges only delivery of its specific ID. A delivered
instruction can still be claimed. Legacy `report` and status changes do not ack
instructions; legacy bulk ack is delivery-only and should not be used by parallel
consumers. Claim tokens are returned only to claim callers and are omitted from
dashboard snapshots; `PRDASH_CLAIM_TOKEN` can supply a token without a command-line
argument.

Distinct consumers must use distinct IDs. Persist the consumer ID and claimed ID
before executing external effects. After a crash, use `get` and reissue `claim`
with the same consumer ID to recover a running claim. A claim never expires
automatically, because the original process may still be running. A terminal
result is immutable; resending exactly the same result is idempotent. Execution
itself is not an exactly-once transaction: inspect prior external effects after a
crash and use `instruction_id + attempt` as an external idempotency key.

Report `-outcome failed` to record a failed attempt. After checking that execution
has stopped, use `instruction retry` with the failed claim's consumer, token and
attempt to requeue it. The next claim receives a new token and attempt; old
results are rejected and previous results remain in history.

HTTP endpoints use `/api/sessions/{id}/instructions/{instruction}` with `GET` for
inspection and `POST` suffixes `/ack`, `/claim`, `/result`, `/retry`. MCP exposes
`get_instruction`, `ack_instruction`, `claim_instruction`,
`report_instruction_result`, and `retry_instruction` on its owning Service.

### Shared state transactions and errors

Each mutation acquires a lock at `<canonical-state-path>.lock`, reloads the latest
revision, applies the update, and commits a private temporary file with an atomic
rename. Readers never see partially written JSON. External GitHub/sbnn work runs
before this short state transaction. Different state paths have independent locks;
symlink aliases are resolved to the same path. Leave the sidecar file in place:
the OS releases its lock when a process exits. Lock waits fail after 10 seconds.
Use a local filesystem with working OS file-lock and rename semantics.

Revision rollback and same-revision content changes are rejected. The lock
coordinates current prdash writers; arbitrary file editors and older versions do
not participate. Do not edit the state file while services are running. Upsert
operations still replace explicitly supplied fields; locking does not turn them
into merge patches.

The snapshot exposes `state_path` and `state_error`. Go callers can inspect
`StatePath()` and must check `StateError()` after `NewService`. Mutations return
load, lock, serialization, write and rename errors and roll back their in-memory
changes on save failure. CLI commands exit with an error; HTTP/MCP mutations also
report the error. Corrupt files are preserved rather than silently replaced with
empty state. Correct the file or restore a backup before retrying. The deprecated
`AckInstructions` API returns zero on failure; use `AckInstructionsChecked` or
exact-ID acknowledgment when an error result is required.

A failed request is not proof that an external action did not run. Inspect the
instruction and external system before retrying side effects; a result commit may
also have completed before an unlock/connection error was observed. Files are
synced before rename, but the state file is not a distributed transaction with
GitHub or the worker's commands.
