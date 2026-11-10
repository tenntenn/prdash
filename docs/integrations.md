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

Repository state is persisted automatically in `$XDG_STATE_HOME/prdash/state-<owner>__<name>.json` (or `~/.local/state/prdash/...`), so sequential invocations can reuse state without manual `-state` flags. Parallel consumers must use one running server via `prdash instruction -server`; independent processes writing the same file are not transactionally coordinated.

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

Use a single Service owner for instruction execution. A standalone MCP process can own state, but running it alongside a web server writing the same file does not provide atomic cross-process claims. Use HTTP instruction clients against the web server for parallel workers (shared-file transactions are tracked separately in #37).

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

Run one dashboard server as the state owner. Every parallel worker must use that
same server; do not launch an independent `-mcp`/state-file writer per consumer.
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

## Worktree validation

Worktrees retain full commit IDs and expose inspection states: `unknown`, `clean`,
`dirty`, `missing`, `prunable`, and `error`. Failed inspection never establishes
cleanliness; `status_error` contains the reason. Detached worktrees are marked
with `detached` and can be matched to a PR by its full `head_sha`.

An explicit PR worktree ID takes precedence over branch matching. Ambiguous,
unavailable, and commit-mismatched candidates are not assigned automatically.
Repository sync preserves existing PR session and worktree bindings; updating a
session without a worktree ID does not change the PR's execution directory.

Before applying queued changes, call the MCP tool `validate_pr_worktree` with
`pr_number`, or `POST /api/prs/{number}/validate-worktree`. Validation rechecks the
actual Git root and HEAD, including manually registered worktrees, and rejects
missing directories, nested directories, changed commits, and changed bindings.
The HTTP API returns 409 for an unavailable target. A successful result is a
point-in-time check, so consumers should execute immediately and revalidate after
switching or updating the checkout. Read-only sbnn operations validate the selected
Git root but may inspect a commit other than the checkout's current HEAD.
