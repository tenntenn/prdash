import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { test } from "node:test";
import vm from "node:vm";

const context = vm.createContext({ window: {} });
vm.runInContext(await readFile(new URL("../prdash/web/search.js", import.meta.url), "utf8"), context);
const matches = context.window.prdashSearch.matches;
const pr = { number: 42, title: "Fix local HTTP access", branch: "fix/auth", worktree_id: "wt-api", session_id: "worker-3" };
const session = { name: "API worker" };
const worktree = { path: "/tmp/sample-checkout", branch: "fix/auth" };
const cases = [
  { query: "", want: true },
  { query: "  \t ", want: true },
  { query: "42", want: true },
  { query: "#42", want: true },
  { query: "LOCAL http", want: true },
  { query: "fix/auth", want: true },
  { query: "wt-api", want: true },
  { query: "sample-checkout", want: true },
  { query: "worker-3", want: true },
  { query: "API WORKER", want: true },
  { query: "#42  auth\tworker", want: true },
  { query: "#42 missing", want: false },
  { query: "#99", want: false },
];
for (const { query, want } of cases) {
  test(`search ${JSON.stringify(query)}`, () => assert.equal(matches(pr, session, worktree, query), want));
}
test("missing related records still match PR fields", () => {
  assert.equal(matches(pr, undefined, undefined, "worker-3 auth"), true);
  assert.equal(matches(pr, undefined, undefined, "sample-checkout"), false);
});
