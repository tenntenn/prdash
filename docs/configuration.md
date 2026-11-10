# Repository configuration

Place `prdash.yaml` in the target repository root. Startup from a subdirectory
finds its nearest `.git` directory or worktree marker; a nested configuration
file does not override the root. Each service keeps its selected repository
root, even when it manages PR worktrees. An absent file uses all defaults.

```yaml
version: 1
review:
  agent: sbnn
  session: separate
  instructions_file: .prdash/review.md
review_response:
  instructions_file: .prdash/respond.md
  verify:
    - go test -race ./...
  push: confirm
  reply: confirm
  resolve: confirm
  rereview:
    max_rounds: 1
```

Instruction files are relative to the configuration directory. They must exist
within that repository, including after symlink resolution. Their contents are
captured with the configuration. Files are limited to 1 MiB each.

## Schema and defaults

| Field | Default | Accepted values |
| --- | --- | --- |
| `version` | `1` | `1` |
| `review.agent` | `sbnn` | Nonempty agent name |
| `review.session` | `separate` | `working`, `separate`, `controller`, `dedicated` |
| `review.model` | empty | Optional model name |
| `review.model_tier` | empty | `pine`, `bamboo`, `plum`; mutually exclusive with `model` |
| `review.instructions_file` | empty | Repository-relative Markdown file |
| `review_response.agent` | empty | Optional agent name |
| `review_response.session` | `working` | Same roles as `review.session` |
| `review_response.instructions_file` | empty | Repository-relative Markdown file |
| `review_response.verify` | `[]` | List of nonblank command strings |
| `review_response.finding_policies.ai` | `auto` | `auto`, `prepare_reply`, `human_decide` |
| `review_response.finding_policies.human` | `prepare_reply` | Same finding policies |
| `review_response.finding_policies.question` | `human_decide` | Same finding policies |
| `review_response.fix`, `review_response.test` | `auto` | `auto`, `confirm`, `off` |
| `review_response.commit`, `push`, `reply`, `resolve` | `confirm` | `auto`, `confirm`, `off` |
| `review_response.rereview.agent`, `.session` | empty | Optional agent and session role |
| `review_response.rereview.max_rounds` | `1` | Integer from 0 to 20 |
| `review_response.rereview.stop_on` | `approved` | `approved`, `no_open_comments` |

YAML mappings, lists, quoted strings, comments, and block strings are supported.
Unknown or duplicate keys, aliases/anchors, merge keys, multiple documents,
wrong types, unsupported versions, and invalid enum values are errors.
`test: off` with verification commands and `commit: off` with `push: auto`
are rejected as contradictory. Errors include the source path, line, field
when known, and reason. YAML syntax errors use the parser's line information;
columns are available for parsed schema and policy errors.

## Inspect and reload

- CLI: `prdash config -dir /absolute/path/to/repository` prints effective JSON
  using the same loader as service startup. Each invocation reads current files.
- HTTP: `GET /api/config` returns the effective configuration and last error;
  `/api/state` also includes `config` and `config_error`.
- MCP: `get_config` exposes the same effective settings.
- A running service reloads only on `POST /api/config/reload` or the MCP
  `reload_config` tool. There is no automatic file watcher.

Effective JSON includes `source` (`file` or `defaults`), `source_path`,
`repo_root`, `defaulted_fields`, captured `instructions`, `loaded_at`, and a
content-derived `revision`. Explicitly specifying a default value removes that
field from `defaulted_fields`. The revision includes source and instruction
contents, but not the load timestamp.

Invalid startup configuration stops CLI dispatch/startup. A failed service
reload retains the last good configuration for inspection, reports the error,
and blocks new dispatch until a valid reload succeeds. Existing requests keep
running with their original settings. Disabling Auto-Mode remains available.

Review and comment requests, queued session instructions, and corresponding
thread records capture `applied_config`. Persisted state retains that snapshot
across restarts. A reload cannot change an in-flight request's captured policy
or instruction contents. Snapshots returned to clients are independent copies.

Loading, inspecting, and reloading configuration never executes `verify`
commands or external actions. This configuration layer validates and carries
policy to request consumers; agent selection and response workflow execution
are separate integrations. A configuration value alone does not launch an
agent or authorize an external action.
