import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = await readFile(new URL("../prdash/web/http-client.js", import.meta.url), "utf8");

function client(fetch) {
  const window = {};
  vm.runInNewContext(source, { window, fetch });
  return window.prdashHTTP;
}

function json(body, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

test("expired credentials are refreshed once without changing payload or duplicating execution", async () => {
  const requests = [];
  let tokens = 0;
  let executed = 0;
  const api = client(async (path, options) => {
    requests.push({ path, options });
    if (path === "/api/auth") return json({ token: ++tokens === 1 ? "sample-old-token" : "sample-new-token" });
    if (options.headers.Authorization === "Bearer sample-old-token") return json({ code: "authentication_required" }, 401);
    executed++;
    return json({ accepted: true });
  });
  const payload = { text: "sample unsent draft\nwith second line" };
  const result = await api.post("/api/controller/instruction", payload);
  assert.equal(result.status, 200);
  assert.equal(tokens, 2);
  assert.equal(executed, 1);
  assert.equal(requests.length, 4);
  const posts = requests.filter((request) => request.path !== "/api/auth");
  assert.equal(posts[0].options.body, posts[1].options.body);
  assert.equal(payload.text, "sample unsent draft\nwith second line");
  assert.equal(requests[0].options.headers["X-Prdash-Client"], "dashboard");
  assert.equal(requests[0].options.cache, "no-store");
});

test("a rejected refreshed token is not retried again", async () => {
  let auth = 0;
  let posts = 0;
  const api = client(async (path) => {
    if (path === "/api/auth") { auth++; return json({ token: "sample-token" }); }
    posts++;
    return json({ code: "authentication_required" }, 401);
  });
  assert.equal((await api.post("/api/sample", {})).status, 401);
  assert.equal(auth, 2);
  assert.equal(posts, 2);
});

for (const status of [401, 403, 500, 503]) {
  test(`status ${status} without the pre-execution rejection code is not replayed`, async () => {
    let posts = 0;
    const api = client(async (path) => {
      if (path === "/api/auth") return json({ token: "sample-token" });
      posts++;
      return json({ error: "sample failure" }, status);
    });
    assert.equal((await api.post("/api/sample", {})).status, status);
    assert.equal(posts, 1);
  });
}

test("a network failure after dispatch is never replayed", async () => {
  let posts = 0;
  const api = client(async (path) => {
    if (path === "/api/auth") return json({ token: "sample-token" });
    posts++;
    throw new Error("sample connection closed after acceptance");
  });
  await assert.rejects(api.post("/api/sample", {}), /connection closed/);
  assert.equal(posts, 1);
});

test("failed token refresh leaves the original payload intact and permits a later explicit retry", async () => {
  let auth = 0;
  let posts = 0;
  const api = client(async (path) => {
    if (path === "/api/auth") {
      if (++auth === 1) return json({ error: "sample unavailable" }, 503);
      return json({ token: "sample-token" });
    }
    posts++;
    return json({ accepted: true });
  });
  const payload = { text: "sample draft" };
  await assert.rejects(api.post("/api/sample", payload), /credentials/);
  assert.equal(payload.text, "sample draft");
  assert.equal(posts, 0);
  assert.equal((await api.post("/api/sample", payload)).status, 200);
  assert.equal(posts, 1);
});

test("concurrent requests share one token acquisition", async () => {
  let auth = 0;
  let posts = 0;
  const api = client(async (path) => {
    if (path === "/api/auth") { auth++; return json({ token: "sample-token" }); }
    posts++;
    return json({ accepted: true });
  });
  const responses = await Promise.all([api.post("/api/sample", { text: "one" }), api.post("/api/sample", { text: "two" })]);
  assert.equal(auth, 1);
  assert.equal(posts, 2);
  assert.ok(responses.every((response) => response.status === 200));
});
