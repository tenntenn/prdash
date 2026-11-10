# Search pull requests

Use **Search pull requests** above the PR list to match the PR number (with or
without `#`), title, branch, worktree ID/path/branch, or session ID/name.
Matching is case-insensitive substring matching. Whitespace separates terms;
every term must occur in at least one searchable field. For example,
`#42 feature` finds PR 42 when another field contains `feature`.

Search combines with the current status/stack and session filters. An empty
query matches all PRs allowed by those filters. If nothing matches, clear the
search or reset the other filters using All PRs and All Sessions / Worktrees.

The query stays in the current page across dashboard updates and SSE
reconnections. It is not saved to the repository or shared with other users.
