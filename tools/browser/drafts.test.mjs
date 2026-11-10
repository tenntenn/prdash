import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { chromium } from "playwright";

let browser;
before(async () => {
  browser = await chromium.launch({
    executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined,
    headless: true,
  });
});
after(async () => { await browser?.close(); });

function snapshot() {
  const sessions = ["sample-session", "other-session"].map((id) => ({
    id, name: id, worktree_id: `wt-${id}`, active_task: "Sample task", messages: [],
    model_tier: "bamboo", subagent_model_tier: "plum", resolved_model: "sample-model", resolved_subagent_model: "sample-submodel",
  }));
  const prs = [101, 102].map((number, i) => ({
    number, title: `Sample PR ${number}`, status: "working", session_id: sessions[i].id,
    worktree_id: sessions[i].worktree_id, model_tier: "bamboo", branch: `sample-${number}`, base_branch: "main",
    ci_status: "passing", review_status: "review_pending", thread: [], additions: 1, deletions: 0, changed_files: 1,
  }));
  return {
    repository: { full_name: "example/sample", default_branch: "main", local_path: "/tmp/sample" },
    summary: { waiting_for_human: 0, agent_working: 2, stacked_chains: 0, auto_mode_enabled: 0, unread_alerts: 0 },
    controller_session: { ...sessions[0], id: "controller", name: "Sample controller" },
    sessions, worktrees: [], notifications: [], pull_requests: prs,
    stacks: [{ id: "sample", is_stacked: false, pull_requests: prs }],
  };
}

async function setup(t) {
  const snap = snapshot();
  const clients = new Set();
  let connected;
  const sseReady = new Promise((resolve) => { connected = resolve; });
  let posted;
  const postReceived = new Promise((resolve) => { posted = resolve; });
  const fixture = { snap, mode: "success", requests: [], pending: null, postReceived, failRefresh: false, token: "sample-initial-token", attempts: [], bootstraps: 0, failAuth: false };
  fixture.publish = () => {
    for (const client of clients) client.write(`data: ${JSON.stringify(snap)}\n\n`);
  };
  fixture.finish = (res) => {
    fixture.publish(); // The server may broadcast before the HTTP response reaches the browser.
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end("{}");
  };
  const server = createServer(async (req, res) => {
    if (req.url === "/api/events") {
      res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
      clients.add(res);
      res.write(`data: ${JSON.stringify(snap)}\n\n`);
      req.on("close", () => clients.delete(res));
      connected();
      return;
    }
    if (req.url === "/api/auth") {
      fixture.bootstraps++;
      assert.equal(req.headers["x-prdash-client"], "dashboard");
      res.writeHead(fixture.failAuth ? 503 : 200, { "Content-Type": "application/json" });
      res.end(JSON.stringify(fixture.failAuth ? { error: "Temporary refresh failure" } : { token: fixture.token }));
      return;
    }
    if (req.method === "POST") {
      let body = "";
      for await (const chunk of req) body += chunk;
      fixture.attempts.push({ path: req.url, body });
      if (req.headers.authorization !== `Bearer ${fixture.token}`) {
        fixture.publish();
        res.writeHead(401, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ code: "authentication_required", error: "Expired credentials" }));
        return;
      }
      fixture.requests.push({ path: req.url, payload: JSON.parse(body) });
      posted();
      if (fixture.mode === "network-error") { res.destroy(); return; }
      if (fixture.mode === "server-error") {
        res.writeHead(503, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ error: "Sample server failure" }));
        return;
      }
      if (fixture.mode === "pending") { fixture.pending = res; return; }
      fixture.finish(res);
      return;
    }
    if (req.url === "/api/state") {
      if (fixture.failRefresh && fixture.requests.length) { res.destroy(); return; }
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify(snap));
      return;
    }
    const assets = { "/http-client.js": ["http-client.js", "text/javascript"], "/": ["index.html", "text/html"], "/app.js": ["app.js", "text/javascript"], "/styles.css": ["styles.css", "text/css"] };
    const asset = assets[req.url];
    if (!asset) { res.writeHead(404); res.end(); return; }
    res.writeHead(200, { "Content-Type": asset[1] });
    res.end(await readFile(new URL(`../../prdash/web/${asset[0]}`, import.meta.url)));
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const context = await browser.newContext();
  const page = await context.newPage();
  t.after(async () => {
    await context.close();
    for (const client of clients) client.end();
    fixture.pending?.destroy();
    await new Promise((resolve) => server.close(resolve));
  });
  await page.goto(`http://127.0.0.1:${server.address().port}`);
  await sseReady;
  await page.locator('[data-pr-card="101"]').waitFor();
  await page.locator("#expand-all-prs").click();
  fixture.page = page;
  return fixture;
}

