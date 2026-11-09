---
name: prdash
description: Launch and operate the prdash command-center dashboard to coordinate pull requests, git worktrees, and AI agent sessions for a target repository. Works across any MCP- or CLI-capable coding agent.
---

# prdash Skill

`prdash` is a repository-scoped command-center dashboard that links **Git Worktrees**, **Agent Sessions**, and **Pull Requests** (including stacked PR chains).

This skill is agent-agnostic and works identically across any coding agent that can run shell commands or connect to Model Context Protocol (MCP) servers.

## Controller Session Role

When an agent session loads this skill and starts `prdash`, that agent session serves as the **Dashboard Controller Session** (`sess-controller`):

1. **Human → Controller**: The human operator can type instructions directly into the **Dashboard Session** input panel in the web UI (or via MCP/API).
2. **Controller → Worker Sessions**: The controller session coordinates worker sessions bound to individual git worktrees, forwarding PR-level instructions, routine actions, and `sbnn` review tasks.
3. **Worker Sessions → Controller & Human**: Worker sessions report progress and completion back to the dashboard. Whenever a PR transitions from `working` (**Agent Working**) to `waiting_for_human` (**Waiting for Human**), `prdash` emits a notification to alert the human operator.

## Starting the Dashboard

Run `prdash` from the repository root:

```bash
go run github.com/tenntenn/prdash -repo owner/name
```

- `prdash` automatically binds an available local TCP port and prints the URL on stdout:
  ```text
  prdash listening on http://127.0.0.1:<port> (repository: owner/name)
  ```
- Open the printed URL in a browser to view and operate the dashboard.

## Operating via MCP Server

`prdash` also runs as a standard JSON-RPC 2.0 MCP server over stdio, sharing the exact same core `prdash.Service` implementation:

```bash
go run github.com/tenntenn/prdash -mcp -repo owner/name
```

### Available MCP Tools

| Tool | Purpose |
| :--- | :--- |
| `get_dashboard` | Retrieve the full repository snapshot (PRs, stacks, sessions, worktrees, model presets, notifications). |
| `list_prs` | List pull requests filtered by `status` (`waiting_for_human` or `working`) or `stack_id`. |
| `get_pr` | Inspect a single PR and its full session conversation thread. |
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
