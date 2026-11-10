---
name: prdash
description: Launch, operate, install, and update the prdash command-center dashboard to coordinate pull requests, git worktrees, and AI agent sessions for a target repository. Works across any coding agent that reads skill files or uses MCP.
license: MIT
---

# Coordinating pull requests and agent sessions with prdash

`prdash` is a repository-scoped command-center web dashboard, CLI, and MCP server that links **Git Worktrees**, **Agent Sessions**, and **Pull Requests** (including stacked PR chains).

Nothing in this skill is specific to a single agent vendor: any coding agent that can read a Markdown instruction file, run shell commands, or connect to a Model Context Protocol (MCP) server can use it.

## Prerequisites

`prdash` must be on `PATH`. Check with `prdash -version`; if it is missing, install the binary with `go install`:

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

## Workflow

State for each repository is automatically persisted in `$XDG_STATE_HOME/prdash/state-<owner>__<repo>.json` (or `~/.local/state/prdash/...`), so sequential invocations can reuse state. For parallel execution, use one server as the state owner and connect workers through the instruction HTTP API. Independent processes writing the same file are not transactionally coordinated (#37).

### 1. Start the dashboard and synchronize the repository

Run `prdash` in the repository directory:

```console
prdash
```

- `prdash` automatically detects the repository (`owner/name`, default branch, local root), discovers local git worktrees (`git worktree list --porcelain`), fetches open GitHub PRs (`gh pr list`), infers stacked PR chains (`Stack 1/N → 2/N → ...`), and prints the listening URL:
  ```text
  prdash listening on http://127.0.0.1:<port> (repository: owner/name)
  ```
- Hand the printed URL to the human operator so they can open and operate the dashboard in their browser.
- To refresh worktrees and open GitHub PRs at any time from the command line:
  ```console
  prdash sync
  prdash status
  ```

### 2. Wait for human instructions (Controller or Worker Session)

Never poll in a tight shell loop. Use `prdash wait` to block until the human sends an instruction or triggers a routine operation in the browser UI:

```console
prdash wait --session controller --timeout 30m
prdash wait --session <worker-session-id> --timeout 30m
```

- If unhandled instructions are already queued, `prdash wait` prints them and exits `0` immediately.
- If `--timeout` expires before an instruction arrives, `prdash wait` exits with status `2`.
- To inspect or acknowledge pending instructions without blocking:
  ```console
  prdash instructions --session <session-id>
  prdash instructions --session <session-id> --ack
  ```

### 3. Act on instructions and report results back

Before executing an instruction, claim its exact ID through the single running
server. Use a stable consumer ID unique to this worker:

```sh
prdash instruction claim -server http://127.0.0.1:8080 -session <assigned-session> -id <instruction-id> -consumer <consumer-id>
prdash instruction result -server http://127.0.0.1:8080 -session <assigned-session> -id <instruction-id> -consumer <consumer-id> -claim-token <returned-token> -attempt <returned-attempt> -target-commit <returned-target-commit> -outcome completed -status waiting_for_human -m "Verification completed"
```

Save the claim before executing. If the target commit is unknown, supply the
concrete input commit with `-target-commit` when claiming. Delivery acknowledgment
(`instruction ack`) is separate from execution completion. `get` returns the
lifecycle state; after a restart, the same consumer can recover its running claim.
Never automatically expire or steal a running claim. Inspect external effects
before resuming and use instruction ID plus attempt as an idempotency key.

Use `-outcome failed` for failure. Explicit `instruction retry` requires that
failed claim's consumer, token and attempt; the next claim gets a fresh attempt.
Identical result resends are safe, but execution is not exactly-once. A stale
result is retained without applying PR status to a newer instruction/head/session.
Legacy `report` is an uncorrelated status report and does not acknowledge queued
instructions. Do not use bulk ack or independent state-file writers for parallel
execution. See the README's instruction lifecycle for the complete contract.

### Available MCP Tools

| Tool | Purpose |
| :--- | :--- |
| `get_dashboard` | Retrieve the full repository snapshot (PRs, stacks, sessions, worktrees, model presets, notifications). |
| `sync_repository` | Discover local git worktrees (`git worktree list`) and open GitHub pull requests (`gh pr list`) and synchronize them into the dashboard. |
| `list_pending_instructions` | List unhandled instructions queued for a worker session or the controller session (`session_id`). |
| `ack_instructions` | Deprecated bulk delivery acknowledgment; use `ack_instruction` for parallel consumers. |
| `get_instruction` / `ack_instruction` | Inspect or acknowledge delivery of one instruction ID and assigned session. |
| `claim_instruction` | Claim one ID with a stable consumer ID; recover the same running claim after restart. |
| `report_instruction_result` | Correlate result with instruction ID, session, consumer, token, attempt and target commit. |
| `retry_instruction` | Explicitly requeue a failed attempt without losing result history. |
| `list_prs` | List pull requests filtered by `status` (`waiting_for_human` or `working`) or `stack_id`. |
| `get_pr` | Inspect a single PR and its full session conversation thread. |
| `upsert_worktree` | Register or update a git worktree bound to an agent session. |
| `upsert_session` | Register or update a worker session bound to a git worktree. |
| `upsert_pr` | Register or update a pull request managed by an agent session. |
| `send_pr_message` | Send an instruction to the session responsible for a specific PR. |
| `post_sbnn_comment` | Post a code review comment through the `sbnn` CLI integration. |
| `request_review` | Send the PR diff to `sbnn` and read open review comments (`sbnn comments --format json`). |
| `address_review` | Instruct the assigned session to address open review feedback on a PR. |
| `run_routine_action` | Trigger a preset operation: `resolve_conflicts`, `rebase_stack`, `fix_ci`, `sync_branch`, or `update_summary`. |
| `set_auto_mode` | Toggle per-PR Auto-Mode (`true` = autonomous handling, `false` = manual mode). |
| `set_pr_status` | Transition a PR between `waiting_for_human` and `working`. |
| `send_session_instruction` | Send a direct instruction to a worker session outside any specific PR thread. |
| `send_controller_instruction` | Send an orchestration instruction to the dashboard controller session. |
| `report_session_result` | Legacy uncorrelated status report; does not acknowledge instructions. |
| `set_model_tier` | Set the 3-tier model preset (`pine`, `bamboo`, `plum`) for a session, subagent, or PR. |
| `configure_model_presets` | Map `pine`, `bamboo`, and `plum` presets to concrete model identifiers. |
| `mark_notification_read` | Mark a notification ID or `"all"` notifications as read. |

## Model Tier Presets (Pine / Bamboo / Plum)

Model assignment uses three abstract presets so workflows remain portable across model providers:

- **Pine (`pine`)**: High-capability, deep-reasoning model for complex architecture or security-critical stacks.
- **Bamboo (`bamboo`)**: Standard balanced model for day-to-day feature implementation and review fixes.
- **Plum (`plum`)**: Fast, low-cost model for routine tasks, formatting, and lightweight subagents.