const forms = [
  { selector: '[data-pr-msg-form="101"]', values: { text: "sample PR draft" }, path: "/api/prs/101/messages" },
  { selector: '[data-session-form="sample-session"]', values: { text: "sample session draft" }, path: "/api/sessions/sample-session/instruction" },
  { selector: '[data-sbnn-form="101"]', values: { file_path: "sample.go", line: "12", body: "sample comment draft" }, path: "/api/prs/101/sbnn-comment" },
  { selector: "#controller-form", values: { text: "sample controller draft" }, path: "/api/controller/instruction" },
];
async function fillForm(page, form) {
  for (const [name, value] of Object.entries(form.values)) await page.locator(`${form.selector} [name="${name}"]`).fill(value);
}
async function assertValues(page, form, cleared = false) {
  for (const [name, value] of Object.entries(form.values)) assert.equal(await page.locator(`${form.selector} [name="${name}"]`).inputValue(), cleared ? "" : value);
}
async function submit(page, selector) {
  await page.locator(selector).evaluate((form) => form.requestSubmit());
}
async function settled(page, selector) {
  await page.waitForFunction((s) => document.querySelector(s)?.getAttribute("aria-busy") === "false", selector);
}

test("SSE updates preserve all drafts, focus, and text selection", async (t) => {
  const f = await setup(t);
  for (const form of forms) await fillForm(f.page, form);
  const editing = f.page.locator('[data-pr-msg-form="101"] [name="text"]');
  await editing.focus();
  await editing.evaluate((input) => input.setSelectionRange(2, 8, "backward"));
  f.snap.pull_requests[1].title = "Updated unrelated PR";
  f.publish();
  await f.page.getByText("Updated unrelated PR", { exact: true }).waitFor();
  for (const form of forms) await assertValues(f.page, form);
  assert.deepEqual(await editing.evaluate((input) => [document.activeElement === input, input.selectionStart, input.selectionEnd, input.selectionDirection]), [true, 2, 8, "backward"]);
});

for (const form of forms) {
  test(`${form.selector}: server failure retains input and an inline error across SSE`, async (t) => {
    const f = await setup(t);
    f.mode = "server-error";
    await fillForm(f.page, form);
    await submit(f.page, form.selector);
    await f.page.locator(`${form.selector} [role="alert"]`).waitFor();
    await assertValues(f.page, form);
    f.snap.pull_requests[1].title = "Updated after failure";
    f.publish();
    await f.page.getByText("Updated after failure", { exact: true }).waitFor();
    assert.equal(await f.page.locator(`${form.selector} [role="alert"]`).textContent(), "Sample server failure");
    await assertValues(f.page, form);
    assert.equal(f.requests[0].path, form.path);
  });

  test(`${form.selector}: success clears only the submitted form`, async (t) => {
    const f = await setup(t);
    for (const draft of forms) await fillForm(f.page, draft);
    await submit(f.page, form.selector);
    await settled(f.page, form.selector);
    for (const draft of forms) await assertValues(f.page, draft, draft === form);
  });
}

test("network failure keeps the draft available for retry", async (t) => {
  const f = await setup(t);
  f.mode = "network-error";
  await fillForm(f.page, forms[0]);
  await submit(f.page, forms[0].selector);
  await f.page.locator(`${forms[0].selector} [role="alert"]`).waitFor();
  await assertValues(f.page, forms[0]);
  f.mode = "success";
  await submit(f.page, forms[0].selector);
  await settled(f.page, forms[0].selector);
  await assertValues(f.page, forms[0], true);
});

test("typing during a pending request survives its success and duplicate submission is blocked", async (t) => {
  const f = await setup(t);
  f.mode = "pending";
  await fillForm(f.page, forms[0]);
  await submit(f.page, forms[0].selector);
  await f.postReceived;
  f.snap.pull_requests[1].title = "Updated while sending";
  f.publish();
  await f.page.getByText("Updated while sending", { exact: true }).waitFor();
  assert.equal(await f.page.locator(`${forms[0].selector} button`).isDisabled(), true);
  await submit(f.page, forms[0].selector);
  await f.page.locator(`${forms[0].selector} [name="text"]`).fill("sample next draft");
  f.finish(f.pending);
  await settled(f.page, forms[0].selector);
  assert.equal(f.requests.length, 1);
  assert.equal(await f.page.locator(`${forms[0].selector} [name="text"]`).inputValue(), "sample next draft");
});

