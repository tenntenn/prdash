(() => {
  let state = null;
  let activeFilter = "all";
  let activeSessionFilter = "";
  let desktopAlertsEnabled = false;
  const drafts = new Map();
  const openPRs = new Set();
  const openSessionLogs = new Set();
  const seenNotifications = new Set();
  let initialLoadComplete = false;

  async function apiPost(path, payload) {
    const resp = await fetch(path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload || {}),
    });
    const data = await resp.json();
    if (!resp.ok) {
      throw new Error(data.error || `Request failed: ${resp.status}`);
    }
    try {
      await refreshState();
    } catch (_) {
      showToast("State refresh failed", "Your change was accepted. The dashboard will reconnect.");
    }
    return data;
  }

  function rememberDraft(form) {
    const key = form.dataset.draftKey;
    const draft = drafts.get(key) || { values: {}, revision: 0, pending: false, error: "" };
    for (const input of form.querySelectorAll("input[name]")) {
      if (draft.values[input.name] !== input.value) {
        draft.values[input.name] = input.value;
        draft.revision++;
      }
    }
    drafts.set(key, draft);
    return draft;
  }

  function restoreDraft(form) {
    const draft = drafts.get(form.dataset.draftKey);
    if (!draft) return;
    for (const input of form.querySelectorAll("input[name]")) {
      if (Object.hasOwn(draft.values, input.name)) input.value = draft.values[input.name];
    }
    form.setAttribute("aria-busy", String(draft.pending));
    form.querySelector("button[type='submit']").disabled = draft.pending;
    let error = form.querySelector("[data-draft-error]");
    if (!error && draft.error) {
      error = document.createElement("div");
      error.dataset.draftError = "";
      error.className = "draft-error";
      error.setAttribute("role", "alert");
      form.appendChild(error);
    }
    if (error) {
      error.textContent = draft.error;
      error.hidden = !draft.error;
    }
  }

  function currentDraftForm(key) {
    return Array.from(document.querySelectorAll("form[data-draft-key]"))
      .find((form) => form.dataset.draftKey === key);
  }

  function renderWithDrafts(root, update) {
    for (const form of root.querySelectorAll("form[data-draft-key]")) rememberDraft(form);
    const input = document.activeElement;
    const form = input?.closest("form[data-draft-key]");
    const editing = form && root.contains(input) && input.matches("input[name]") ? {
      key: form.dataset.draftKey,
      name: input.name,
      start: input.selectionStart,
      end: input.selectionEnd,
      direction: input.selectionDirection,
      scrollLeft: input.scrollLeft,
    } : null;
    update();
    for (const form of root.querySelectorAll("form[data-draft-key]")) restoreDraft(form);
    if (editing) {
      const restored = currentDraftForm(editing.key)?.elements.namedItem(editing.name);
      if (restored) {
        restored.focus({ preventScroll: true });
        if (editing.start !== null) {
          restored.setSelectionRange(editing.start, editing.end, editing.direction);
        }
        restored.scrollLeft = editing.scrollLeft;
      }
    }
  }

  async function submitDraft(form, path, payload) {
    const key = form.dataset.draftKey;
    const draft = rememberDraft(form);
    if (draft.pending) return;
    const submittedRevision = draft.revision;
    draft.pending = true;
    draft.error = "";
    restoreDraft(form);
    try {
      await apiPost(path, payload);
      const current = currentDraftForm(key);
      if (current) rememberDraft(current);
      // A response may arrive after an SSE render, a filter change, or more typing.
      if (draft.revision === submittedRevision) {
        for (const name of Object.keys(draft.values)) draft.values[name] = "";
        draft.revision++;
      }
    } catch (err) {
      draft.error = err.message || "Could not send. Your draft has been kept.";
    } finally {
      draft.pending = false;
      const current = currentDraftForm(key);
      if (current) restoreDraft(current);
    }
  }

  document.addEventListener("input", (event) => {
    const form = event.target.closest("form[data-draft-key]");
    if (form) rememberDraft(form);
  });

  async function refreshState() {
    const resp = await fetch("/api/state");
    if (!resp.ok) return;
    const snap = await resp.json();
    applySnapshot(snap);
  }

  function applySnapshot(snap) {
    if (initialLoadComplete && Array.isArray(snap.notifications)) {
      for (const n of snap.notifications) {
        if (!seenNotifications.has(n.id) && !n.read) {
          showToast(n.title, n.message);
        }
      }
    }
    if (Array.isArray(snap.notifications)) {
      for (const n of snap.notifications) {
        seenNotifications.add(n.id);
      }
    }
    initialLoadComplete = true;
    state = snap;
    render();
  }

  function showToast(title, message) {
    if (desktopAlertsEnabled && "Notification" in window && Notification.permission === "granted") {
      try {
        new Notification(title, { body: message });
      } catch (_) {}
    }
    const container = document.getElementById("toast-container");
    if (!container) return;
    const toast = document.createElement("div");
    toast.className = "toast";
    const strong = document.createElement("strong");
    strong.textContent = title;
    const span = document.createElement("span");
    span.textContent = message;
    toast.append(strong, span);
    container.appendChild(toast);
    setTimeout(() => {
      toast.remove();
    }, 5000);
  }

  function escapeHTML(str) {
    return String(str ?? "")
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;");
  }

  function formatStatusLabel(status) {
    return status === "waiting_for_human" ? "Waiting for Human" : "Agent Working";
  }

  function formatTierOptions(selected) {
    const tiers = [
      { value: "pine", label: "Pine (High)" },
      { value: "bamboo", label: "Bamboo (Std)" },
      { value: "plum", label: "Plum (Fast)" },
    ];
    return tiers
      .map(
        (t) =>
          `<option value="${t.value}" ${t.value === selected ? "selected" : ""}>${t.label}</option>`
      )
      .join("");
  }

  function findSession(sessionID) {
    if (!state) return null;
    if (state.controller_session && state.controller_session.id === sessionID) {
      return state.controller_session;
    }
    return (state.sessions || []).find((s) => s.id === sessionID) || null;
  }

  function findWorktree(worktreeID) {
    if (!state) return null;
    return (state.worktrees || []).find((w) => w.id === worktreeID) || null;
  }

  function render() {
    if (!state) return;

    // Header & Summary
    document.getElementById("repo-full-name").textContent = state.repository.full_name;
    document.getElementById("repo-branch").textContent = `default: ${state.repository.default_branch}`;
    document.getElementById("repo-path").textContent = state.repository.local_path;

    document.getElementById("stat-waiting").textContent = String(state.summary.waiting_for_human);
    document.getElementById("stat-working").textContent = String(state.summary.agent_working);
    document.getElementById("stat-stacks").textContent = String(state.summary.stacked_chains);
    document.getElementById("stat-automode").textContent = String(state.summary.auto_mode_enabled);

    renderControllerPanel();
    renderNotificationsPanel();
    renderWorkerSessionsPanel();
    renderPRList();
  }

  function renderControllerPanel() {
    const ctrl = state.controller_session;
    document.getElementById("controller-name").textContent = ctrl.name;
    document.getElementById("controller-task").textContent = ctrl.active_task;

    const modelSel = document.getElementById("controller-model-tier");
    const subSel = document.getElementById("controller-subagent-tier");
    modelSel.value = ctrl.model_tier;
    subSel.value = ctrl.subagent_model_tier;
    document.getElementById("controller-resolved-model").textContent =
      `Active: ${ctrl.resolved_model} · Subagents: ${ctrl.resolved_subagent_model}`;

    const feed = document.getElementById("controller-messages");
    const recent = (ctrl.messages || []).slice(-6).reverse();
    feed.innerHTML = recent
      .map(
        (m) => `
        <div class="feed-msg">
          <strong>${escapeHTML(m.from)} → ${escapeHTML(m.to)}</strong>
          ${m.pr_number ? `<span class="meta-pill">PR #${m.pr_number}</span>` : ""}
          <div>${escapeHTML(m.text)}</div>
        </div>`
      )
      .join("");
  }

  function renderNotificationsPanel() {
    const unreadEl = document.getElementById("unread-count");
    unreadEl.textContent = String(state.summary.unread_alerts);

    const listEl = document.getElementById("notification-list");
    const items = state.notifications || [];
    if (!items.length) {
      listEl.innerHTML = `<div class="feed-msg">No notifications yet.</div>`;
      return;
    }
    listEl.innerHTML = items
      .map(
        (n) => `
        <div class="notif-card ${n.read ? "" : "unread"}" data-notif-id="${escapeHTML(n.id)}">
          <div class="notif-title">${escapeHTML(n.title)}</div>
          <div>${escapeHTML(n.message)}</div>
        </div>`
      )
      .join("");
  }

  function renderWorkerSessionsPanel() {
    const sessionSelect = document.getElementById("session-filter");
    if (sessionSelect) {
      const currentVal = activeSessionFilter;
      const opts = [`<option value="">All Sessions / Worktrees</option>`];
      for (const s of state.sessions || []) {
        opts.push(
          `<option value="${escapeHTML(s.id)}" ${s.id === currentVal ? "selected" : ""}>${escapeHTML(
            s.name
          )} · ${escapeHTML(s.worktree_id)}</option>`
        );
      }
      sessionSelect.innerHTML = opts.join("");
    }
    const container = document.getElementById("worker-sessions-list");
    const sessions = state.sessions || [];
    const content = sessions
      .map((s) => {
        const wt = findWorktree(s.worktree_id);
        const logOpen = openSessionLogs.has(s.id);
        const msgs = (s.messages || []).slice(-5);
        return `
        <div class="worker-card" data-session-card="${escapeHTML(s.id)}">
          <div class="worker-card-header">
            <span class="worker-name">${escapeHTML(s.name)}</span>
            <span class="worktree-pill">${escapeHTML(s.worktree_id)} (${wt ? escapeHTML(wt.branch) : "detached"})</span>
          </div>
          <div class="controller-task">${escapeHTML(s.active_task)}</div>
          <div class="tier-row">
            <label class="tier-label">
              <span>Session Tier</span>
              <select class="select-compact" data-session-tier="${escapeHTML(s.id)}">
                ${formatTierOptions(s.model_tier)}
              </select>
            </label>
            <label class="tier-label">
              <span>Subagent Tier</span>
              <select class="select-compact" data-subagent-tier="${escapeHTML(s.id)}">
                ${formatTierOptions(s.subagent_model_tier)}
              </select>
            </label>
          </div>
          <div class="resolved-model-hint">
            Model: ${escapeHTML(s.resolved_model)} (Sub: ${escapeHTML(s.resolved_subagent_model)})
          </div>
          <form class="instruction-form" data-session-form="${escapeHTML(s.id)}" data-draft-key="session:${escapeHTML(s.id)}">
            <input type="text" name="text" placeholder="Direct instruction to ${escapeHTML(s.id)}..." required>
            <button type="submit" class="btn btn-secondary btn-sm">Send</button>
          </form>
          <details data-session-log="${escapeHTML(s.id)}" ${logOpen ? "open" : ""}>
            <summary class="resolved-model-hint" style="cursor:pointer">
              Inter-Session Log (${(s.messages || []).length} events)
            </summary>
            <div class="message-feed" style="margin-top:6px">
              ${msgs
                .map(
                  (m) => `
                  <div class="feed-msg">
                    <strong>${escapeHTML(m.direction.toUpperCase())}:</strong> ${escapeHTML(m.text)}
                  </div>`
                )
                .join("")}
            </div>
          </details>
        </div>`;
      })
      .join("");
    renderWithDrafts(container, () => { container.innerHTML = content; });
  }

  function prMatchesFilter(pr) {
    if (activeSessionFilter && pr.session_id !== activeSessionFilter) {
      return false;
    }
    if (activeFilter === "waiting_for_human") {
      return pr.status === "waiting_for_human";
    }
    if (activeFilter === "working") {
      return pr.status === "working";
    }
    if (activeFilter === "stacked") {
      return Boolean(pr.stack_id);
    }
    return true;
  }

  function renderPRList() {
    const listEl = document.getElementById("pr-list");
    const groups = state.stacks || [];

    const htmlParts = [];
    for (const group of groups) {
      const visiblePRs = (group.pull_requests || []).filter(prMatchesFilter);
      if (!visiblePRs.length) continue;

      if (group.is_stacked) {
        const chainNums = group.pull_requests.map((p) => `#${p.number}`).join(" → ");
        htmlParts.push(`
          <div class="stack-container is-stacked" data-stack-group="${escapeHTML(group.id)}">
            <div class="stack-header">
              <span class="stack-badge">Stacked Chain · ${escapeHTML(group.title)} (${group.pull_requests.length} PRs)</span>
              <span class="stack-chain-hint">${escapeHTML(chainNums)}</span>
            </div>
            ${visiblePRs.map((pr) => renderPRCard(pr)).join("")}
          </div>
        `);
      } else {
        htmlParts.push(`
          <div class="stack-container" data-stack-group="${escapeHTML(group.id)}">
            ${visiblePRs.map((pr) => renderPRCard(pr)).join("")}
          </div>
        `);
      }
    }

    renderWithDrafts(listEl, () => {
      listEl.innerHTML = htmlParts.length
        ? htmlParts.join("")
        : `<div class="panel">No pull requests match the selected filter.</div>`;
    });
  }

  function renderPRCard(pr) {
    const isOpen = openPRs.has(pr.number);
    const sess = findSession(pr.session_id);
    const wt = findWorktree(pr.worktree_id);
    const nextStatus = pr.status === "waiting_for_human" ? "working" : "waiting_for_human";
    const nextStatusBtnLabel =
      pr.status === "waiting_for_human"
        ? "Hand Off to Agent (Set Working)"
        : "Mark Waiting for Human";

    const stackBadge = pr.stack_id
      ? `<span class="stack-step-pill">Stack ${pr.stack_order}/${pr.stack_size}${
          pr.parent_pr_number ? ` · base #${pr.parent_pr_number}` : " · root"
        }</span>`
      : "";

    const conflictBadge = pr.has_conflicts
      ? `<span class="meta-pill conflict">CONFLICT</span>`
      : "";

    const ciClass =
      pr.ci_status === "failing"
        ? "ci-failing"
        : pr.ci_status === "passing"
        ? "ci-passing"
        : "";

    return `
      <article class="pr-card" data-pr-card="${pr.number}">
        <details class="pr-details" data-pr-details="${pr.number}" ${isOpen ? "open" : ""}>
          <summary class="pr-summary">
            <div class="pr-summary-left">
              <span class="chevron">▶</span>
              <span class="status-badge ${escapeHTML(pr.status)}" data-pr-status-badge="${pr.number}">
                <span class="status-dot"></span>
                ${escapeHTML(formatStatusLabel(pr.status))}
              </span>
              <span class="pr-number">#${pr.number}</span>
              <span class="pr-title">${escapeHTML(pr.title)}</span>
              ${stackBadge}
            </div>
            <div class="pr-summary-right" data-stop-summary-toggle="true">
              ${conflictBadge}
              <span class="meta-pill ${ciClass}">CI: ${escapeHTML(pr.ci_status)}</span>
              <span class="meta-pill">${escapeHTML(sess ? sess.name : pr.session_id)} · ${escapeHTML(pr.worktree_id)}</span>
              <label class="auto-toggle" title="Toggle autonomous routine and review handling for this PR">
                <input type="checkbox" data-auto-toggle="${pr.number}" ${pr.auto_mode ? "checked" : ""}>
                <span>Auto-Mode</span>
              </label>
              <select class="select-compact" data-pr-tier="${pr.number}" aria-label="Model tier for PR #${pr.number}">
                ${formatTierOptions(pr.model_tier)}
              </select>
            </div>
          </summary>

          <div class="pr-body">
            <p class="pr-desc">${escapeHTML(pr.description)}</p>
            <div class="pr-meta-grid">
              <span class="meta-pill">Branch: ${escapeHTML(pr.branch)} ← ${escapeHTML(pr.base_branch)}</span>
              <span class="meta-pill">Worktree: ${escapeHTML(wt ? wt.path : pr.worktree_id)}</span>
              <span class="meta-pill">Diff: +${pr.additions} / -${pr.deletions} (${pr.changed_files} files)</span>
              <span class="meta-pill">Review: ${escapeHTML(pr.review_status)}</span>
              <span class="meta-pill">Model: ${escapeHTML(pr.resolved_model)} (${escapeHTML(pr.model_tier)})</span>
            </div>

            <div class="pr-actions-grid">
              <div class="action-box">
                <h4 class="action-box-title">Routine Operations</h4>
                <div class="button-cluster">
                  <button type="button" class="btn btn-sm" data-routine="${pr.number}" data-action="resolve_conflicts">
                    Resolve Conflicts
                  </button>
                  <button type="button" class="btn btn-sm" data-routine="${pr.number}" data-action="rebase_stack">
                    Rebase Stack
                  </button>
                  <button type="button" class="btn btn-sm" data-routine="${pr.number}" data-action="fix_ci">
                    Fix CI
                  </button>
                  <button type="button" class="btn btn-sm" data-routine="${pr.number}" data-action="sync_branch">
                    Sync Branch
                  </button>
                  <button type="button" class="btn btn-sm" data-routine="${pr.number}" data-action="update_summary">
                    Update Summary
                  </button>
                </div>
              </div>

              <div class="action-box">
                <h4 class="action-box-title">Review &amp; Turn Handoff</h4>
                <div class="button-cluster">
                  <button type="button" class="btn btn-secondary btn-sm" data-request-review="${pr.number}">
                    Request Review (sbnn)
                  </button>
                  <button type="button" class="btn btn-primary btn-sm" data-address-review="${pr.number}">
                    Address Review
                  </button>
                  <button type="button" class="btn btn-sm" data-toggle-status="${pr.number}" data-next-status="${nextStatus}">
                    ${escapeHTML(nextStatusBtnLabel)}
                  </button>
                </div>
              </div>

              <div class="action-box">
                <h4 class="action-box-title">Send PR Comment via sbnn</h4>
                <form class="sbnn-form" data-sbnn-form="${pr.number}" data-draft-key="comment:${pr.number}">
                  <input type="text" name="file_path" placeholder="File (optional)">
                  <input type="number" name="line" placeholder="Line" min="1">
                  <input type="text" name="body" placeholder="Review comment via sbnn..." required>
                  <button type="submit" class="btn btn-secondary btn-sm">Comment (sbnn)</button>
                </form>
              </div>
            </div>

            <div class="thread-section">
              <h4 class="action-box-title">Session Message Thread (PR #${pr.number} ↔ ${escapeHTML(sess ? sess.name : pr.session_id)})</h4>
              <div class="thread-list" data-thread-list="${pr.number}">
                ${(pr.thread || [])
                  .map(
                    (m) => `
                    <div class="thread-item role-${escapeHTML(m.role)}">
                      <div class="thread-meta">
                        <span class="thread-author">${escapeHTML(m.author)} (${escapeHTML(m.kind)})</span>
                        <span>${m.file_path ? `${escapeHTML(m.file_path)}${m.line ? `:${m.line}` : ""}` : ""}</span>
                      </div>
                      <p class="thread-text">${escapeHTML(m.text)}</p>
                    </div>`
                  )
                  .join("")}
              </div>
              <form class="instruction-form" data-pr-msg-form="${pr.number}" data-draft-key="pr:${pr.number}">
                <input
                  type="text"
                  name="text"
                  placeholder="Send instruction to ${escapeHTML(sess ? sess.name : pr.session_id)} for PR #${pr.number}..."
                  required
                >
                <button type="submit" class="btn btn-primary">Send to Session</button>
              </form>
            </div>
          </div>
        </details>
      </article>
    `;
  }

  // Event delegation
  document.addEventListener("click", async (e) => {
    // Prevent interactive controls inside <summary> from toggling details open/close
    if (e.target.closest("[data-stop-summary-toggle]") && e.target.matches("select, option, input, label, span")) {
      if (!e.target.matches("input[type='checkbox']")) {
        e.stopPropagation();
      }
    }

    const filterBtn = e.target.closest(".filter-btn");
    if (filterBtn) {
      activeFilter = filterBtn.dataset.filter;
      document.querySelectorAll(".filter-btn").forEach((b) => b.classList.toggle("active", b === filterBtn));
      renderPRList();
      return;
    }

    if (e.target.id === "expand-all-prs") {
      if (state && state.pull_requests) {
        state.pull_requests.forEach((p) => openPRs.add(p.number));
        renderPRList();
      }
      return;
    }

    if (e.target.id === "collapse-all-prs") {
      openPRs.clear();
      renderPRList();
      return;
    }

    if (e.target.id === "mark-notifications-read") {
      await apiPost("/api/notifications/read", { id: "all" });
      return;
    }

    if (e.target.id === "sync-repo-btn") {
      e.target.disabled = true;
      e.target.textContent = "Syncing...";
      try {
        await apiPost("/api/sync", {});
      } finally {
        e.target.disabled = false;
        e.target.textContent = "Sync GitHub & Worktrees";
      }
      return;
    }

    if (e.target.id === "enable-desktop-alerts") {
      if ("Notification" in window && Notification.permission !== "granted") {
        try {
          await Notification.requestPermission();
        } catch (_) {}
      }
      desktopAlertsEnabled = !desktopAlertsEnabled;
      e.target.textContent = desktopAlertsEnabled ? "Browser Alerts: ON" : "Enable Browser Alerts";
      return;
    }

    if (e.target.id === "open-model-settings") {
      const dlg = document.getElementById("model-settings-dialog");
      if (state && state.model_config) {
        document.getElementById("preset-pine").value = state.model_config.pine;
        document.getElementById("preset-bamboo").value = state.model_config.bamboo;
        document.getElementById("preset-plum").value = state.model_config.plum;
      }
      dlg.showModal();
      return;
    }

    if (e.target.id === "close-model-settings") {
      document.getElementById("model-settings-dialog").close();
      return;
    }

    const routineBtn = e.target.closest("[data-routine]");
    if (routineBtn) {
      const prNum = Number(routineBtn.dataset.routine);
      const action = routineBtn.dataset.action;
      openPRs.add(prNum);
      await apiPost(`/api/prs/${prNum}/routine`, { action });
      return;
    }

    const reqReviewBtn = e.target.closest("[data-request-review]");
    if (reqReviewBtn) {
      const prNum = Number(reqReviewBtn.dataset.requestReview);
      openPRs.add(prNum);
      await apiPost(`/api/prs/${prNum}/request-review`, {});
      return;
    }

    const addrReviewBtn = e.target.closest("[data-address-review]");
    if (addrReviewBtn) {
      const prNum = Number(addrReviewBtn.dataset.addressReview);
      openPRs.add(prNum);
      await apiPost(`/api/prs/${prNum}/address-review`, {});
      return;
    }

    const statusBtn = e.target.closest("[data-toggle-status]");
    if (statusBtn) {
      const prNum = Number(statusBtn.dataset.toggleStatus);
      const nextStatus = statusBtn.dataset.nextStatus;
      openPRs.add(prNum);
      await apiPost(`/api/prs/${prNum}/status`, { status: nextStatus });
      return;
    }
  });

  document.addEventListener(
    "toggle",
    (e) => {
      if (e.target.matches("[data-pr-details]")) {
        const prNum = Number(e.target.dataset.prDetails);
        if (e.target.open) {
          openPRs.add(prNum);
        } else {
          openPRs.delete(prNum);
        }
      } else if (e.target.matches("[data-session-log]")) {
        const sessID = e.target.dataset.sessionLog;
        if (e.target.open) {
          openSessionLogs.add(sessID);
        } else {
          openSessionLogs.delete(sessID);
        }
      }
    },
    true
  );

  document.addEventListener("change", async (e) => {
    if (e.target.id === "session-filter") {
      activeSessionFilter = e.target.value;
      renderPRList();
      return;
    }

    if (e.target.matches("[data-auto-toggle]")) {
      const prNum = Number(e.target.dataset.autoToggle);
      const enabled = e.target.checked;
      await apiPost(`/api/prs/${prNum}/auto-mode`, { enabled });
      return;
    }

    if (e.target.matches("[data-pr-tier]")) {
      const prNum = Number(e.target.dataset.prTier);
      const tier = e.target.value;
      await apiPost(`/api/prs/${prNum}/model-tier`, { tier });
      return;
    }

    if (e.target.id === "controller-model-tier" || e.target.id === "controller-subagent-tier") {
      const tier = document.getElementById("controller-model-tier").value;
      const subagentTier = document.getElementById("controller-subagent-tier").value;
      await apiPost("/api/sessions/sess-controller/model-tier", {
        tier,
        subagent_tier: subagentTier,
      });
      return;
    }

    if (e.target.matches("[data-session-tier]") || e.target.matches("[data-subagent-tier]")) {
      const sessID = e.target.dataset.sessionTier || e.target.dataset.subagentTier;
      const card = e.target.closest("[data-session-card]");
      const tier = card.querySelector("[data-session-tier]").value;
      const subagentTier = card.querySelector("[data-subagent-tier]").value;
      await apiPost(`/api/sessions/${encodeURIComponent(sessID)}/model-tier`, {
        tier,
        subagent_tier: subagentTier,
      });
      return;
    }
  });

  document.addEventListener("submit", async (e) => {
    if (e.target.id === "controller-form") {
      e.preventDefault();
      const input = document.getElementById("controller-input");
      const text = input.value.trim();
      if (!text) return;
      await submitDraft(e.target, "/api/controller/instruction", { text });
      return;
    }

    if (e.target.id === "model-settings-form") {
      e.preventDefault();
      const pine = document.getElementById("preset-pine").value.trim();
      const bamboo = document.getElementById("preset-bamboo").value.trim();
      const plum = document.getElementById("preset-plum").value.trim();
      await apiPost("/api/models", { pine, bamboo, plum });
      document.getElementById("model-settings-dialog").close();
      return;
    }

    if (e.target.matches("[data-pr-msg-form]")) {
      e.preventDefault();
      const prNum = Number(e.target.dataset.prMsgForm);
      const input = e.target.querySelector("input[name='text']");
      const text = input.value.trim();
      if (!text) return;
      openPRs.add(prNum);
      await submitDraft(e.target, `/api/prs/${prNum}/messages`, { author: "Human Operator", text });
      return;
    }

    if (e.target.matches("[data-sbnn-form]")) {
      e.preventDefault();
      const prNum = Number(e.target.dataset.sbnnForm);
      const filePath = e.target.querySelector("input[name='file_path']").value.trim();
      const lineVal = Number(e.target.querySelector("input[name='line']").value || 0);
      const bodyInput = e.target.querySelector("input[name='body']");
      const body = bodyInput.value.trim();
      if (!body) return;
      openPRs.add(prNum);
      await submitDraft(e.target, `/api/prs/${prNum}/sbnn-comment`, {
        file_path: filePath,
        line: lineVal,
        body,
      });
      return;
    }

    if (e.target.matches("[data-session-form]")) {
      e.preventDefault();
      const sessID = e.target.dataset.sessionForm;
      const input = e.target.querySelector("input[name='text']");
      const text = input.value.trim();
      if (!text) return;
      openSessionLogs.add(sessID);
      await submitDraft(e.target, `/api/sessions/${encodeURIComponent(sessID)}/instruction`, { text });
      return;
    }
  });

  function connectSSE() {
    if (typeof EventSource === "undefined") {
      refreshState();
      return;
    }
    const es = new EventSource("/api/events");
    es.onmessage = (ev) => {
      try {
        const snap = JSON.parse(ev.data);
        applySnapshot(snap);
      } catch (_) {}
    };
    es.onerror = () => {
      es.close();
      setTimeout(connectSSE, 3000);
    };
  }

  refreshState().then(() => connectSSE());
})();
