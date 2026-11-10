# Review response workflow foundation

This draft implements policy preview and a value-based workflow state machine.
It does not yet connect the workflow to `AddressReview`, Auto-Mode, a step
executor, or persisted PR workflow history. Existing response dispatch must not
be described as enforcing these new checkpoints until that integration lands.

Repository configuration adds an ordered `review_response.steps` list and
`review_response.rereview.gate` (`auto`, `confirm`, or `off`). Defaults are:

```yaml
review_response:
  steps: [fix, test, commit, push, reply, resolve, rereview]
  rereview:
    gate: confirm
    max_rounds: 1
    stop_on: approved
```

Unknown and duplicate actions are rejected. Enabled actions must occur once.
Dependencies require fixes before tests, tests before commits, commits before
pushes, replies before resolution, and tests before re-review. Explicit `off`
actions are recorded as skipped; they cannot be requested by a one-off scope.

`PlanReviewResponse` uses explicit finding types (`ai`, `human`, `question`)
and the repository's corresponding handling policies. It does not infer authors
or questions from keywords or punctuation. Automatic code changes are scoped to
findings whose policy is `auto`. `prepare_reply` produces a draft-reply step;
`human_decide` creates a human checkpoint. The service preview binds findings
to existing PR thread entries and refuses to promote a human or unknown source
to AI provenance.

One-off text is context only. Nonempty text adds an `instruction_review`
checkpoint instead of guessing its meaning with a keyword permission filter.
Structured `requested_actions` that are disabled or unknown appear in the
preview's conflicts and prevent constructing a workflow.

Preview without executing anything:

- HTTP: `POST /api/prs/42/response-preview` with a `ResponseRequest` JSON body.
- MCP: `preview_response_workflow` with `pr_number` and optional `request`.
- Go: `Service.PreviewResponseWorkflow` or the pure `PlanReviewResponse` function.

`NewResponseWorkflow` rebuilds the supplied plan from its captured repository
policy. `BeginResponseStep` starts at most one step and stops at `confirm`
without explicit step approval. Automatic callers must never supply approval.
`CompleteResponseStep` records completed, failed, disputed, or interrupted
results. Failures stop further steps while preserving earlier successes.
Re-review completion requires an actual confirmed verdict. Matching the stop
criterion completes the run; otherwise rounds are bounded and the limit creates
a human checkpoint. All operations return independent copies and retain the
captured configuration and instruction contents.

Remaining integration work: durable workflow history and per-step authorization,
actual bounded step dispatch, responsible-agent selection, Web/CLI workflow
controls, `AddressReview` and Auto-Mode enforcement, and end-to-end recovery tests.
