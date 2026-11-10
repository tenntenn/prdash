# CLI and integration reference

Use the [prdash Skill](../skills/prdash/SKILL.md) for the normal agent workflow. This reference is for manual operation and integration authors.


### Starting the Web Dashboard & Using the CLI

```console
$ prdash                               # auto-detects git repo, worktrees, and open GitHub PRs
$ prdash status                        # show repository summary (worktrees, sessions, PRs)
$ prdash sync                          # refresh git worktrees and open GitHub PRs
$ prdash wait --session controller     # block until a human instruction arrives
$ prdash instructions --session sess-1 # list unhandled instructions for a session
$ prdash report --pr 42 -m "Fixed CI"  # report completion and transition PR to waiting_for_human
$ prdash send --pr 42 -m "Run tests"   # queue an instruction for a PR session
```

Repository state is persisted automatically in `$XDG_STATE_HOME/prdash/state-<owner>__<name>.json` (or `~/.local/state/prdash/...`), so the Web UI, CLI subcommands, and MCP server (`prdash -mcp`) share state out of the box without needing manual `-state` flags.

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

Passing the same `-state` file path to both the web server and the MCP server keeps browser actions and MCP tool calls synchronized in real time.

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