test("filtering a PR out and back keeps its draft", async (t) => {
  const f = await setup(t);
  await fillForm(f.page, forms[0]);
  await f.page.locator('[data-filter="waiting_for_human"]').click();
  assert.equal(await f.page.locator(forms[0].selector).count(), 0);
  await f.page.locator('[data-filter="all"]').click();
  await assertValues(f.page, forms[0]);
});

test("an accepted submission is cleared even when the following state refresh fails", async (t) => {
  const f = await setup(t);
  f.failRefresh = true;
  await fillForm(f.page, forms[0]);
  await submit(f.page, forms[0].selector);
  await settled(f.page, forms[0].selector);
  await assertValues(f.page, forms[0], true);
  await f.page.getByText("Your change was accepted. The dashboard will reconnect.", { exact: true }).waitFor();
  assert.equal(f.requests.length, 1);
});

test("partial sync exposes errors and refreshed lifecycle without losing drafts", async (t) => {
  const fixture = await setup(t);
  const { page, snap } = fixture;
  await fillForm(page, forms[0]);
  fixture.mode = "server-error";
  snap.sync = { git_error: "Sample Git unavailable", github_error: "" };
  snap.pull_requests[0].github_state = "open";
  snap.pull_requests[0].is_draft = true;
  snap.pull_requests[0].mergeability = "unknown";
  snap.pull_requests[0].ci_status = "unknown";
  await page.locator("#sync-repo-btn").click();
  await page.waitForFunction(() => document.querySelector("#repository-sync-status")?.textContent.includes("Sample Git unavailable"));
  assert.match(await page.locator("#toast-container").textContent(), /Repository sync incomplete/);
  const card = await page.locator('[data-pr-card="101"]').textContent();
  assert.match(card, /GitHub: open · Draft/);
  assert.match(card, /Mergeability: unknown/);
  assert.match(card, /CI: unknown/);
  await assertValues(page, forms[0]);
});

test("expired credentials refresh without dropping drafts or executing twice", async (t) => {
  const f = await setup(t);
  await f.page.evaluate(() => window.prdashHTTP.post("/api/warmup", {}));
  f.requests.length = 0;
  f.attempts.length = 0;
  f.token = "sample-rotated-token";
  for (const form of forms) await fillForm(f.page, form);
  const editing = f.page.locator(`${forms[1].selector} [name="text"]`);
  await editing.focus();
  await editing.evaluate((input) => input.setSelectionRange(2, 8, "backward"));
  await submit(f.page, forms[0].selector);
  await settled(f.page, forms[0].selector);
  await assertValues(f.page, forms[0], true);
  for (const form of forms.slice(1)) await assertValues(f.page, form);
  assert.deepEqual(await editing.evaluate((input) => [document.activeElement === input, input.selectionStart, input.selectionEnd, input.selectionDirection]), [true, 2, 8, "backward"]);
  assert.equal(f.requests.length, 1);
  assert.equal(f.attempts.length, 2);
  assert.equal(f.attempts[0].body, f.attempts[1].body);
  assert.equal(f.bootstraps, 2);
});

test("failed credential refresh preserves submitted draft for explicit retry", async (t) => {
  const f = await setup(t);
  await f.page.evaluate(() => window.prdashHTTP.post("/api/warmup", {}));
  f.requests.length = 0;
  f.attempts.length = 0;
  f.token = "sample-rotated-token";
  f.failAuth = true;
  await fillForm(f.page, forms[0]);
  await submit(f.page, forms[0].selector);
  await settled(f.page, forms[0].selector);
  await assertValues(f.page, forms[0]);
  assert.equal(f.requests.length, 0);
  assert.equal(f.attempts.length, 1);
  await f.page.getByText("Could not refresh dashboard credentials. Please retry.", { exact: true }).waitFor();
  f.failAuth = false;
  await submit(f.page, forms[0].selector);
  await settled(f.page, forms[0].selector);
  await assertValues(f.page, forms[0], true);
  assert.equal(f.requests.length, 1);
  assert.equal(f.attempts.length, 2);
  assert.equal(f.attempts[0].body, f.attempts[1].body);
});
