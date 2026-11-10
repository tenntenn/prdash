# Agent CLI operations

The embedded Skill (`prdash skill`) is the primary workflow guide. These commands let an agent register its execution context and operate reviews without opening the dashboard or connecting an MCP client. All operations use the same service and persistent state as the dashboard.

Select the repository explicitly with `--repo owner/name`. Use `--dir /absolute/repository/path` when local Git operations are needed. To share a nondefault state file, pass the same `--state /absolute/path/state.json` to every process. An explicit repository that differs from the state file is rejected by these commands.

## Registration

Pass one JSON object on standard input, or use `--input /absolute/path/entity.json`:

```sh
printf '%s\n' '{"id":"wt-35","path":"/tmp/project-35","branch":"feature/35"}' | prdash register worktree --repo owner/name --json
printf '%s\n' '{"id":"worker-35","name":"Issue 35 worker","worktree_id":"wt-35"}' | prdash register session --repo owner/name --json
printf '%s\n' '{"number":35,"title":"Implement feature","branch":"feature/35","base_branch":"main"}' | prdash register pr --repo owner/name --json
```

These register dashboard records; they do not create a Git worktree, launch an agent, or open a GitHub PR. Register real resources after creating them. IDs identify worktrees and sessions; the PR number identifies a PR. Repeating a registration updates that record using the service's upsert semantics: omitted fields receive service defaults, rather than acting as a JSON merge patch. Existing PR conversation history is preserved when omitted. Unknown fields, malformed JSON and multiple objects are rejected.

## Assignment

```sh
prdash assign --repo owner/name --pr 35 --worktree wt-35 --session worker-35 --json
```

Both destinations must already be registered. Assignment changes only the PR's execution worktree and destination session (and its update timestamp), preserving CI, status and conversation history. A session's own worktree remains independent. Invalid destinations fail without partially changing the PR.

## Review

```sh
prdash review request --repo owner/name --dir /tmp/project-35 --pr 35 -m 'Check error handling' --json
printf '%s\n' 'Address the review comments and run tests.' | prdash review address --repo owner/name --pr 35 --stdin --json
```

`request` invokes the service's sbnn review integration. Install `sbnn` and provide the correct local repository directory. It does not submit a GitHub approval. `address` queues an instruction for the assigned session; it does not execute the worker's edits. The worker receives it through `wait` or `instructions` and reports the actual result through `report`. `-m` and `--stdin` are mutually exclusive; empty standard input is rejected. Omitting both uses the service's default message.

Use `--help` after the entity/action for flag descriptions. `--json` writes the resulting entity to stdout. Errors go to stderr with exit status 1; successful commands and help exit 0. Existing `wait` uses exit status 2 for timeout. Consumers should check the exit status before parsing JSON.
