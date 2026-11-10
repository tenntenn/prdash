# Reviewer selection and execution

Repository `prdash.yaml` selects review ownership independently from the PR's
implementation owner. See [configuration](configuration.md) for defaults and
schema validation.

```yaml
review:
  agent: sample-provider
  session: dedicated
  model: sample-model
  instructions_file: .prdash/review.md
```

A request resolves the configured role against explicitly connected runtime
bindings. Persisted IDs, provider labels, and discovered worktrees do not prove
that an agent is connected. No synthetic reviewer sessions are created and an
unavailable provider is never replaced with another provider.

| Role | Resolution |
| --- | --- |
| `working` | The implementation owner's connected runtime |
| `separate` | Exactly one matching connected runtime other than the owner or controller |
| `dedicated` | Exactly one matching binding marked `Dedicated`, other than the owner |
| `controller` | The controller's matching connected runtime |

Zero matches, multiple matches, disconnected sessions, unsupported models, or
an unresolved target commit produce an error before dispatch. `model_tier`
resolves through the service's preset map and is sent as a requested model;
`model` sends the explicit name. Neither changes another session's saved model
settings or implicitly calls its model-control integration.

## Inspect before dispatch

- Web: **Preview & Request Review** shows the reviewer, requested model, captured
  instructions, target commit, and configuration path before dispatch.
- CLI: `prdash review-preview -dir /absolute/repository -state /absolute/state.json -pr 42`.
- HTTP: `GET /api/prs/42/review-preview`.
- MCP: `preview_review` with `pr_number` and optional `focus`.

The result includes a `preview_key`. Pass it to HTTP `request-review` or MCP
`request_review` to reject a changed policy, reviewer, target, or focus between
inspection and dispatch. Web does this automatically. Preview never executes
reviews or applies models. A second simultaneous review for the same PR is
rejected while the first is running.

## Runtime integration

Embedding applications explicitly provide process-local `Options.ReviewRuntimes`,
keyed by a registered session ID. Each `ReviewRuntimeBinding` declares its
provider, `SupportedModels`, optional `Dedicated` designation, and a
`ReviewRuntime` implementation. The session ID stays local; only portable roles
and provider names belong in the shared YAML file.

`RunReview(context.Context, SBNNReviewRequest)` receives the fixed target commit,
request ID, requested model, captured Markdown instructions, focus, and full
configuration snapshot in `Execution`. It must honor these values or return an
error. Its result reports `AppliedModel` only after the model was actually used.
A requested model that is not confirmed by the result makes the request fail;
the audit preserves the reported model to explain a mismatch. The existing
`SBNNReviewRequest` and result names are retained for API compatibility.

The standalone CLI does not invent connections to arbitrary agent providers.
Additional providers require an embedding integration with an explicit runtime
binding. Restarting from persisted state alone leaves those sessions unconnected.

## Built-in sbnn behavior

The default `agent: sbnn`, `session: separate` uses the configured local sbnn
executable to synchronize review comments. It is a local tool, not a live AI
reviewer. A missing executable is reported during preview. Model and instruction
file overrides are rejected because this adapter cannot honor them.

The adapter sends a diff pinned to the selected commit instead of reading a
moving branch. The sbnn integration isolates its target by repository and pull request.
Failure to read or send that diff is an error; it does not silently fall back to
an unrelated current GitHub diff. Review execution cannot replace the PR's
implementation owner, worktree, or existing comments.

## Audit history

A `review_request` thread record is persisted before execution. Its
`review_execution` contains the request ID, full target commit and source,
resolved reviewer, requested model, captured configuration and instructions.
Completion records the actual reviewer and reported applied model. Errors
remain visible with `status: failed`; they do not add a successful review comment.
Returned snapshots are deep copies, and the audit survives configuration reloads
and application restarts. A runtime acceptance or a saved preset is never
presented as an observed applied model.
