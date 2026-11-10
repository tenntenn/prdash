# prdash

**prdash** is a repository-scoped command-center dashboard for viewing, coordinating, and operating pull requests created by AI coding agent sessions.

`prdash` ships as a vendor-neutral **Agent Skill** ([`skills/prdash/SKILL.md`](skills/prdash/SKILL.md)) embedded in a single Go binary, along with an MCP server and an importable Go package ([`github.com/tenntenn/prdash/prdash`](prdash)). The coding agent session that loads the skill acts as the **Dashboard Controller Session**—starting the local web dashboard, receiving instructions from the browser UI, and coordinating worker sessions bound to git worktrees.

Each dashboard instance is scoped to a target repository and links three core entities:
- **Worktree**: Git worktrees where agent sessions run isolated work.
- **Session**: Agent sessions bound to worktrees, plus the controller session running the dashboard itself.
- **Pull Request (PR)**: Pull requests created by agent sessions, including stacked PR chains.

## Screenshots

### Main Dashboard Overview (Stacked PR Chains & Turn Status)

![prdash Dashboard Overview](docs/screenshots/dashboard-overview.png)

### Expanded PR Detail, Routine Actions, `sbnn` Review & Session Thread

![prdash Expanded PR Detail](docs/screenshots/pr-detail-expanded.png)

## Install

Install the `prdash` binary with `go install`:

```console
$ go install github.com/tenntenn/prdash@latest
```

or download a prebuilt binary for your platform from the [GitHub Releases](https://github.com/tenntenn/prdash/releases) page and place it on your `PATH`.

All web assets and the agent skill are embedded in the binary via `go:embed`, so no additional runtime files are required.

## Agent Skill

`prdash` ships a vendor-neutral agent skill: a plain Markdown file with YAML front matter that describes how to launch the dashboard, coordinate worktree sessions, and operate pull requests via the CLI, HTTP API, or MCP server. Nothing in it is tied to a specific agent vendor, so any coding agent that reads skill or instruction files can use it.

The source is [`skills/prdash/SKILL.md`](skills/prdash/SKILL.md) and it is embedded in the binary, so the skill you install always matches the `prdash` binary you are running.

### Installing the skill

```console
$ prdash skill                          # print SKILL.md to stdout
$ prdash skill --list                   # list the files the skill consists of
$ prdash skill --install <dir>          # write <dir>/prdash/SKILL.md
$ prdash skill --install <dir> --force  # overwrite an older copy
```

`--install` writes the skill as a `prdash/` directory inside `<dir>` (`<dir>/prdash/SKILL.md`) and refuses to overwrite an existing file unless `--force` is given.

Where `<dir>` should point depends on your coding agent:

| Agent / Environment | Command |
| --- | --- |
| Project-scoped `.agents/skills` (Codex, Antigravity, Jules, …) | `prdash skill --install .agents/skills` |
| User-scoped `~/.agents/skills` | `prdash skill --install ~/.agents/skills` |
| Claude Code (this project / all projects) | `prdash skill --install .claude/skills` or `prdash skill --install ~/.claude/skills` |
| An agent with its own rules or skills directory (Cursor, Cline, Windsurf, …) | `prdash skill --install <dir>` (check the agent's docs for the path it expects) |
| Agents that read `AGENTS.md` without a skill directory | `prdash skill >> AGENTS.md` (or link `.agents/skills/prdash/SKILL.md` from `AGENTS.md`) |

### Updating the skill

Upgrade the `prdash` binary (`go install` or the latest release binary) and re-run `prdash skill --install` with `--force`:

```console
$ go install github.com/tenntenn/prdash@latest
$ prdash skill --install .agents/skills --force
```

Because the skill itself documents this update procedure, you can also simply ask your coding agent:

> Update the `prdash` skill to the latest version.

## Usage

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
| `-state` | *(in-memory)* | Optional JSON file path to persist and synchronize state between Web and MCP processes |
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

## Dashboard Features

- **Stacked PR Chains**: Stacked pull requests are grouped into ordered chains (`Stack 1/3`, `Stack 2/3`, `Stack 3/3`) showing base-branch dependencies (`#101 → #102 → #103`).
- **Collapsible Details (Collapsed by Default)**: Click any PR row to toggle its detail panel open or closed, or use **Expand All** / **Collapse All**.
- **Turn Ownership Badges**:
  - **Waiting for Human** (`waiting_for_human`): Highlighted in amber when the session has completed its step and awaits human review or decision.
  - **Agent Working** (`working`): Highlighted in blue while the assigned session is actively modifying the PR.
- **Filter Toolbar**: Filter pull requests by status (**All PRs**, **Waiting for Human**, **Agent Working**, **Stacked Only**) or by assigned **Worker Session / Worktree**.
- **Per-PR Controls**:
  - **Auto-Mode Toggle**: When enabled on a PR, routine tasks and review responses are processed autonomously by the session and transition back to **Waiting for Human** with a notification.
  - **Model Tier Selector (Pine / Bamboo / Plum)**: Select the three-tier model preset (`pine` = High Capability, `bamboo` = Standard Balanced, `plum` = Fast Lightweight) per PR, session, or subagent.
  - **Routine Operations**: One-click actions for **Resolve Conflicts**, **Rebase Stack**, **Fix CI**, **Sync Branch**, and **Update Summary**.
  - **Review & Turn Handoff**: **Request Review (sbnn)**, **Address Review**, inline/PR comment submission via `sbnn` ([`prdash/sbnn.go`](prdash/sbnn.go)), and manual status handoff.
- **Dashboard Session & Notifications Sidebar**: Send orchestration instructions to the controller session running the dashboard, inspect inter-session logs, and receive real-time in-app and Web Notification API alerts when PRs transition to **Waiting for Human**.

## Development

Tools are managed with [aqua](https://aquaproj.github.io/); run `aqua i` to install `go`, `task`, and `goimports`.

```console
$ task build     # build the prdash binary
$ task check     # run go fix, goimports, go vet (+ gokuigan), and go test -race
$ task ui:docs   # regenerate screenshots in docs/screenshots/
```
