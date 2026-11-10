# Repository synchronization

`prdash sync`, the dashboard's **Sync GitHub & Worktrees** button and the `sync_repository` MCP tool share one service operation.

The open PR query requests up to 101 records and retains the first 100. If the extra record exists, synchronization reports truncation explicitly; it never treats missing records as proof of closure. Monitored PRs absent from that open list are queried individually, so their `github_state` (`open`, `merged`, `closed`, or `unknown`) and `is_draft` remain current. Closed and merged records remain available with their conversation history.

`ci_status: unknown` means no checks were supplied or a check's outcome is unavailable. `mergeability` distinguishes `unknown`, `mergeable` and `conflicting`; the older `has_conflicts` flag is true only for confirmed conflicts. Unknown must not be treated as evidence that CI passed or that a PR is conflict-free.

A failure in either local Git or GitHub discovery causes a non-success response, while successfully fetched worktrees and PRs are still merged. Failed PR lookups preserve the last known record. The dashboard shows the error and refreshes the available data. Snapshot JSON includes `sync.git_error`, `sync.github_error`, `sync.truncated` and attempt/completion timestamps. These diagnostics describe the current service instance rather than persisted cross-process history.

Each long-running service permits one active sync and caches its result (including errors) for 30 seconds after completion. Repeated UI/API requests during that interval use the last result without additional GitHub calls; `sync.next_allowed_at` identifies the next permitted attempt. A concurrent request reports that synchronization is in progress. Separate CLI processes have separate throttles, so external schedulers should avoid tight retry loops.
