# Local HTTP access

The dashboard accepts loopback listeners only (`127.0.0.1`, `::1`, or
`localhost`). Wildcard and non-loopback addresses are rejected at startup.
The handler also rejects non-loopback peers, foreign Host values, and foreign
Origins; forwarded headers do not expand this policy.

State-changing HTTP requests require `Content-Type: application/json` and a
server-lifetime operation token. A local agent API client obtains the token from
`GET /api/auth` with `X-Prdash-Client: dashboard`, then sends it in the
`Authorization: Bearer ...` header:

```sh
PRDASH_URL=http://127.0.0.1:1234 # use the URL printed by the server
PRDASH_TOKEN=$(curl --fail --silent --show-error \
  -H 'X-Prdash-Client: dashboard' "$PRDASH_URL/api/auth" | jq -r .token)
curl --fail --silent --show-error \
  -H "Authorization: Bearer $PRDASH_TOKEN" \
  -H 'Content-Type: application/json' \
  --data '{"text":"Summarize pending work"}' \
  "$PRDASH_URL/api/controller/instruction"
```

Tokens are not stored in the repository or persisted across server restarts.
An HTTP 401 response with `code: "authentication_required"` is emitted before
an operation is dispatched. Clients may reacquire a token and retry that
specific rejection once. Do not automatically replay network errors or other
failures: the operation might already have executed. The dashboard refreshes
credentials without reloading the page, preserving drafts and editing state.

The token endpoint has no CORS allowance and requires a custom request header.
This protects against cross-origin browser requests, including DNS-rebinding
Host values; local processes that can access the listener can acquire a token.
It is not a separate authentication boundary between local OS accounts.
CLI commands that operate directly on the state file and stdio MCP do not use
HTTP credentials.

The `prdash instruction` CLI connects to the running server with `-server`:

```sh
prdash instruction ack -server "$PRDASH_URL" -session controller -id "$INSTRUCTION_ID"
```

It acquires an operation token automatically after the server's explicit
pre-operation authentication rejection. An optional `PRDASH_API_TOKEN` supplies
an existing token; missing or expired credentials are recovered using the same
one-retry policy. This operation token is separate from the instruction's
claim token (`PRDASH_CLAIM_TOKEN`), which authorizes reporting a claimed attempt.
See the [instruction lifecycle](integrations.md#claim-and-report-an-individual-instruction) for the
claim/result workflow.
