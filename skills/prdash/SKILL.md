---
name: prdash
description: Launch, operate, install, and update the prdash command-center dashboard to coordinate pull requests, git worktrees, and AI agent sessions for a target repository. Works across any coding agent that reads skill files or uses MCP.
license: MIT
---

# Coordinating pull requests and agent sessions with prdash

`prdash` is a repository-scoped command-center web dashboard and MCP server that links **Git Worktrees**, **Agent Sessions**, and **Pull Requests** (including stacked PR chains).

Nothing in this skill is specific to a single agent vendor: any coding agent that can read a Markdown instruction file, run shell commands, or connect to a Model Context Protocol (MCP) server can use it.

## Prerequisites

`prdash` must be on `PATH`. Check with `prdash --version`; if it is missing, install the binary with `go install`:

```console
go install github.com/tenntenn/prdash@latest
```

or download a prebuilt binary for the user's OS and architecture from `https://github.com/tenntenn/prdash/releases` and place it on `PATH`.

## Installing and updating this skill

The `SKILL.md` file is embedded directly inside the `prdash` binary so that the installed skill always matches the `prdash` command on `PATH`.

When the user asks to **install** or **update** the `prdash` skill:

1. Install or upgrade the `prdash` binary:
   ```console
   go install github.com/tenntenn/prdash@latest
   ```
   (or replace the `prdash` binary on `PATH` with the latest release binary from `https://github.com/tenntenn/prdash/releases`).
2. Write the updated skill into the directory where your agent loads skills (pass `--force` when updating an existing copy):
   ```console
   prdash skill --install <skills-dir> --force
   ```
   Common target directories for `<skills-dir>`:
   - `.agents/skills` (project-scoped vendor-neutral skills directory)
   - `~/.agents/skills` (user-scoped vendor-neutral skills directory)
   - `.claude/skills` or `~/.claude/skills` (Claude Code)
   - Any agent-specific rules/skills directory configured in the workspace

To inspect the embedded skill without writing files:

```console
prdash skill          # print SKILL.md to stdout
prdash skill --list   # list embedded skill files
```

---

## Controller Session Role

When an agent session loads this skill and starts `prdash`, that agent session acts as the **Dashboard Controller Session** (`sess-controller`):

1. **Human → Controller**: The human operator can send orchestration instructions from the **Dashboard Session** panel in the browser UI.
2. **Controller → Worker Sessions**: The controller session coordinates worker sessions bound to individual git worktrees, dispatching PR-level instructions, routine actions, and `sbnn` review tasks.
3. **Worker Sessions → Controller & Human**: Worker sessions report progress and completion back to the dashboard. Whenever a PR transitions from `working` (**Agent Working**) to `waiting_for_human` (**Waiting for Human**), `prdash` emits a notification to alert the human operator.

## Starting the Dashboard

Start `prdash` for the target repository, passing `-state` so the Web UI and MCP / CLI invocations share state:

```console
prdash -repo owner/name -state /tmp/prdash-state.json
```

- `prdash` automatically binds an available local TCP port and prints the URL on stdout:
  ```text
  prdash listening on http://127.0.0.1:<port> (repository: owner/name)
  ```
- Hand the printed URL to the human operator so they can open and operate the dashboard in their browser.

## Operating via MCP Server

`prdash` also runs as a JSON-RPC 2.0 Model Context Protocol (MCP) server over stdio, sharing the same `prdash.Service` core:

```console
prdash -mcp -repo owner/name -state /tmp/prdash-state.json
```

### Available MCP Tools

| Tool | Purpose |
| :--- | :--- |
| `get_dashboard` | Retrieve the full repository snapshot (PRs, stacks, sessions, worktrees, model presets, notifications). |
| `sync_repository` | Discover local git worktrees (`git worktree list`) and open GitHub pull requests (`gh pr list`) and synchronize them into the dashboard. |
| `list_pending_instructions` | List queued instructions for a worker session or the controller session (`session_id`). |
| `list_prs` | List pull requests filtered by `status` (`waiting_for_human` or `working`) or `stack_id`. |
| `get_pr` | Inspect a single PR and its full session conversation thread. |
| `upsert_worktree` | Register or update a git worktree bound to an agent session. |
| `upsert_session` | Register or update a worker session bound to a git worktree. |
| `upsert_pr` | Register or update a pull request managed by an agent session. |
| `send_pr_message` | Send an instruction to the session responsible for a specific PR. |
| `post_sbnn_comment` | Post a code review comment through the `sbnn` integration boundary. |
| `request_review` | Run an automated review on a PR via `sbnn`. |
| `address_review` | Instruct the assigned session to address open review feedback on a PR. |
| `run_routine_action` | Trigger a preset operation: `resolve_conflicts`, `rebase_stack`, `fix_ci`, `sync_branch`, or `update_summary`. |
| `set_auto_mode` | Toggle per-PR Auto-Mode (`true` = autonomous handling, `false` = manual mode). |
| `set_pr_status` | Transition a PR between `waiting_for_human` and `working`. |
| `send_session_instruction` | Send a direct instruction to a worker session outside any specific PR thread. |
| `send_controller_instruction` | Send an orchestration instruction to the dashboard controller session. |
| `report_session_result` | Report status and results from a worker session back to the dashboard. |
| `set_model_tier` | Set the 3-tier model preset (`pine`, `bamboo`, `plum`) for a session, subagent, or PR. |
| `configure_model_presets` | Map `pine`, `bamboo`, and `plum` presets to concrete model identifiers. |
| `mark_notification_read` | Mark a notification ID or `"all"` notifications as read. |

## Operating via HTTP REST API

When the web dashboard is running at `http://127.0.0.1:<port>`, any agent can inspect and control it over HTTP:

- `GET /api/state` — Fetch full `Snapshot` JSON (including `controller_session.messages` to read instructions entered by the human in the UI).
- `POST /api/worktrees` — Register or update a git worktree.
- `POST /api/sessions` — Register or update a worker session.
- `POST /api/prs` — Register or update a pull request.
- `POST /api/prs/{number}/messages` — `{"author": "Controller Agent", "text": "..."}`
- `POST /api/prs/{number}/sbnn-comment` — `{"file_path": "pkg/foo.go", "line": 42, "body": "..."}`
- `POST /api/prs/{number}/request-review` — `{"focus": "concurrency"}`
- `POST /api/prs/{number}/address-review` — `{"instruction": "..."}`
- `POST /api/prs/{number}/routine` — `{"action": "resolve_conflicts"}`
- `POST /api/prs/{number}/auto-mode` — `{"enabled": true}`
- `POST /api/prs/{number}/status` — `{"status": "waiting_for_human", "summary": "..."}`
- `POST /api/prs/{number}/model-tier` — `{"tier": "pine"}`
- `POST /api/sessions/{id}/instruction` — `{"text": "..."}`
- `POST /api/sessions/{id}/report` — `{"pr_number": 102, "status": "waiting_for_human", "report": "..."}`
- `POST /api/controller/instruction` — `{"text": "..."}`
- `POST /api/models` — `{"pine": "...", "bamboo": "...", "plum": "..."}`

## Model Tier Presets (Pine / Bamboo / Plum)

Model assignment uses three abstract presets so workflows remain portable across model providers:

- **Pine (`pine`)**: High-capability, deep-reasoning model for complex architecture or security-critical stacks.
- **Bamboo (`bamboo`)**: Standard balanced model for day-to-day feature implementation and review fixes.
- **Plum (`plum`)**: Fast, low-cost model for routine tasks, formatting, and lightweight subagents.
