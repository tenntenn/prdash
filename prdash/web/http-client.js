(() => {
  let token = "";
  let pendingToken = null;

  async function operationToken() {
    if (token) return token;
    if (!pendingToken) {
      pendingToken = (async () => {
        const response = await fetch("/api/auth", {
          headers: { "X-Prdash-Client": "dashboard" },
          cache: "no-store",
          credentials: "same-origin",
        });
        if (!response.ok) throw new Error("Could not refresh dashboard credentials. Please retry.");
        const body = await response.json();
        if (typeof body.token !== "string" || !body.token) {
          throw new Error("Dashboard returned invalid credentials.");
        }
        token = body.token;
        return token;
      })().finally(() => { pendingToken = null; });
    }
    return pendingToken;
  }

  async function post(path, payload) {
    const body = JSON.stringify(payload || {});
    let sentToken = await operationToken();
    const send = () => fetch(path, {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: `Bearer ${sentToken}` },
      credentials: "same-origin",
      body,
    });
    let response = await send();
    if (response.status === 401) {
      const rejection = await response.clone().json().catch(() => null);
      if (rejection?.code === "authentication_required") {
        // This response is emitted before the operation is dispatched. Never
        // retry network errors or other failures whose execution is uncertain.
        if (token === sentToken) token = "";
        sentToken = await operationToken();
        response = await send();
      }
    }
    return response;
  }

  window.prdashHTTP = Object.freeze({ post });
})();
