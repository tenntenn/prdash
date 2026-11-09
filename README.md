# prdash

**prdash** is a local command-center dashboard in Go for viewing, coordinating, and operating pull requests created by AI coding agent sessions.

Each dashboard instance is scoped to a target repository and links three core entities:
- **Worktree**: Git worktrees where agent sessions run isolated work.
- **Session**: Agent sessions bound to worktrees, plus the controller session running the dashboard itself.
- **Pull Request (PR)**: Pull requests created by agent sessions, including stacked PR chains.

---

## Quick Start

Start the dashboard from the repository root:

```bash
go run .
```

`prdash` automatically binds an available local TCP port and prints the dashboard URL on startup:

```text
prdash listening on http://127.0.0.1:<port> (repository: acme/nebula-api)
```

Open the printed URL in your browser. On startup, `prdash` seeds realistic sample data for a target repository (`acme/nebula-api` by default, configurable via `-repo owner/name`), including:
- **1 Dashboard Controller Session** (`sess-controller`) and **4 Worker Sessions** (`sess-alpha` through `sess-delta`)
- **4 Git Worktrees** (`wt-alpha` through `wt-delta`)
- **9 Pull Requests**, including a **3-tier stacked PR chain** (`#101 → #102 → #103`), a **2-tier stacked PR chain** (`#104 → #105`), and **4 standalone PRs** (`#106` through `#109`) across mixed states (**Waiting for Human** and **Agent Working**).

---

## Three Delivery Forms (Unified Core Logic)

All three forms share the single core implementation in [`github.com/tenntenn/prdash/prdash`](prdash):

### 1. Agent Skill (`skills/prdash/SKILL.md`)
An agent-agnostic skill definition compatible with any coding agent that supports `SKILL.md` instructions, shell execution, or MCP tools. The agent session that loads [`skills/prdash/SKILL.md`](skills/prdash/SKILL.md) acts as the dashboard controller session and can receive instructions directly from the web UI.

### 2. MCP Server (`-mcp`)
Run `prdash` as a JSON-RPC 2.0 Model Context Protocol server over stdio:

```bash
go run . -mcp
```

Exposes tools for querying the dashboard (`get_dashboard`, `list_prs`, `get_pr`), sending PR/session/controller instructions (`send_pr_message`, `send_session_instruction`, `send_controller_instruction`, `report_session_result`), running reviews and routine operations (`post_sbnn_comment`, `request_review`, `address_review`, `run_routine_action`), toggling Auto-Mode (`set_auto_mode`), and configuring model tiers (`set_model_tier`, `configure_model_presets`).

### 3. Go Library (`github.com/tenntenn/prdash/prdash`)
Import `github.com/tenntenn/prdash/prdash` directly in Go programs to embed the state service, HTTP handler, or MCP server:

```go
package main

import (
	"net/http"

	"github.com/tenntenn/prdash/prdash"
)

func main() {
	svc := prdash.NewService(prdash.Options{
		Repository: "owner/repo",
	})
	http.ListenAndServe("127.0.0.1:0", prdash.NewHandler(svc))
}
```

---

## Dashboard Screen Guide

### 1. Main Screen: Pull Request List
- **Stacked PR Chains**: Stacked pull requests are visually grouped into ordered chains (`Stack 1/3`, `Stack 2/3`, `Stack 3/3`) showing base-branch dependencies (`#101 → #102 → #103`).
- **Collapsible Details (Collapsed by Default)**: Click any PR row to toggle its full detail panel open or closed, or use **Expand All** / **Collapse All**.
- **Clear Turn Ownership Badges**:
  - **Waiting for Human** (`waiting_for_human`): Highlighted in amber — indicates the agent session has completed its step and awaits human review or decision.
  - **Agent Working** (`working`): Highlighted in blue — indicates the assigned agent session is actively modifying the PR.
- **Per-PR Controls**:
  - **Auto-Mode Toggle**: When enabled on a PR, routine tasks and review responses are processed autonomously by the session and transition back to **Waiting for Human** with a notification. When disabled, the PR waits for explicit human instructions.
  - **Model Tier Selector (Pine / Bamboo / Plum)**: Select the three-tier model preset (`pine` = High Capability, `bamboo` = Standard Balanced, `plum` = Fast Lightweight) per PR or session. Concrete model names are configurable via the **Model Presets (Pine / Bamboo / Plum)** dialog in the header.
  - **Routine Operation Buttons**: One-click actions for **Resolve Conflicts**, **Rebase Stack**, **Fix CI**, **Sync Branch**, and **Update Summary**.
  - **Review & Turn Handoff**: **Request Review (sbnn)**, **Address Review**, and manual status handoff between **Waiting for Human** and **Agent Working**.
  - **Send PR Comment via `sbnn`**: Submits inline or PR-level review comments through the isolated `SBNNReviewer` interface ([`prdash/sbnn.go`](prdash/sbnn.go)).

### 2. Per-PR Session Interaction Thread
- Inside each expanded PR card, a dedicated message thread displays the conversation history between the human operator, the dashboard controller session, the `sbnn` review tool, and the worker session assigned to that PR.
- Submitting a message routes the instruction to the assigned worker session and updates both the PR thread and session state.

### 3. Dashboard Controller Session & Worker Sessions Sidebar
- **Dashboard Session Panel**: Displays the controller session running the dashboard itself (`Dashboard Orchestrator`), its active model tier, inter-session event feed, and an input box to send instructions directly to the dashboard session.
- **Notifications Panel**: Real-time alerts emitted whenever a PR transitions to **Waiting for Human**, accompanied by toast notifications in the UI.
- **Worker Sessions & Worktrees Panel**: Lists each worker session alongside its bound git worktree, model presets for the session and its subagents, a direct session instruction form, and an expandable inter-session communication log.
