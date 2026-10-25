"use strict";

// ---------- helpers ----------
const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

// h("div.card", {onclick}, child, "text", ...)
function h(tag, attrs, ...children) {
  const [name, ...classes] = tag.split(".");
  const el = document.createElement(name);
  if (classes.length) el.className = classes.join(" ");
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "value") el.value = v;
    else if (k === "checked") el.checked = v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat(Infinity)) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = typeof body === "string" ? body : JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  if (res.status === 204) return null;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const err = new Error(data.error || gatewayMessage(res.status) || `HTTP ${res.status}`);
    err.status = res.status;
    err.data = data;
    throw err;
  }
  return data;
}

// Errors from a proxy in front of the app (Cloudflare uses 52x) have no
// JSON body; explain them instead of showing a bare status code.
function gatewayMessage(status) {
  if (status === 524) return "The request timed out at the proxy (HTTP 524): the app took longer than about 100 seconds to answer.";
  if (status === 502 || status === 521 || status === 522 || status === 523) return `The app couldn't be reached through the proxy (HTTP ${status}). Is it running?`;
  if (status === 503 || status === 504) return `The app is unavailable or timed out (HTTP ${status}). Try again in a moment.`;
  return "";
}

const fmtDuration = (s) => (s % 3600 === 0 && s >= 3600 ? `${s / 3600}h` : s % 60 === 0 && s >= 60 ? `${s / 60}m` : `${s}s`);
const fmtTime = (s) => (s ? new Date(s).toLocaleString() : "");
const ago = (s) => {
  if (!s) return "never";
  const sec = Math.round((Date.now() - new Date(s)) / 1000);
  if (sec < 60) return `${sec}s ago`;
  if (sec < 3600) return `${Math.round(sec / 60)}m ago`;
  if (sec < 86400) return `${Math.round(sec / 3600)}h ago`;
  return `${Math.round(sec / 86400)}d ago`;
};

// ---------- state ----------
const state = { me: null, catalog: null, rules: [], events: [], editing: null, live: null };
const isAdmin = () => state.me && state.me.role === "admin";

// ---------- boot ----------
document.addEventListener("DOMContentLoaded", boot);

async function boot() {
  $("#reload-ui").addEventListener("click", reloadUI);
  const params = new URLSearchParams(location.search);
  if (params.get("login") === "denied") $("#login-msg").textContent = "That Discord account isn't on the allow list.";
  if (params.has("login")) history.replaceState(null, "", "/");

  try {
    const me = await api("GET", "/api/me");
    state.me = me.user;
  } catch (e) {
    $("#login").classList.remove("hidden");
    return;
  }
  renderMe();
  document.body.classList.toggle("is-admin", isAdmin());
  $$(".admin-only").forEach((el) => el.classList.toggle("hidden", !isAdmin()));
  $$(".viewer-only").forEach((el) => el.classList.toggle("hidden", isAdmin()));
  $("#tabs").classList.remove("hidden");

  state.catalog = await api("GET", "/api/catalog");
  $("#field-list").append(...state.catalog.fields.map((f) => h("option", { value: f })));

  $$("#tabs button").forEach((b) => b.addEventListener("click", () => {
    if (b.dataset.tab === "history" && state.historyScope) {
      // The tab itself shows everything, not the last item opened from a card.
      state.historyScope = null;
      $("#history-kind").value = "";
    }
    showTab(b.dataset.tab);
  }));
  $("#new-rule").addEventListener("click", () => openEditor(null));
  $("#events-refresh").addEventListener("click", () => loadEvents());
  $("#events-more").addEventListener("click", () => loadEvents(true));
  wireEventSearch();
  wireHistory();
  wireScheduleEditor();
  wireEditor();
  wireSetup();
  wireSecrets();
  wireDiscardDialog();
  wireRequestEditor();
  $("#result-close").addEventListener("click", () => $("#result-dlg").close());

  window.addEventListener("hashchange", () => showTab(location.hash.slice(1)));
  showTab(location.hash.slice(1) || "rules");
}

// The Discord avatar is also the "Reload UI" button. Falls back to the
// user's initial when there's no avatar or it fails to load.
function renderAvatar(me) {
  const btn = $("#reload-ui");
  const name = me.displayName || me.username || "?";
  const initial = () => h("span.avatar-fallback", { "aria-hidden": "true" }, [...name][0].toUpperCase());
  btn.title = `Signed in as ${name}. Click to reload the UI (clears cached files)`;
  if (!me.avatarUrl) {
    btn.replaceChildren(initial());
    return;
  }
  const img = h("img", { src: me.avatarUrl, alt: "", referrerpolicy: "no-referrer" });
  img.addEventListener("error", () => btn.replaceChildren(initial()), { once: true });
  btn.replaceChildren(img);
}

function renderMe() {
  const me = state.me;
  const box = $("#me");
  renderAvatar(me);
  box.replaceChildren(
    h("span", {}, me.displayName || me.username),
    h("span.badge", {}, me.role),
    h("button.btn.small", {
      onclick: async () => {
        await api("POST", "/auth/logout");
        location.reload();
      },
    }, "Sign out"),
  );
}

function showTab(tab) {
  if (!["rules", "requests", "schedules", "events", "history", "setup", "secrets"].includes(tab) || (tab === "secrets" && !isAdmin())) tab = "rules";
  if (location.hash !== `#${tab}`) history.replaceState(null, "", `#${tab}`);
  $$("#tabs button").forEach((b) => b.classList.toggle("active", b.dataset.tab === tab));
  $$(".view").forEach((v) => v.classList.toggle("hidden", v.id !== `view-${tab}`));
  if (!liveTabs.includes(tab)) stopLive();
  if (tab === "rules") {
    startLive();
    loadRules();
  }
  if (tab === "requests") loadRequests();
  if (tab === "events") {
    startLive();
    loadEvents();
  }
  if (tab === "schedules") loadSchedules();
  if (tab === "history") loadHistory();
  if (tab === "setup") loadSetup();
  if (tab === "secrets") loadSecrets();
}

// ---------- rules ----------
async function loadRules() {
  [state.rules, state.requests, state.schedules] = await Promise.all([api("GET", "/api/rules"), api("GET", "/api/requests"), api("GET", "/api/schedules")]);
  state.rules.forEach((r) => (r.activeRuns || []).forEach(stampRun));
  const list = $("#rules-list");
  if (!state.rules.length) {
    list.replaceChildren(h("div.empty", {}, isAdmin() ? "No rules yet. Create one to react to YourSix events." : "No rules yet."));
    return;
  }
  list.replaceChildren(...state.rules.map(ruleCard));
}

function triggerSummary(t) {
  const parts = [];
  parts.push(t.topics && t.topics.length ? t.topics.join(", ") : "any topic");
  parts.push(t.codes && t.codes.length ? t.codes.join(", ") : "any code");
  for (const f of t.filters || []) {
    parts.push(`${f.field} ${f.op.replace("_", " ")}${["exists", "not_exists"].includes(f.op) ? "" : ` "${f.value}"`}`);
  }
  return "If " + parts.join(" · ");
}

function actionSummary(a) {
  const label = a.name ? `${a.name}: ` : "";
  if (a.type === "http") return `${label}${a.method || "GET"} ${a.url}${a.auth ? ` (${a.auth} auth)` : ""}`;
  if (a.type === "delay") return `${label}wait ${a.delayMillis} ms${a.watch ? `, ${watchSummary(a.watch)}` : ""}`;
  if (a.type === "discord") {
    const n = a.discord?.embeds?.length || 0;
    const extras = [n ? `${n} embed${n === 1 ? "" : "s"}` : "", a.discord?.pingUsers?.length || a.discord?.pingRoles?.length ? "pings" : "", a.discord?.silent ? "silent" : ""].filter(Boolean);
    return `${label}Discord: ${a.message || a.discord?.embeds?.[0]?.title || ""}${extras.length ? ` (${extras.join(", ")})` : ""}`;
  }
  if (a.type === "request") {
    const sr = (state.requests || []).find((r) => r.id === a.requestId);
    const params = Object.entries(a.params || {}).map(([k, v]) => `${k}=${v}`).join(", ");
    return `${label}Saved action: ${sr ? sr.name : `#${a.requestId} (missing)`}${params ? ` (${params})` : ""}`;
  }
  return label + a.type;
}

// watchSummary describes a delay's watch: "watching the rule's trigger:
// restart (up to 30m), then stop".
function watchSummary(w) {
  const t = w.trigger || {};
  const what = w.sameAsTrigger ? "the rule's trigger" : [...(t.topics || []), ...(t.codes || [])].join(", ") || "events";
  const extra = (t.filters || []).length ? ` (${t.filters.length} filter${t.filters.length === 1 ? "" : "s"})` : "";
  const does = [
    w.restart ? `restart (up to ${fmtDuration(Math.round(w.maxMillis / 1000))})` : "",
    w.actions?.length ? `${w.actions.map(actionSummary).join("; ")}${w.everyMatch ? " each time" : ""}` : "",
    w.stop ? "stop" : "",
  ].filter(Boolean);
  const quiet = w.ifNoMatch?.length ? `; if none: ${w.ifNoMatch.map(actionSummary).join("; ")}` : "";
  return `watching ${what}${extra}${does.length ? ": " + does.join(", then ") : ""}${quiet}`;
}

function ruleCard(r) {
  const admin = isAdmin();
  const active = r.activeRuns || [];
  // Mark the rule's own action list with the first active run's position.
  const cur = active[0] && active[0].current;
  const card = h("div.card.rule-card" + (r.enabled ? ".enabled" : ".disabled") + (active.length ? ".is-running" : ""), {},
    h("div.card-head", {},
      h("h3", {}, r.name),
      r.enabled ? h("span.badge.ok", {}, "enabled") : h("span.badge", {}, "disabled"),
      active.length ? h("span.badge.running", {}, active.length > 1 ? `running ×${active.length}` : "running") : null,
      r.skipIfRunning ? h("span.badge", { title: "New events are ignored while a run is in progress" }, "one at a time") : null,
      r.cooldownSeconds ? h("span.badge", { title: `Ignores events for ${r.cooldownSeconds}s after the last run ${r.cooldownFrom === "finish" ? "finished" : "started"}` }, `once per ${fmtDuration(r.cooldownSeconds)}${r.cooldownFrom === "finish" ? " after finish" : ""}`) : null,
      scheduleBadge(r.scheduleId),
      r.onFailure?.length ? h("span.badge", { title: "Runs extra actions when a step fails" }, "handles failures") : null,
      h("span.muted.small", { title: fmtTime(r.lastFiredAt) }, `fired ${ago(r.lastFiredAt)}`),
      admin ? h("button.btn.small", { onclick: () => testRule(r) }, "Test") : null,
      h("button.btn.small", { onclick: () => showHistory("rule", r.id, r.name) }, "History"),
      admin ? h("button.btn.small", { onclick: () => openEditor(r) }, "Edit") : null,
      admin ? h("button.btn.small.danger", { onclick: () => deleteRule(r) }, "Delete") : null,
    ),
    active.length ? h("div.rule-activity", { "aria-live": "polite" }, active.map((run) => runProgress(run))) : null,
    h("p.summary", {}, triggerSummary(r.trigger)),
    r.updatedByName && r.updatedBy !== state.me.id
      ? h("p.muted.small", {}, `Saved by ${r.updatedByName}: runs with their secrets. Saving it yourself makes it yours.`)
      : null,
    h("ol.actions-list", {}, r.actions.map((a, i) => h("li" + (cur ? (i < cur.index ? ".step-done" : i === cur.index ? ".step-current" : "") : ""), {}, actionSummary(a)))),
    r.onFailure?.length ? h("p.summary", {}, "If anything fails: ", r.onFailure.map(actionSummary).join("; ")) : null,
  );
  card.dataset.ruleId = r.id;
  if (admin) clickToEdit(card, `${r.name}: ${r.enabled ? "enabled" : "disabled"}`, () => openEditor(r));
  return card;
}

// clickToEdit makes a whole card open its editor: click anywhere except its
// buttons and links (or when the click finished a text selection), or focus
// it and press Enter/Space.
function clickToEdit(card, label, open) {
  card.classList.add("clickable");
  card.tabIndex = 0;
  card.setAttribute("aria-label", `${label}. Press Enter to edit.`);
  card.title = "Click to edit";
  card.addEventListener("click", (e) => {
    if (e.target.closest("button, a, input, select, textarea, summary")) return;
    if (String(window.getSelection())) return;
    open();
  });
  card.addEventListener("keydown", (e) => {
    if (e.target === card && (e.key === "Enter" || e.key === " ")) {
      e.preventDefault();
      open();
    }
  });
}

async function deleteRule(r) {
  const yes = await confirmDialog({
    title: "Delete this rule?",
    message: [h("strong", {}, r.name), " will be removed and stop reacting to events. Its past runs stay in the event log."],
    confirmLabel: "Delete rule", danger: true,
  });
  if (!yes) return;
  await api("DELETE", `/api/rules/${r.id}`);
  loadRules();
}

// Starts a rule test, then polls it so each step shows up as it finishes.
async function testRule(r) {
  const n = r.actions.length;
  const yes = await confirmDialog({
    title: "Run this rule now?",
    message: ["Testing runs ", h("strong", {}, r.name), `'s ${n} action${n === 1 ? "" : "s"} for real with a sample event, so they will call your devices (move cameras, trigger outputs, send messages…). Its trigger, cooldown and “ignore while running” don't apply.`],
    confirmLabel: "Run test",
  });
  if (!yes) return;
  showResult(`Testing "${r.name}"…`, h("p.muted", {}, "Starting…"));
  let start;
  try {
    start = await api("POST", `/api/rules/${r.id}/test`, {});
  } catch (e) {
    showResult("Test failed", h("p.error", {}, e.message));
    return;
  }
  const note = start.matchesTrigger ? "" : h("span.muted", {}, " (sample event would not match this trigger's filters)");
  await pollRun(`Test: ${r.name}`, start, r.actions.length, note);
  loadRules();
}

// pollRun shows a background run live in the result dialog until it ends.
// The run lives on the server: closing the dialog doesn't stop it, and the
// result stays in the Events tab.
async function pollRun(title, start, total, note = "") {
  const token = (state.testToken = {});
  const sample = h("details", {}, h("summary", {}, "Sample event used"), h("pre", {}, JSON.stringify(start.event, null, 2)));
  const render = (run) => {
    const running = run.status === "running";
    stampRun(run);
    if (!run.totalSteps) run.totalSteps = total;
    showResult(title,
      h("p", {}, "Status: ", h(`span.badge.${run.status}`, {}, run.status), note),
      running ? runProgress(run) : null,
      run.steps.length ? renderSteps(run.steps) : running ? null : h("p.muted", {}, "No steps recorded."),
      running ? h("p.muted.small", {}, "You can close this dialog; the test keeps running and its result appears in the Events tab.") : null,
      sample,
    );
  };

  let run = start.run;
  render(run);
  const dlg = $("#result-dlg");
  while (run.status === "running") {
    await new Promise((res) => setTimeout(res, 1000));
    // Stop polling if the dialog was closed or another test started.
    if (!dlg.open || state.testToken !== token) return;
    try {
      run = await api("GET", `/api/runs/${run.id}`);
    } catch {
      // A blip (e.g. a proxy hiccup) shouldn't end the view; keep trying.
      continue;
    }
    if (state.testToken === token) render(run);
  }
}

function renderSteps(steps) {
  if (!steps || !steps.length) return h("p.muted", {}, "No steps recorded.");
  const firstFailureStep = steps.findIndex((s) => s.onFailure);
  return h("div", {}, steps.map((s, i) => [
    i === firstFailureStep ? h("div.step-group", {}, "If anything fails") : null,
    h("div.step" + (s.skipped ? "" : s.ok ? ".ok" : ".bad"), {},
      h("div", {}, h("strong", {}, `${s.index + 1}. ${s.name || ACTION_LABELS[s.type] || s.type}`), " ",
        s.skipped ? h("span.badge", {}, "skipped") : h(`span.badge.${s.ok ? "ok" : "failed"}`, {}, s.ok ? "ok" : "failed"),
        s.status ? h("span.badge", {}, `HTTP ${s.status}`) : null,
        s.skipped ? null : h("span.muted.small", {}, ` ${s.durationMs || 0} ms`)),
      s.target ? h("div.muted.small", {}, s.target) : null,
      s.error ? h("div.error", {}, s.error) : null,
      s.response ? h("pre", {}, s.response) : null,
      s.watch ? watchDetail(s.watch) : null,
    )]));
}

// watchDetail shows what a watched delay saw, and its actions' results
// grouped by the match that ran them.
function watchDetail(w) {
  const failed = (w.steps || []).filter((s) => !s.ok && !s.skipped).length;
  const parts = [`${w.matches} matching event${w.matches === 1 ? "" : "s"}`,
    w.restarts ? `countdown restarted ${w.restarts}×` : "", w.stopped ? "stopped the rule" : ""].filter(Boolean);
  const groups = [];
  for (const s of w.steps || []) {
    const last = groups[groups.length - 1];
    if (last && last.match === s.match) last.steps.push(s);
    else groups.push({ match: s.match, steps: [s] });
  }
  return h("div.watch-detail", {},
    h("div.muted.small", {}, "While waiting: ", parts.join(" · "), failed ? h("span.error", {}, ` · ${failed} action${failed === 1 ? "" : "s"} failed`) : null),
    groups.map((g) => [h("div.step-group", {}, g.match ? `On match ${g.match}` : "Nothing matched"), renderSteps(g.steps)]),
    w.dropped ? h("p.muted.small", {}, `${w.dropped} more results not kept.`) : null);
}

function showResult(title, ...body) {
  $("#result-title").textContent = title;
  $("#result-body").replaceChildren(...body.filter((b) => b !== null && b !== undefined && b !== ""));
  const dlg = $("#result-dlg");
  if (!dlg.open) dlg.showModal();
}

// ---------- rule editor ----------
const blankRule = () => ({
  name: "",
  enabled: true,
  cooldownSeconds: 0,
  cooldownFrom: "start",
  skipIfRunning: false,
  trigger: { topics: ["device"], codes: [], filters: [] },
  actions: [],
});

const blankAction = (type) => ({
  http: { type: "http", name: "", method: "GET", url: "http://camera.local/axis-cgi/", auth: "digest", username: "root", password: '{{ secret "CAMERA_PASS" }}', headers: {}, body: "", timeoutSeconds: 10, insecureTLS: false, continueOnError: false },
  delay: { type: "delay", name: "", delayMillis: 1000 },
  discord: { type: "discord", name: "", webhookUrl: "", message: "{{ .event.eventDescription }} on {{ .data.deviceName }} ({{ .data.siteName }})" },
  request: { type: "request", name: "", requestId: "", params: {} },
}[type]);

// Only the fields the API accepts.
function cleanRule(r) {
  return {
    name: r.name,
    enabled: r.enabled,
    cooldownSeconds: Number(r.cooldownSeconds) || 0,
    cooldownFrom: r.cooldownFrom || "start",
    skipIfRunning: !!r.skipIfRunning,
    trigger: { topics: r.trigger.topics || [], codes: r.trigger.codes || [], filters: r.trigger.filters || [] },
    ...(r.scheduleId ? { scheduleId: Number(r.scheduleId) } : {}),
    actions: r.actions || [],
    ...(r.onFailure && r.onFailure.length ? { onFailure: r.onFailure } : {}),
  };
}

function wireEditor() {
  guardEditor("rule", $("#editor"), $("#editor-cancel"), editorState);
  $("#add-filter").addEventListener("click", () => addFilterRow({ field: "deviceName", op: "equals", value: "" }));
  $$("[data-add-action]").forEach((b) => b.addEventListener("click", () => { addActionBox(blankAction(b.dataset.addAction)); schedulePreview(); }));
  $$("[data-add-fail]").forEach((b) => b.addEventListener("click", () => { addActionBox(blankAction(b.dataset.addFail), $("#f-onfail")); schedulePreview(); }));
  $("#f-schedule").addEventListener("change", showScheduleInfo);
  wirePreview();
  $("#editor-json-mode").addEventListener("change", (e) => {
    const err = $("#editor-error");
    err.textContent = "";
    try {
      if (e.target.checked) {
        $("#f-json").value = JSON.stringify(readVisual(), null, 2);
      } else {
        fillVisual(cleanRule(JSON.parse($("#f-json").value)));
      }
      $("#editor-visual").classList.toggle("hidden", e.target.checked);
      $("#editor-json").classList.toggle("hidden", !e.target.checked);
    } catch (ex) {
      e.target.checked = !e.target.checked;
      err.textContent = "Invalid JSON: " + ex.message;
    }
  });
  $("#editor-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const err = $("#editor-error");
    err.textContent = "";
    let rule;
    try {
      rule = $("#editor-json-mode").checked ? cleanRule(JSON.parse($("#f-json").value)) : readVisual();
    } catch (ex) {
      err.textContent = "Invalid JSON: " + ex.message;
      return;
    }
    const save = async (expected) => {
      err.textContent = "";
      try {
        if (state.editing && state.editing.id) await api("PUT", `/api/rules/${state.editing.id}`, { ...rule, ...(expected ? { updatedAt: expected } : {}) });
        else await api("POST", "/api/rules", rule);
        $("#editor").close();
        loadRules();
      } catch (ex) {
        if (ex.status === 409 && ex.data?.conflict) {
          showConflict(err, ex, state.editing, {
            overwrite: () => save(ex.data.current.updatedAt),
            reload: () => {
              state.editing = ex.data.current;
              $("#editor-json-mode").checked = false;
              $("#editor-visual").classList.remove("hidden");
              $("#editor-json").classList.add("hidden");
              fillVisual(cleanRule(structuredClone(ex.data.current)));
              guards.rule.snapshot();
              err.replaceChildren();
            },
          });
        } else err.textContent = ex.message;
      }
    };
    // Saving sends the version this editor opened, so the server can refuse
    // if someone else saved in between.
    save(state.editing?.updatedAt);
  });
}

// showConflict explains that someone else saved the item after this editor
// opened it, and offers to compare, overwrite with these edits, or load
// their version (discarding these edits).
function showConflict(errBox, ex, opened, { overwrite, reload }) {
  const compare = () => showResult("What changed since you opened it",
    h("p.muted.small", {}, "Red lines are the version you opened; green lines are what's saved now. Your unsaved edits aren't shown."),
    ...diffView(pretty(contentOf(opened)), pretty(contentOf(ex.data.current)), false));
  errBox.replaceChildren(h("div.conflict", {},
    h("p", {}, h("strong", {}, "Not saved. "),
      `${ex.data.current.updatedByName || "Someone else"} saved a newer version `,
      h("span", { title: fmtTime(ex.data.current.updatedAt) }, ago(ex.data.current.updatedAt)),
      ", after you opened this editor."),
    h("div.row", {},
      h("button.btn.small", { type: "button", onclick: compare }, "Show their changes"),
      h("button.btn.small.danger", { type: "button", onclick: overwrite }, "Overwrite with mine"),
      h("button.btn.small", { type: "button", onclick: reload }, "Load their version"))));
}

async function openEditor(rule) {
  state.editing = rule;
  // Fresh lists for the "Saved action" and schedule pickers.
  [state.requests, state.schedules] = await Promise.all([api("GET", "/api/requests"), api("GET", "/api/schedules")]);
  $("#editor-title").textContent = rule ? `Edit: ${rule.name}` : "New rule";
  $("#editor-error").textContent = "";
  $("#editor-json-mode").checked = false;
  $("#editor-visual").classList.remove("hidden");
  $("#editor-json").classList.add("hidden");
  fillVisual(cleanRule(rule ? structuredClone(rule) : blankRule()));
  $("#editor").showModal();
  guards.rule.snapshot();
}

function fillVisual(r) {
  $("#f-name").value = r.name;
  $("#f-enabled").checked = r.enabled;
  $("#f-cooldown").value = r.cooldownSeconds;
  $("#f-cooldown-from").value = r.cooldownFrom || "start";
  $("#f-skip-running").checked = !!r.skipIfRunning;

  state.codePicker = pillPicker($("#f-codes"), {
    placeholder: "Add an event code…", empty: "Any event code", warnUnknown: true,
    options: () => {
      const topics = selectedTopics();
      return (topics.length ? topics : state.catalog.topics).map((t) => [t, state.catalog.codes[t] || []]);
    },
  });
  state.topicPicker = pillPicker($("#f-topics"), {
    placeholder: "Add a topic…", empty: "Any topic",
    options: () => [[null, state.catalog.topics]],
    onchange: () => state.codePicker.refresh(),
  });
  state.topicPicker.set(r.trigger.topics);
  state.codePicker.set(r.trigger.codes);

  $("#f-filters").replaceChildren();
  r.trigger.filters.forEach((flt) => addFilterRow(flt));
  $("#f-actions").replaceChildren();
  r.actions.forEach((a) => addActionBox(a));
  $("#f-onfail").replaceChildren();
  (r.onFailure || []).forEach((a) => addActionBox(a, $("#f-onfail")));

  const sel = $("#f-schedule");
  const known = state.schedules || [];
  sel.replaceChildren(h("option", { value: "" }, "Any time"),
    ...known.map((s) => h("option", { value: s.id }, s.name)),
    ...(r.scheduleId && !known.some((s) => s.id === r.scheduleId) ? [h("option", { value: r.scheduleId }, `#${r.scheduleId} (missing)`)] : []));
  sel.value = r.scheduleId ? String(r.scheduleId) : "";
  showScheduleInfo();
  schedulePreview();
}

// showScheduleInfo describes the schedule picked in the rule editor.
function showScheduleInfo() {
  const s = (state.schedules || []).find((x) => String(x.id) === $("#f-schedule").value);
  const info = $("#f-schedule-info");
  if (!s) {
    info.textContent = (state.schedules || []).length ? "" : "Create schedules on the Schedules tab.";
    return;
  }
  const on = scheduleActive(s, new Date());
  info.replaceChildren(`${scheduleText(s)} · ${tzLabel(s.timezone)} · `, h(on ? "span.on-now" : "span.off-now", {}, on ? "open now" : "closed now"));
}

const selectedTopics = () => state.topicPicker ? state.topicPicker.values() : [];
const readCodes = () => state.codePicker ? state.codePicker.values() : [];

// pillPicker turns container into a dropdown that adds removable pills.
// options() returns [[group label or null, values]] and is re-read on every
// refresh, so it can depend on another picker. Pills for values that aren't
// among the options (e.g. entered as JSON) are kept; with warnUnknown they're
// flagged, since the rule won't match them under the chosen topics.
function pillPicker(container, { placeholder, empty, options, onchange, warnUnknown = false }) {
  const pills = h("div.pills", { "data-empty": empty });
  const sel = h("select.picker-add", { "aria-label": placeholder });
  const values = () => $$(".pill", pills).map((p) => p.dataset.value);
  const refresh = () => {
    const have = new Set(values());
    const known = new Set();
    const groups = options();
    sel.replaceChildren(h("option", { value: "" }, placeholder), ...groups.map(([label, list]) => {
      const opts = list.map((v) => {
        known.add(v);
        return h("option", { value: v, disabled: have.has(v) }, v);
      });
      return label && groups.length > 1 ? h("optgroup", { label }, opts) : opts;
    }).flat());
    sel.value = "";
    pills.classList.toggle("is-empty", !have.size);
    for (const p of $$(".pill", pills)) {
      const v = p.dataset.value;
      const unknown = warnUnknown && !known.has(v) && !known.has(v.split(":").pop());
      p.classList.toggle("unknown", unknown);
      p.title = unknown ? `“${v}” isn't a code of the chosen topics` : "";
    }
  };
  const pill = (v) => {
    const el = h("span.pill", {}, h("span", {}, v),
      h("button", { type: "button", "aria-label": `Remove ${v}`, onclick: () => { el.remove(); refresh(); onchange?.(); sel.focus(); } }, "✕"));
    el.dataset.value = v;
    return el;
  };
  sel.addEventListener("change", () => {
    const v = sel.value;
    if (v && !values().includes(v)) pills.append(pill(v));
    refresh();
    onchange?.();
  });
  container.replaceChildren(pills, sel);
  const set = (list) => {
    pills.replaceChildren(...[...new Set(list)].map(pill));
    refresh();
    onchange?.();
  };
  refresh();
  return { values, refresh, set };
}

function addFilterRow(f, container = $("#f-filters")) {
  const row = $("#tpl-filter").content.firstElementChild.cloneNode(true);
  $(".ff-field", row).value = f.field || "";
  $(".ff-op", row).value = f.op || "equals";
  $(".ff-value", row).value = f.value || "";
  $(".ff-del", row).addEventListener("click", () => row.remove());
  container.append(row);
}

const readFilters = (container) => $$(":scope > .filter-row", container).map((row) => ({
  field: $(".ff-field", row).value.trim(),
  op: $(".ff-op", row).value,
  value: $(".ff-value", row).value,
})).filter((f) => f.field);

const field = (label, input) => [h("span.muted", {}, label), input];

const ACTION_LABELS = { http: "HTTP request", delay: "Delay", discord: "Discord message", request: "Saved action" };

// addActionBox renders an action's form into container (the rule editor's
// action list by default). With fixed, it's the single action in the saved
// action editor: no header, reorder/remove buttons, name or continue-on-error.
function addActionBox(a, container = $("#f-actions"), { fixed = false } = {}) {
  const box = h("div.action-box" + (fixed ? ".fixed" : ""), {});
  box.dataset.type = a.type;
  const head = fixed ? null : h("div.action-head", {},
    h("strong", {}, ACTION_LABELS[a.type] || a.type),
    h("button.btn.small", { type: "button", "aria-label": "Move up", onclick: () => box.previousElementSibling && box.parentNode.insertBefore(box, box.previousElementSibling) }, "↑"),
    h("button.btn.small", { type: "button", "aria-label": "Move down", onclick: () => box.nextElementSibling && box.parentNode.insertBefore(box.nextElementSibling, box) }, "↓"),
    h("button.btn.small.danger", { type: "button", "aria-label": "Remove action", onclick: () => box.remove() }, "✕"),
  );
  const inp = (k, attrs = {}) => h("input", { "data-k": k, value: a[k] ?? "", ...attrs });
  let grid;
  if (a.type === "http") {
    const method = h("select", { "data-k": "method" }, ["GET", "POST", "PUT", "PATCH", "DELETE"].map((m) => h("option", { value: m }, m)));
    method.value = a.method || "GET";
    const auth = h("select", { "data-k": "auth" }, [["", "none"], ["basic", "basic"], ["digest", "digest (Axis default)"]].map(([v, l]) => h("option", { value: v }, l)));
    auth.value = a.auth || "";
    const headers = h("textarea", { "data-k": "headers", rows: 2, placeholder: "Header-Name: value (one per line)" });
    headers.value = Object.entries(a.headers || {}).map(([k, v]) => `${k}: ${v}`).join("\n");
    const body = h("textarea", { "data-k": "body", rows: 3, placeholder: "Optional request body (templated)" });
    body.value = a.body || "";
    grid = h("div.action-grid", {},
      fixed ? null : field("Name", inp("name", { placeholder: "optional label" })),
      field("Method", method),
      field("URL", inp("url", { placeholder: "http://192.168.1.50/axis-cgi/io/port.cgi?action=2:/" })),
      field("Auth", auth),
      field("Username", inp("username")),
      field("Password", inp("password", { placeholder: '{{ secret "CAMERA_PASS" }}' })),
      field("Headers", headers),
      field("Body", body),
      field("Timeout (s)", inp("timeoutSeconds", { type: "number", min: 0, max: 120 })),
      field("Expect status", inp("expectStatus", { value: (a.expectStatus || []).join(","), placeholder: "default: any 2xx" })),
      field("Options", h("div.row", {},
        h("label.inline", {}, h("input", { type: "checkbox", "data-k": "insecureTLS", checked: !!a.insecureTLS }), "Allow self-signed TLS"),
        fixed ? null : h("label.inline", {}, h("input", { type: "checkbox", "data-k": "continueOnError", checked: !!a.continueOnError }), "Continue on error"))),
    );
  } else if (a.type === "delay") {
    grid = h("div.action-grid", {},
      fixed ? null : field("Name", inp("name", { placeholder: "optional label" })),
      field("Delay (ms)", inp("delayMillis", { type: "number", min: 1, max: 600000 })));
    // Only a rule's own delays can watch for events.
    if (!fixed && container.id === "f-actions") box.append(head, grid, watchSection(a, box));
  } else if (a.type === "discord") {
    grid = discordActionGrid(a, inp, fixed);
  } else if (a.type === "request") {
    grid = requestActionGrid(a, inp);
  } else {
    grid = h("pre", {}, JSON.stringify(a, null, 2));
    box.dataset.raw = JSON.stringify(a);
  }
  if (!box.childElementCount) box.append(...[head, grid].filter(Boolean));
  container.append(box);
  return box;
}

// watchSection is a delay's "While waiting, watch for events" settings
// (a.watch). It sets box._readWatch, which readAction uses.
function watchSection(a, box) {
  const w = a.watch || { sameAsTrigger: true, trigger: { filters: [] }, restart: true, maxMillis: 1800000 };
  const t = w.trigger || {};
  const on = h("input", { type: "checkbox", class: "w-on", checked: !!a.watch });
  const same = h("input", { type: "checkbox", class: "w-same", checked: !!w.sameAsTrigger });
  const topics = h("div.picker"), codes = h("div.picker");
  let topicPicker = null; // set below; the code picker's options read it
  const codePicker = pillPicker(codes, {
    placeholder: "Add an event code…", empty: "Any event code", warnUnknown: true,
    options: () => {
      const picked = topicPicker ? topicPicker.values() : [];
      return (picked.length ? picked : state.catalog.topics).map((tp) => [tp, state.catalog.codes[tp] || []]);
    },
  });
  topicPicker = pillPicker(topics, {
    placeholder: "Add a topic…", empty: "Any topic",
    options: () => [[null, state.catalog.topics]],
    onchange: () => codePicker.refresh(),
  });
  topicPicker.set(t.topics || []);
  codePicker.set(t.codes || []);
  const own = h("div.w-own", {}, field("Topics", topics), field("Event codes", codes));
  const filters = h("div.w-filters");
  (t.filters || []).forEach((f) => addFilterRow(f, filters));
  const restart = h("input", { type: "checkbox", class: "w-restart", checked: !!w.restart });
  const maxSecs = h("input.narrow", { type: "number", min: 1, max: 3600, value: Math.round((w.maxMillis || 1800000) / 1000), "aria-label": "Longest wait in seconds" });
  const runActs = h("input", { type: "checkbox", class: "w-run", checked: !!w.actions?.length });
  const every = h("input", { type: "checkbox", class: "w-every", checked: !!w.everyMatch });
  const stop = h("input", { type: "checkbox", class: "w-stop", checked: !!w.stop });
  const acts = h("div.w-actions"), quiet = h("div.w-nomatch");
  (w.actions || []).forEach((x) => addActionBox(x, acts));
  (w.ifNoMatch || []).forEach((x) => addActionBox(x, quiet));
  const addButtons = (list) => h("div.row", {}, [["discord", "+ Discord message"], ["http", "+ HTTP request"], ["request", "+ Saved action"]].map(([type, label]) =>
    h("button.btn.small", { type: "button", onclick: () => { addActionBox(blankAction(type), list); schedulePreview(); } }, label)));
  const actsBlock = h("div.w-sub", {}, acts, addButtons(acts),
    h("label.inline", {}, every, "Run them on every match, not just the first"));

  const body = h("div.watch-body", {},
    h("label.inline", {}, same, "Same events as this rule's trigger"),
    own,
    h("div.w-label", {}, "Filters ", h("span.muted.small", {}, "Values can use the event that started the run, e.g. deviceName equals ", h("code", {}, "{{ .data.deviceName }}"), " for the same camera.")),
    filters,
    h("div.row", {}, h("button.btn.small", { type: "button", onclick: () => addFilterRow({ field: "deviceName", op: "equals", value: "{{ .data.deviceName }}" }, filters) }, "+ Filter")),
    h("div.w-label", {}, "When one arrives"),
    h("label.inline", {}, restart, "Restart the countdown, up to ", maxSecs, " seconds in total"),
    h("label.inline", {}, runActs, "Run actions"),
    actsBlock,
    h("label.inline", {}, stop, "Stop the rule (skip its remaining steps)"),
    h("div.w-label", {}, "If nothing arrives ", h("span.muted.small", {}, "(optional) Runs when the wait ends without a match.")),
    quiet, addButtons(quiet),
    h("p.muted.small", {}, "Matching events are still checked against every rule as usual. Actions here see ", h("code", {}, "{{ .watch.matches }}"), ", ",
      h("code", {}, "{{ .watch.data.deviceName }}"), " (the matched event) and more; so do the rule's later steps."));
  const sync = () => {
    body.classList.toggle("hidden", !on.checked);
    own.classList.toggle("hidden", same.checked);
    maxSecs.disabled = !restart.checked;
    actsBlock.classList.toggle("hidden", !runActs.checked);
  };
  for (const el of [on, same, restart, runActs]) el.addEventListener("change", sync);
  sync();

  box._readWatch = () => {
    if (!on.checked) return null;
    // Same shape the server saves: the rule's trigger leaves these empty.
    const out = { trigger: same.checked ? { topics: [], codes: [] } : { topics: topicPicker.values(), codes: codePicker.values() } };
    out.trigger.filters = readFilters(filters);
    if (same.checked) out.sameAsTrigger = true;
    if (restart.checked) Object.assign(out, { restart: true, maxMillis: (Number(maxSecs.value) || 0) * 1000 });
    if (runActs.checked) {
      const list = $$(":scope > .action-box", acts).map(readAction);
      if (list.length) out.actions = list;
      if (list.length && every.checked) out.everyMatch = true;
    }
    if (stop.checked) out.stop = true;
    const none = $$(":scope > .action-box", quiet).map(readAction);
    if (none.length) out.ifNoMatch = none;
    return out;
  };
  return h("div.watch-box", {}, h("label.inline.w-toggle", {}, on, h("strong", {}, "While waiting, watch for events")), body);
}

// discordActionGrid is the form for a "discord" action. Its extra webhook
// properties live under a.discord (inputs tagged data-dk); embeds are
// .embed-box elements (data-ek) with .embed-field rows (data-fk).
function discordActionGrid(a, inp, fixed = false) {
  const d = a.discord || {};
  const msg = h("textarea", { "data-k": "message", rows: 2, placeholder: "Optional if you add an embed" });
  msg.value = a.message || "";
  const dinp = (k, attrs = {}) => h("input", { "data-dk": k, value: d[k] ?? "", ...attrs });
  const dchk = (k, label, title) => h("label.inline", { title }, h("input", { type: "checkbox", "data-dk": k, checked: !!d[k] }), label);

  const embeds = h("div.embeds", {});
  for (const e of d.embeds || []) embeds.append(embedBox(e));
  const addEmbed = h("button.btn.small", { type: "button", onclick: () => embeds.append(embedBox({})) }, "+ Embed");

  const more = h("details.discord-more", { open: !!(d.username || d.avatarUrl || d.threadId || d.threadName) },
    h("summary", {}, "Sender & thread"),
    h("div.action-grid", {},
      field("Username", dinp("username", { placeholder: "blank = the webhook's name" })),
      field("Avatar URL", dinp("avatarUrl", { placeholder: "https://… (blank = the webhook's avatar)" })),
      field("Thread ID", dinp("threadId", { placeholder: "post into an existing thread" })),
      field("Thread name", dinp("threadName", { placeholder: "forum channels: start a new post with this title" }))));

  return h("div.action-grid", {},
    fixed ? null : field("Name", inp("name", { placeholder: "optional label" })),
    field("Webhook URL", inp("webhookUrl", { placeholder: "blank = Y6IFTTT_DISCORD_WEBHOOK_URL" })),
    field("Message", msg),
    field("Ping users", dinp("pingUsers", { value: (d.pingUsers || []).join(", "), placeholder: "user IDs, comma-separated (right-click › Copy User ID)" })),
    field("Ping roles", dinp("pingRoles", { value: (d.pingRoles || []).join(", "), placeholder: "role IDs, comma-separated" })),
    field("Options", h("div.row", {},
      dchk("pingEveryone", "Allow @everyone / @here", "Otherwise @everyone and @here in the message don't ping"),
      dchk("silent", "Silent", "Post without a push or desktop notification"),
      dchk("tts", "Text-to-speech"),
      fixed ? null : h("label.inline", {}, h("input", { type: "checkbox", "data-k": "continueOnError", checked: !!a.continueOnError }), "Continue on error"))),
    h("div.full", {}, more),
    field("Embeds", h("div", {}, embeds, addEmbed)),
  );
}

const EVENT_TIME = "{{ .event.eventOccurred }}";

function embedBox(e) {
  const box = h("div.embed-box", {});
  const einp = (k, attrs = {}) => h("input", { "data-ek": k, value: e[k] ?? "", ...attrs });
  const desc = h("textarea", { "data-ek": "description", rows: 2 });
  desc.value = e.description || "";

  // A text field (so templates and blanks work) with a picker beside it.
  const color = einp("color", { placeholder: "#5865F2", class: "color-text" });
  const picker = h("input", { type: "color", value: /^#?[0-9a-f]{6}$/i.test(e.color || "") ? "#" + e.color.replace("#", "") : "#5865f2" });
  const syncBar = () => { box.style.borderLeftColor = /^#?[0-9a-f]{6}$/i.test(color.value.trim()) ? "#" + color.value.trim().replace("#", "") : ""; };
  picker.addEventListener("input", () => { color.value = picker.value; syncBar(); });
  color.addEventListener("input", () => { if (/^#?[0-9a-f]{6}$/i.test(color.value.trim())) picker.value = "#" + color.value.trim().replace("#", ""); syncBar(); });
  syncBar();

  const ts = einp("timestamp", { placeholder: EVENT_TIME });
  const fields = h("div.embed-fields", {});
  for (const f of e.fields || []) fields.append(embedFieldRow(f));

  box.append(
    h("div.action-head", {}, h("strong", {}, "Embed"), h("button.btn.small.danger", { type: "button", onclick: () => box.remove() }, "✕")),
    h("div.action-grid", {},
      field("Title", einp("title")),
      field("Title link", einp("url", { placeholder: "https://…" })),
      field("Description", desc),
      field("Color", h("div.row", {}, picker, color)),
      field("Timestamp", h("div.row", {}, ts, h("button.btn.small", { type: "button", onclick: () => { ts.value = EVENT_TIME; } }, "Event time"))),
      field("Fields", h("div", {}, fields, h("button.btn.small", { type: "button", onclick: () => fields.append(embedFieldRow({})) }, "+ Field"))),
    ),
    h("details.discord-more", { open: !!(e.authorName || e.footerText || e.thumbnailUrl || e.imageUrl) },
      h("summary", {}, "Author, footer & images"),
      h("div.action-grid", {},
        field("Author", einp("authorName", { placeholder: "name" })),
        field("Author link", einp("authorUrl", { placeholder: "https://…" })),
        field("Author icon", einp("authorIconUrl", { placeholder: "image URL" })),
        field("Footer", einp("footerText")),
        field("Footer icon", einp("footerIconUrl", { placeholder: "image URL" })),
        field("Thumbnail", einp("thumbnailUrl", { placeholder: "image URL (small, top right)" })),
        field("Image", einp("imageUrl", { placeholder: "image URL (large, bottom)" })))),
  );
  return box;
}

function embedFieldRow(f) {
  const row = h("div.embed-field", {},
    h("input", { "data-fk": "name", value: f.name || "", placeholder: "Name" }),
    h("input", { "data-fk": "value", value: f.value || "", placeholder: "Value" }),
    h("label.inline", {}, h("input", { type: "checkbox", "data-fk": "inline", checked: !!f.inline }), "Inline"),
    h("button.btn.small.danger", { type: "button", onclick: () => row.remove() }, "✕"));
  return row;
}

// readDiscord collects a discord action's options, or undefined if none are set.
function readDiscord(box) {
  const d = {};
  const read = (el, k, obj) => {
    if (el.type === "checkbox") { if (el.checked) obj[k] = true; } else if (el.value.trim() !== "") obj[k] = el.value;
  };
  for (const el of $$("[data-dk]", box)) {
    const k = el.dataset.dk;
    if (k === "pingUsers" || k === "pingRoles") {
      const ids = el.value.split(/[\s,]+/).filter(Boolean);
      if (ids.length) d[k] = ids;
    } else read(el, k, d);
  }
  const embeds = $$(".embed-box", box).map((eb) => {
    const e = {};
    for (const el of $$("[data-ek]", eb)) read(el, el.dataset.ek, e);
    const fields = $$(".embed-field", eb).map((row) => {
      const f = {};
      for (const el of $$("[data-fk]", row)) read(el, el.dataset.fk, f);
      return f;
    }).filter((f) => f.name || f.value);
    if (fields.length) e.fields = fields;
    return e;
  });
  if (embeds.length) d.embeds = embeds;
  return Object.keys(d).length ? d : undefined;
}

// savedActionLine is a one-line description of what a saved action does.
const savedActionLine = (sr) => actionSummary({ ...sr.request, type: sr.request.type || "http", name: "" });

// requestActionGrid is the form for a "request" action: pick a saved action,
// then fill in its parameters (inputs rebuilt when the choice changes).
function requestActionGrid(a, inp) {
  const known = state.requests || [];
  const sel = h("select", { "data-k": "requestId" },
    h("option", { value: "" }, known.length ? "Choose a saved action…" : "No saved actions yet (create one on the Saved actions tab)"),
    known.map((r) => h("option", { value: r.id }, `${r.name} · ${ACTION_LABELS[r.request.type || "http"]}`)));
  if (a.requestId && !known.some((r) => r.id === Number(a.requestId))) {
    sel.append(h("option", { value: a.requestId }, `#${a.requestId} (missing)`));
  }
  sel.value = a.requestId ? String(a.requestId) : "";

  const summary = h("div.muted.small.request-summary", {});
  const paramsBox = h("div.params-box", {});
  let values = { ...(a.params || {}) };
  const render = () => {
    // Keep what was typed when switching back and forth.
    for (const el of $$("[data-param]", paramsBox)) values[el.dataset.param] = el.value;
    const sr = known.find((r) => r.id === Number(sel.value));
    summary.textContent = sr ? `${savedActionLine(sr)}${sr.description ? ` · ${sr.description}` : ""}` : "";
    if (!sr) {
      paramsBox.replaceChildren();
      return;
    }
    paramsBox.replaceChildren(...(sr.params.length
      ? sr.params.map((p) => h("label.param-input", {},
        h("span", {}, p.name, p.default ? "" : h("span.error", { title: "required" }, " *")),
        h("input", { "data-param": p.name, value: values[p.name] ?? "", placeholder: p.default ? `default: ${p.default}` : (p.description || "required") }),
        p.description && p.default ? h("span.muted.small", {}, p.description) : null))
      : [h("span.muted.small", {}, "This saved action has no parameters.")]));
  };
  sel.addEventListener("change", render);
  render();

  return h("div.action-grid", {},
    field("Name", inp("name", { placeholder: "optional label (defaults to the saved action's name)" })),
    field("Saved action", h("div", {}, sel, summary)),
    field("Parameters", paramsBox),
    field("Options", h("label.inline", {}, h("input", { type: "checkbox", "data-k": "continueOnError", checked: !!a.continueOnError }), "Continue on error")),
  );
}

function readAction(box) {
  if (box.dataset.raw) return JSON.parse(box.dataset.raw);
  const a = { type: box.dataset.type };
  // A delay's watch holds action boxes of its own; their fields aren't ours.
  for (const el of $$("[data-k]", box).filter((el) => el.closest(".action-box") === box)) {
    const k = el.dataset.k;
    if (el.type === "checkbox") {
      if (el.checked) a[k] = true;
    } else if (k === "headers") {
      const hdrs = {};
      for (const line of el.value.split("\n")) {
        const i = line.indexOf(":");
        if (i > 0) hdrs[line.slice(0, i).trim()] = line.slice(i + 1).trim();
      }
      if (Object.keys(hdrs).length) a.headers = hdrs;
    } else if (k === "expectStatus") {
      const codes = el.value.split(",").map((s) => parseInt(s, 10)).filter((n) => n > 0);
      if (codes.length) a.expectStatus = codes;
    } else if (k === "requestId") {
      if (el.value !== "") a.requestId = Number(el.value);
    } else if (["timeoutSeconds", "delayMillis"].includes(k)) {
      if (el.value !== "") a[k] = Number(el.value);
    } else if (el.value !== "") {
      a[k] = el.value;
    }
  }
  if (a.type === "request") {
    const params = {};
    for (const el of $$("[data-param]", box)) if (el.value !== "") params[el.dataset.param] = el.value;
    if (Object.keys(params).length) a.params = params;
  }
  if (a.type === "discord") {
    const d = readDiscord(box);
    if (d) a.discord = d;
  }
  if (a.type === "delay" && box._readWatch) {
    const w = box._readWatch();
    if (w) a.watch = w;
  }
  return a;
}

function readVisual() {
  return {
    name: $("#f-name").value.trim(),
    enabled: $("#f-enabled").checked,
    cooldownSeconds: Number($("#f-cooldown").value) || 0,
    cooldownFrom: $("#f-cooldown-from").value,
    skipIfRunning: $("#f-skip-running").checked,
    trigger: {
      topics: selectedTopics(),
      codes: readCodes(),
      filters: readFilters($("#f-filters")),
    },
    ...($("#f-schedule").value ? { scheduleId: Number($("#f-schedule").value) } : {}),
    actions: $$("#f-actions > .action-box").map(readAction),
    ...($$("#f-onfail > .action-box").length ? { onFailure: $$("#f-onfail > .action-box").map(readAction) } : {}),
  };
}

// ---------- template preview ----------
// While "Preview templates" is ticked, the editor's actions are rendered by
// the server (POST /api/preview) against a sample event for the trigger, and
// each action box shows what its URL, body, message, … would be. Secrets
// show as ‹secret NAME›.
let previewTimer, previewSeq = 0;
const PREVIEW_KEY = "y6.previewTemplates";

function wirePreview() {
  const box = $("#f-preview-on");
  try { box.checked = localStorage.getItem(PREVIEW_KEY) === "1"; } catch { /* storage blocked */ }
  box.addEventListener("change", () => {
    try { localStorage.setItem(PREVIEW_KEY, box.checked ? "1" : "0"); } catch { /* storage blocked */ }
    schedulePreview();
  });
  // Any edit in the form refreshes the preview shortly after typing stops.
  for (const type of ["input", "change"]) {
    $("#editor-visual").addEventListener(type, (e) => { if (e.target !== box) schedulePreview(); });
  }
  $("#editor-visual").addEventListener("click", (e) => { if (e.target.closest(".action-head button, .ff-del, .pill button")) schedulePreview(); });
}

function schedulePreview() {
  clearTimeout(previewTimer);
  if (!$("#f-preview-on").checked) {
    $$("#editor .preview").forEach((p) => p.remove());
    $("#preview-note").classList.add("hidden");
    return;
  }
  previewTimer = setTimeout(runPreview, 400);
}

async function runPreview() {
  if (!$("#editor").open || !$("#f-preview-on").checked) return;
  const seq = ++previewSeq;
  let res;
  try {
    res = await api("POST", "/api/preview", { rule: readVisual() });
  } catch (e) {
    if (seq !== previewSeq) return;
    $("#preview-note").textContent = `Preview unavailable: ${e.message}`;
    $("#preview-note").classList.remove("hidden");
    return;
  }
  if (seq !== previewSeq) return;
  const ev = res.event;
  $("#preview-note").replaceChildren("Previewing with a sample ", h("code", {}, `${ev.eventTopic}:${ev.eventCode}`), " event",
    res.matchesTrigger ? "." : h("span.error", {}, " (it doesn't pass this rule's filters, so values may differ from real events)."));
  $("#preview-note").classList.remove("hidden");
  const show = (boxes, results) => boxes.forEach((box, i) => {
    $(":scope > .preview", box)?.remove();
    const fields = results?.[i] || [];
    if (!fields.length) return;
    box.append(h("div.preview", {}, h("div.preview-title", {}, "Preview"),
      fields.map((f) => h("div.pv-row", {},
        h("span.pv-field", {}, f.field),
        f.error ? h("span.pv-value.error", {}, f.error) : h("code.pv-value", {}, f.value || "(empty)")))));
  });
  show($$("#f-actions > .action-box"), res.actions);
  show($$("#f-onfail > .action-box"), res.onFailure);
  $$("#f-actions > .action-box").forEach((box, i) => {
    const w = res.watches?.[i] || {};
    show($$(".w-actions > .action-box", box), w.actions);
    show($$(".w-nomatch > .action-box", box), w.ifNoMatch);
  });
}

// ---------- events ----------
// The list loads once, then stays current through the server's live stream
// (GET /api/events/stream): each pushed event replaces its own row, so
// expanded rows stay open and nothing flickers.
const emptyEventsRow = (text) => h("tr", {}, h("td.empty", { colspan: 5 }, text));

// The search box and outcome filter. While either is set, the server does
// the matching; new pushed events trigger a refresh rather than being shown
// as-is, since only the server can tell whether they match.
const eventFilter = () => ({ q: $("#events-q").value.trim(), outcome: $("#events-outcome").value, from: $("#events-from").value, to: $("#events-to").value });
const filteringEvents = () => { const f = eventFilter(); return !!(f.q || f.outcome || f.from || f.to); };
let eventsSeq = 0;
let eventsRefreshTimer;

function wireEventSearch() {
  let timer;
  const run = () => {
    clearTimeout(timer);
    state.events = [];
    loadEvents();
  };
  $("#events-q").addEventListener("input", () => { clearTimeout(timer); timer = setTimeout(run, 300); });
  for (const id of ["#events-outcome", "#events-from", "#events-to"]) $(id).addEventListener("change", run);
  $("#events-search").addEventListener("submit", (e) => { e.preventDefault(); run(); });
}

async function loadEvents(older) {
  const body = $("#events-body");
  if (!older && !state.events.length) body.replaceChildren(emptyEventsRow(filteringEvents() ? "Searching…" : "Loading events…"));
  const before = older && state.events.length ? state.events[state.events.length - 1].id : 0;
  const f = eventFilter();
  const qs = new URLSearchParams({ limit: 50 });
  if (before) qs.set("before", before);
  if (f.q) qs.set("q", f.q);
  if (f.outcome) qs.set("outcome", f.outcome);
  // Dates are local days: from the start of "from" to the end of "to".
  if (f.from) qs.set("since", new Date(`${f.from}T00:00`).toISOString());
  if (f.to) {
    const end = new Date(`${f.to}T00:00`);
    end.setDate(end.getDate() + 1);
    qs.set("until", end.toISOString());
  }
  // Drop responses to searches that have since been replaced.
  const seq = ++eventsSeq;
  let page;
  try {
    page = await api("GET", `/api/events?${qs}`);
    page.forEach(stampEvent);
  } catch (e) {
    if (seq === eventsSeq && !state.events.length) body.replaceChildren(emptyEventsRow(`Couldn't load events: ${e.message}`));
    return;
  }
  if (seq !== eventsSeq) return;
  if (older) {
    state.events = state.events.concat(page.filter((e) => !state.events.some((x) => x.id === e.id)));
  } else {
    // Keep anything pushed while this page was loading.
    const pushed = state.events.filter((e) => !page.some((x) => x.id === e.id) && (!page.length || e.id > page[0].id));
    state.events = pushed.concat(page);
  }
  renderEvents();
  $("#events-more").classList.toggle("hidden", page.length < 50);
}

// renderEvents redraws the whole table, reusing open detail rows as they are.
function renderEvents() {
  const body = $("#events-body");
  const details = new Map($$("#events-body tr.detail").map((r) => [r.dataset.id, r]));
  if (!state.events.length) {
    body.replaceChildren(emptyEventsRow(filteringEvents()
      ? "No events match this search."
      : "No events yet. Point a YourSix webhook at this service or simulate one from Setup."));
    return;
  }
  body.replaceChildren(...state.events.flatMap((e) => {
    const d = details.get(String(e.id));
    return d ? [eventRow(e), d] : [eventRow(e)];
  }));
}

function eventRow(e) {
  const tr = h("tr.ev", {},
    h("td", { title: fmtTime(e.receivedAt), "data-time": e.receivedAt }, ago(e.receivedAt)),
    h("td", {}, h("code", {}, `${e.topic}:${e.code}`), " ", h("span.muted", {}, e.description), e.test ? h("span.badge.test", {}, "test") : null),
    h("td.muted.small", {}, e.sourceIp),
    h("td", {}, e.runs.length
      ? e.runs.map((r) => r.status === "running"
        ? h("span.badge.running.badge-live", {}, r.ruleName, " · ", runProgress(r, { compact: true }))
        : h(`span.badge.${r.status}`, { title: r.status }, r.ruleName))
      : h("span.muted.small", {}, "no match")),
    h("td.video", {}, matrixLink(e, "Matrix ↗")),
  );
  tr.dataset.id = e.id;
  tr.addEventListener("click", () => toggleEvent(tr, e.id));
  return tr;
}

// upsertEvent applies one pushed event: replace its row in place, or add it
// at the top if it's new. Older events outside the loaded page are ignored.
function upsertEvent(ev) {
  const body = $("#events-body");
  const i = state.events.findIndex((e) => e.id === ev.id);
  if (i >= 0) {
    state.events[i] = ev;
    const old = $(`#events-body tr.ev[data-id="${ev.id}"]`);
    if (old) old.replaceWith(eventRow(ev));
    const detail = $(`#events-body tr.detail[data-id="${ev.id}"]`);
    if (detail) fillEventDetail(detail, ev.id); // quietly, no "Loading…"
    return;
  }
  if (state.events.length && ev.id < state.events[0].id) return;
  if (filteringEvents()) {
    // Let the server decide if it matches; batch bursts into one refresh.
    clearTimeout(eventsRefreshTimer);
    eventsRefreshTimer = setTimeout(() => loadEvents(), 800);
    return;
  }
  state.events.unshift(ev);
  if (state.events.length === 1) {
    renderEvents();
  } else {
    body.prepend(eventRow(ev));
  }
  const row = $(`#events-body tr.ev[data-id="${ev.id}"]`);
  if (row) row.classList.add("fresh");
}

// matrixLink opens the event in the YourSix Matrix (its camera or alarm group,
// playing back from when it happened) in a new tab.
function matrixLink(e, label) {
  if (!e.matrixUrl) return null;
  return h("a.btn.small.matrix-link", {
    href: e.matrixUrl, target: "_blank", rel: "noopener noreferrer",
    title: "Open this event's video in the YourSix Matrix, at the time it happened",
    onclick: (ev) => ev.stopPropagation(), // don't also expand the row
  }, label);
}

function toggleEvent(tr, id) {
  const next = tr.nextElementSibling;
  if (next && next.classList.contains("detail")) {
    next.remove();
    return;
  }
  const d = h("tr.detail", {}, h("td", { colspan: 5 }, h("p.muted", {}, "Loading…")));
  d.dataset.id = id;
  tr.after(d);
  fillEventDetail(d, id);
}

// Keep the "x ago" column current without reloading anything.
setInterval(() => {
  for (const td of $$("#events-body td[data-time]")) td.textContent = ago(td.dataset.time);
}, 15000);

// ---------- live connection ----------
function setLiveStatus(status) {
  const pill = $("#events-live");
  pill.dataset.status = status;
  $(".label", pill).textContent = { live: "Live", connecting: "Connecting…", reconnecting: "Reconnecting…", offline: "Offline" }[status] || status;
  pill.title = status === "live" ? "New events and rule results appear here as they happen." : "Not receiving live updates right now; use Refresh.";
}

// Tabs that show live data: the event log, and rules' running state.
const liveTabs = ["rules", "events"];
const currentTab = () => ($("#tabs button.active") || {}).dataset?.tab;

function startLive() {
  if (state.live) return;
  const es = new EventSource("/api/events/stream");
  state.live = es;
  setLiveStatus("connecting");
  let connectedBefore = false;
  es.addEventListener("open", () => {
    setLiveStatus("live");
    // After a reconnect, catch up on anything missed while disconnected.
    if (connectedBefore) {
      if (!$("#view-events").classList.contains("hidden")) loadEvents();
      if (!$("#view-rules").classList.contains("hidden")) loadRules();
    }
    connectedBefore = true;
  });
  es.addEventListener("event", (m) => {
    try {
      const ev = stampEvent(JSON.parse(m.data));
      if (!$("#view-events").classList.contains("hidden")) upsertEvent(ev);
      if (!$("#view-rules").classList.contains("hidden")) applyRuleActivity(ev);
    } catch {
      // ignore a malformed message
    }
  });
  es.addEventListener("signedout", () => {
    stopLive();
    location.reload();
  });
  es.addEventListener("error", () => {
    if (es.readyState === EventSource.CLOSED) {
      // The browser gave up (e.g. the server refused); try again shortly.
      setLiveStatus("offline");
      if (state.live === es) state.live = null;
      setTimeout(() => { if (liveTabs.includes(currentTab())) startLive(); }, 5000);
    } else {
      setLiveStatus("reconnecting");
    }
  });
}

function stopLive() {
  if (state.live) state.live.close();
  state.live = null;
}


const skipReason = {
  cooldown: "Event accepted but ignored: the rule is inside its cooldown window.",
  busy: "Event accepted but ignored: a previous run of this rule was still in progress.",
  offhours: "Event accepted but ignored: it came outside the rule's active hours.",
};

async function fillEventDetail(row, id) {
  const ev = await api("GET", `/api/events/${id}`);
  stampEvent(ev);
  const cell = h("td", { colspan: 5 },
    ev.runs.map((r) => h("div", {},
      h("h3", {}, r.ruleName, " ", h(`span.badge.${r.status}`, {}, r.status)),
      skipReason[r.status] ? h("p.muted", {}, skipReason[r.status]) : [
        r.status === "running" ? runProgress(r) : null,
        r.steps.length || r.status !== "running" ? renderSteps(r.steps) : null,
      ])),
    h("details", {}, h("summary", {}, "Payload"), h("pre", {}, JSON.stringify(ev.payload, null, 2))),
    h("div.row", {}, matrixLink(ev, "Open in Matrix ↗"), isAdmin() ? h("button.btn.small", {
      onclick: async (evt) => {
        evt.stopPropagation();
        const btn = evt.currentTarget;
        const yes = await confirmDialog({
          title: "Replay this event?",
          message: [`This sends the ${ev.topic}:${ev.code} event through all enabled rules again, as a test. Matching rules will run their actions for real and call your devices. Cooldowns and “ignore while running” still apply.`],
          confirmLabel: "Replay",
        });
        if (!yes) return;
        try {
          const res = await api("POST", `/api/events/${id}/replay`);
          // The replayed event itself arrives through the live stream.
          const n = res.matchedRules.length;
          flashButton(btn, n ? `Replayed · ${n} rule${n === 1 ? "" : "s"} matched` : "Replayed · no rules matched", "copied");
        } catch (e) {
          await infoDialog("Replay failed", e.message);
        }
      },
    }, "Replay through rules") : null),
  );
  row.replaceChildren(cell);
}

// ---------- setup ----------
function wireSetup() {
  const topicSel = $("#sim-topic");
  topicSel.append(...state.catalog.topics.map((t) => h("option", { value: t }, t)));
  topicSel.value = "device";
  const fillCodes = () => {
    $("#sim-code").replaceChildren(...state.catalog.codes[topicSel.value].map((c) => h("option", { value: c }, c)));
    if (topicSel.value === "device") $("#sim-code").value = "vmd";
  };
  topicSel.addEventListener("change", () => { fillCodes(); loadSample(); });
  $("#sim-code").addEventListener("change", loadSample);
  fillCodes();
  $("#sim-sample").addEventListener("click", loadSample);
  $("#copy-webhook").addEventListener("click", (e) => copyText($("#webhook-url").textContent, e.currentTarget));
  $("#sim-send").addEventListener("click", async () => {
    const out = $("#sim-result");
    out.textContent = "Sending…";
    try {
      const res = await api("POST", "/api/simulate", $("#sim-payload").value);
      out.textContent = `Dispatched. ${res.matchedRules.length} rule(s) matched. See the Events tab for results.`;
    } catch (e) {
      out.textContent = "Error: " + e.message;
    }
  });
}

function loadSample() {
  const topic = $("#sim-topic").value;
  const code = $("#sim-code").value;
  const data = {
    siteId: "182140d9-d99c-4dc8-ae14-0071a01d7e98", siteName: "HQ", siteDescription: "Company headquarters",
    groupId: "cf84e116-ee61-4bd8-b078-3d2fadfcc9ac", groupName: "West entrance", groupDescription: "E1", groupActive: true, groupState: "schedule",
  };
  if (topic !== "alarmgroup") Object.assign(data, { deviceId: "0eeb5774-e88d-11ef-95f8-afbaa9403ab9", deviceName: "CAM-01", deviceDescription: "Camera 01" });
  if (topic === "device" && ["vmd", "aoa", "motion_guard", "fence_guard", "loitering_guard"].includes(code)) data.videoSourceId = "0";
  if (topic === "barrier") Object.assign(data, { barrierId: "5d0b1f7c-ee13-11ef-bc15-b33a54d715fa", barrierName: "Front door", barrierDescription: "Main entrance door" });
  if (topic === "peripheral") Object.assign(data, { peripheralId: "6630e036-ee13-11ef-bc15-b33a54d715fa", peripheralName: "Input 1", peripheralDescription: "Door contact" });
  if (topic === "sensor") Object.assign(data, { sensorId: "7a41c2de-ee13-11ef-bc15-b33a54d715fa", sensorName: "Temperature", sensorDescription: "Server room temperature" });
  $("#sim-payload").value = JSON.stringify({
    eventTopic: topic, eventCode: code, eventDescription: `Simulated ${topic}:${code}`,
    eventOccurred: new Date().toISOString(), eventId: crypto.randomUUID ? crypto.randomUUID() : String(Date.now()), eventData: data,
  }, null, 2);
}

async function loadSetup() {
  if (!isAdmin()) return;
  if (!$("#sim-payload").value) loadSample();
  const info = await api("GET", "/api/info");
  $("#webhook-url").textContent = info.webhookUrl;
  const checks = [`User-Agent check: ${info.requireUserAgent ? "on" : "off"}`, `Source IP check: ${info.verifySourceIp ? "on" : "off"}`];
  if (info.clientIpHeader) checks.push(`Client IP header: ${info.clientIpHeader}`);
  checks.push(`Event history kept ${info.retentionDays} days`);
  $("#webhook-checks").textContent = checks.join(" · ");
}

// ---------- secrets ----------
function wireSecrets() {
  $("#secret-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const msg = $("#secret-msg");
    const name = $("#secret-name").value.trim().toUpperCase();
    try {
      await api("PUT", `/api/secrets/${encodeURIComponent(name)}`, { value: $("#secret-value").value });
      msg.textContent = `Saved ${name}.`;
      $("#secret-form").reset();
      loadSecrets();
    } catch (ex) {
      msg.textContent = ex.message;
    } finally {
      $("#secret-value").value = "";
    }
  });
}

async function loadSecrets() {
  const res = await api("GET", "/api/secrets");
  $("#secrets-disabled").classList.toggle("hidden", res.enabled);
  $("#secrets-panel").classList.toggle("hidden", !res.enabled);
  if (!res.enabled) {
    $("#secrets-disabled").replaceChildren(h("p.error", {}, res.error));
    return;
  }
  const body = $("#secrets-body");
  if (!res.secrets.length) {
    body.replaceChildren(h("tr", {}, h("td.empty", { colspan: 4 }, "No secrets yet.")));
    return;
  }
  body.replaceChildren(...res.secrets.map((s) => h("tr", {},
    h("td", {}, h("code", {}, s.name)),
    h("td", {}, s.usedBy.length ? s.usedBy.map((n) => h("span.badge", {}, n)) : h("span.muted.small", {}, "unused")),
    h("td.muted.small", { title: fmtTime(s.updatedAt) }, ago(s.updatedAt)),
    h("td", {},
      h("button.btn.small", { onclick: () => { $("#secret-name").value = s.name; $("#secret-value").focus(); } }, "Replace"),
      " ",
      h("button.btn.small.danger", {
        onclick: async () => {
          const yes = await confirmDialog({
            title: "Delete this secret?",
            message: [h("code", {}, s.name), " will be permanently deleted. Its value can't be recovered.",
              s.usedBy.length ? h("span.error", {}, ` These will fail until it's re-added: ${s.usedBy.join(", ")}.`) : null],
            confirmLabel: "Delete secret", danger: true,
          });
          if (!yes) return;
          await api("DELETE", `/api/secrets/${encodeURIComponent(s.name)}`);
          loadSecrets();
        },
      }, "Delete"),
    ),
  )));
}

// ---------- clipboard ----------
// navigator.clipboard only exists on HTTPS or localhost; over plain HTTP on
// a LAN IP fall back to the legacy select-and-copy path.
async function copyText(text, btn) {
  let ok = false;
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      ok = true;
    } else {
      const ta = h("textarea", { readonly: true, "aria-hidden": "true" });
      ta.value = text;
      ta.className = "offscreen";
      document.body.append(ta);
      ta.select();
      ok = document.execCommand("copy");
      ta.remove();
    }
  } catch {
    ok = false;
  }
  flashButton(btn, ok ? "Copied ✓" : "Copy failed", ok ? "copied" : "copy-failed");
  if (!ok) {
    // Leave the text selected so it can be copied by hand.
    const sel = window.getSelection();
    const range = document.createRange();
    range.selectNodeContents($("#webhook-url"));
    sel.removeAllRanges();
    sel.addRange(range);
  }
}

function flashButton(btn, label, cls) {
  clearTimeout(btn._flash);
  btn.dataset.label ??= btn.textContent;
  btn.textContent = label;
  btn.classList.remove("copied", "copy-failed");
  btn.classList.add(cls);
  btn._flash = setTimeout(() => {
    btn.textContent = btn.dataset.label;
    btn.classList.remove(cls);
  }, 1800);
}

// ---------- unsaved-changes guard ----------
// Stable JSON (sorted keys) so key order never counts as a change.
function canonical(v) {
  if (Array.isArray(v)) return `[${v.map(canonical).join(",")}]`;
  if (v && typeof v === "object") {
    return `{${Object.keys(v).sort().map((k) => `${JSON.stringify(k)}:${canonical(v[k])}`).join(",")}}`;
  }
  return JSON.stringify(v);
}

// The editor's current content in comparable form. Unparseable JSON is
// compared as raw text, so any edit to it still counts as a change.
function editorState() {
  if ($("#editor-json-mode").checked) {
    const text = $("#f-json").value;
    try {
      return canonical(cleanRule(JSON.parse(text)));
    } catch {
      return "raw:" + text;
    }
  }
  return canonical(cleanRule(readVisual()));
}

// guardEditor makes an editor dialog close on a backdrop click, Esc or its
// Cancel button, asking first (via the shared #discard-dlg) when stateFn()
// differs from the snapshot taken when it was opened.
const guards = {};
function guardEditor(key, dlg, cancelBtn, stateFn) {
  let snap;
  const dirty = () => snap !== undefined && stateFn() !== snap;
  const requestClose = () => {
    if (!dirty()) {
      dlg.close();
      return;
    }
    state.discardTarget = dlg;
    const confirmDlg = $("#discard-dlg");
    if (!confirmDlg.open) confirmDlg.showModal();
  };
  cancelBtn.addEventListener("click", requestClose);

  // Backdrop clicks land on the <dialog> element itself, but so do clicks on
  // its scrollbar, so test the point against the dialog's box. Require the
  // press to start outside too, so drag-selecting text never closes it.
  const outside = (e) => {
    const r = dlg.getBoundingClientRect();
    return e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom;
  };
  let pressedOutside = false;
  dlg.addEventListener("mousedown", (e) => { pressedOutside = e.target === dlg && outside(e); });
  dlg.addEventListener("click", (e) => {
    if (pressedOutside && e.target === dlg && outside(e)) requestClose();
    pressedOutside = false;
  });
  // Esc. Chrome doesn't always let a page cancel the dialog's own "cancel"
  // (close-watcher rules), so take the key before the dialog acts on it.
  // The cancel handler stays as a backstop for other close requests.
  dlg.addEventListener("keydown", (e) => {
    if (e.key !== "Escape" || $("#discard-dlg").open) return;
    e.preventDefault();
    e.stopPropagation();
    requestClose();
  });
  dlg.addEventListener("cancel", (e) => {
    e.preventDefault();
    requestClose();
  });
  dlg.addEventListener("close", () => { snap = undefined; });

  guards[key] = { dirty, requestClose, snapshot: () => { snap = stateFn(); } };
  return guards[key];
}

// The "Discard unsaved changes?" dialog is shared by every guarded editor.
function wireDiscardDialog() {
  const confirmDlg = $("#discard-dlg");
  $("#discard-keep").addEventListener("click", () => confirmDlg.close());
  $("#discard-yes").addEventListener("click", () => {
    confirmDlg.close();
    if (state.discardTarget) state.discardTarget.close();
  });
  // Clicking the confirm dialog's own backdrop means "keep editing".
  confirmDlg.addEventListener("click", (e) => {
    const r = confirmDlg.getBoundingClientRect();
    if (e.target === confirmDlg && (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom)) confirmDlg.close();
  });
}

// ---------- reload UI ----------
// Drop cached UI files and reload. Clear-Site-Data (from the server) empties
// the browser's HTTP cache for this site on HTTPS/localhost without signing
// out; fetching with cache:"reload" also replaces the cached copies over
// plain HTTP, where browsers ignore that header.
async function reloadUI(e) {
  const btn = e.currentTarget;
  btn.disabled = true;
  btn.classList.add("spinning");
  try {
    await fetch("/ui/clear-cache", { method: "POST", cache: "no-store" });
  } catch {
    // Server unreachable: still try a hard reload below.
  }
  try {
    if ("caches" in window) {
      for (const key of await caches.keys()) await caches.delete(key);
    }
    await Promise.all(["/", "/index.html", "/app.js", "/app.css"].map((u) => fetch(u, { cache: "reload" }).catch(() => {})));
  } finally {
    location.reload();
  }
}

// ---------- saved actions ----------
// Stored and served as "requests" (/api/requests, action type "request"):
// they started out as HTTP-only saved requests.
async function loadRequests() {
  state.requests = await api("GET", "/api/requests");
  const list = $("#requests-list");
  if (!state.requests.length) {
    list.replaceChildren(h("div.empty", {}, isAdmin() ? "No saved actions yet. Create one to reuse it across rules." : "No saved actions yet."));
    return;
  }
  list.replaceChildren(...state.requests.map(requestCard));
}

function requestCard(sr) {
  const admin = isAdmin();
  const req = sr.request;
  // Every templated string is in the JSON, with its quotes escaped.
  const usesSecrets = /\bsecret\s+\\"/.test(JSON.stringify(req));
  const card = h("div.card", {},
    h("div.card-head", {},
      h("h3", {}, sr.name),
      h("span.badge.type", {}, ACTION_LABELS[req.type || "http"]),
      req.auth ? h("span.badge", {}, `${req.auth} auth`) : null,
      usesSecrets ? h("span.badge", { title: "Uses secrets: only rules of the admin who saved it can use it" }, "uses secrets") : null,
      admin ? h("button.btn.small", { onclick: () => testRequest(sr) }, "Test") : null,
      h("button.btn.small", { onclick: () => showHistory("action", sr.id, sr.name) }, "History"),
      admin ? h("button.btn.small", { onclick: () => openRequestEditor(sr) }, "Edit") : null,
      admin ? h("button.btn.small.danger", { onclick: () => deleteRequest(sr) }, "Delete") : null,
    ),
    sr.description ? h("p.summary", {}, sr.description) : null,
    h("p.request-line", {}, savedActionLine(sr)),
    sr.params.length
      ? h("p.summary", {}, "Parameters: ", sr.params.map((p) => h("span.badge", { title: p.description || "" }, p.default ? `${p.name} = ${p.default}` : `${p.name} (required)`)))
      : null,
    h("p.summary", {}, sr.usedBy.length ? ["Used by: ", sr.usedBy.map((r) => h("span.badge", {}, r.name))] : "Not used by any rule yet."),
    sr.updatedByName && state.me && sr.updatedBy !== state.me.id
      ? h("p.muted.small", {}, `Saved by ${sr.updatedByName}.`)
      : null,
  );
  if (admin) clickToEdit(card, `${sr.name}: ${ACTION_LABELS[req.type || "http"]}`, () => openRequestEditor(sr));
  return card;
}

const blankSaved = (type) => ({ ...blankAction(type), name: undefined, continueOnError: undefined });
const blankRequest = () => ({ name: "", description: "", params: [], request: blankSaved("http") });

function addParamRow(p = {}) {
  const row = h("div.param-row", {},
    h("input.p-name", { value: p.name || "", placeholder: "name, e.g. preset", "aria-label": "Parameter name" }),
    h("input.p-default", { value: p.default || "", placeholder: "default (empty = required)", "aria-label": "Default value" }),
    h("input.p-desc", { value: p.description || "", placeholder: "description (shown to rule editors)", "aria-label": "Description" }),
    h("button.btn.small.danger", { type: "button", "aria-label": "Remove parameter", onclick: () => row.remove() }, "✕"),
  );
  $("#r-params").append(row);
}

function readRequestForm() {
  const box = $("#r-request .action-box");
  const request = box ? readAction(box) : {};
  // Rules supply these; the fixed form has no inputs for them anyway.
  delete request.name;
  delete request.continueOnError;
  return {
    name: $("#r-name").value.trim(),
    description: $("#r-desc").value.trim(),
    params: $$("#r-params .param-row").map((row) => {
      const p = { name: $(".p-name", row).value.trim() };
      const d = $(".p-default", row).value;
      const desc = $(".p-desc", row).value.trim();
      if (d !== "") p.default = d;
      if (desc) p.description = desc;
      return p;
    }).filter((p) => p.name),
    request,
  };
}

function openRequestEditor(sr) {
  state.editingRequest = sr;
  const v = sr ? structuredClone(sr) : blankRequest();
  $("#req-title").textContent = sr ? `Edit: ${sr.name}` : "New saved action";
  $("#req-error").textContent = "";
  $("#r-name").value = v.name;
  $("#r-desc").value = v.description || "";
  $("#r-params").replaceChildren();
  (v.params || []).forEach((p) => addParamRow(p));
  const type = v.request.type || "http";
  $("#r-type").value = type;
  // One form per type, kept while the editor is open so switching type and
  // back doesn't lose what was typed.
  state.savedForms = {};
  $("#r-request").replaceChildren();
  state.savedForms[type] = addActionBox({ ...v.request, type }, $("#r-request"), { fixed: true });
  if (!$("#req-editor").open) $("#req-editor").showModal();
  guards.request.snapshot();
}

function wireRequestEditor() {
  guardEditor("request", $("#req-editor"), $("#req-cancel"), () => canonical(readRequestForm()));
  $("#new-request").addEventListener("click", () => openRequestEditor(null));
  $("#r-add-param").addEventListener("click", () => addParamRow());
  $("#r-type").addEventListener("change", (e) => {
    const type = e.target.value;
    const box = state.savedForms[type];
    if (box) $("#r-request").replaceChildren(box);
    else {
      $("#r-request").replaceChildren();
      state.savedForms[type] = addActionBox(blankSaved(type), $("#r-request"), { fixed: true });
    }
  });
  $("#req-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const err = $("#req-error");
    err.textContent = "";
    const body = readRequestForm();
    const save = async (expected) => {
      err.textContent = "";
      try {
        if (state.editingRequest) await api("PUT", `/api/requests/${state.editingRequest.id}`, { ...body, ...(expected ? { updatedAt: expected } : {}) });
        else await api("POST", "/api/requests", body);
        $("#req-editor").close();
        loadRequests();
      } catch (ex) {
        if (ex.status === 409 && ex.data?.conflict) {
          showConflict(err, ex, state.editingRequest, {
            overwrite: () => save(ex.data.current.updatedAt),
            reload: () => openRequestEditor(ex.data.current),
          });
        } else err.textContent = ex.message;
      }
    };
    save(state.editingRequest?.updatedAt);
  });
}

async function deleteRequest(sr) {
  const yes = await confirmDialog({
    title: "Delete this saved action?",
    message: [h("strong", {}, sr.name), " will be removed. Rules can no longer use it."],
    confirmLabel: "Delete saved action", danger: true,
  });
  if (!yes) return;
  try {
    await api("DELETE", `/api/requests/${sr.id}`);
  } catch (e) {
    await infoDialog(`Can't delete “${sr.name}”`, e.message);
  }
  loadRequests();
}

// testRequest runs a saved action on its own. With parameters, it first
// asks for their values (defaults pre-filled).
function testRequest(sr) {
  const does = { delay: ["Testing ", h("strong", {}, sr.name), " just waits; nothing is sent."],
    discord: ["Testing posts ", h("strong", {}, sr.name), " to Discord for real."] }[sr.request.type]
    || ["Testing sends ", h("strong", {}, sr.name), " for real, so it will call the device it points at."];
  const run = async (params) => {
    showResult(`Testing "${sr.name}"…`, h("p.muted", {}, "Starting…"));
    let start;
    try {
      start = await api("POST", `/api/requests/${sr.id}/test`, { params });
    } catch (e) {
      showResult("Test failed", h("p.error", {}, e.message));
      return;
    }
    await pollRun(`Test: ${sr.name}`, start, 1);
    loadRequests();
  };
  if (!sr.params.length) {
    confirmDialog({
      title: "Run this saved action now?",
      message: does,
      confirmLabel: "Run test",
    }).then((yes) => yes && run({}));
    return;
  }
  const form = h("form.params-box", {},
    sr.params.map((p) => h("label.param-input", {},
      h("span", {}, p.name, p.default ? "" : h("span.error", { title: "required" }, " *")),
      h("input", { "data-param": p.name, value: p.default || "", placeholder: p.description || "", required: !p.default }))),
    h("p.muted.small", {}, "Values can use event data, e.g. {{ .data.deviceName }}; a sample motion event is used."),
    h("div.row", {}, h("button.btn.primary", { type: "submit" }, "Run test")),
  );
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    const params = {};
    for (const el of $$("[data-param]", form)) if (el.value !== "") params[el.dataset.param] = el.value;
    run(params);
  });
  showResult(`Test: ${sr.name}`, form);
  $("input", form)?.focus();
}

// ---------- live run progress ----------
// The server sends a running step's elapsed/remaining time from its own
// clock. Turn those into local deadlines once, when the data arrives, so
// countdowns are right even if this computer's clock is off.
function stampRun(run) {
  const cur = run && run.current;
  if (cur && cur._startedLocal === undefined) {
    const now = Date.now();
    cur._startedLocal = now - (cur.elapsedMs || 0);
    if (cur.remainingMs != null) {
      cur._untilLocal = now + cur.remainingMs;
      cur._durationMs = (cur.elapsedMs || 0) + cur.remainingMs;
    }
  }
  return run;
}
const stampEvent = (ev) => { (ev.runs || []).forEach(stampRun); return ev; };

function fmtClock(ms) {
  const s = Math.max(0, ms) / 1000;
  if (s < 10) return `${s.toFixed(1)}s`;
  if (s < 60) return `${Math.floor(s)}s`;
  return `${Math.floor(s / 60)}:${String(Math.floor(s % 60)).padStart(2, "0")}`;
}

// runProgress shows what a running rule is doing: "Step 2 of 4 · Settle ·
// 4.2s left" (delays count down, with a bar) or "… · 3s elapsed".
// compact is the short form for badges: "2/4 · 4.2s".
function runProgress(run, { compact = false } = {}) {
  stampRun(run);
  const cur = run.current;
  const total = run.totalSteps || "?";
  if (!cur) return h(compact ? "span.progress" : "div.progress", {}, h("span.pulse-dot", { "aria-hidden": "true" }), "Starting…");
  const isDelay = cur._untilLocal !== undefined;
  const timer = isDelay
    ? h("span.countdown", { "data-until": cur._untilLocal, "data-suffix": compact ? "" : " left" }, fmtClock(cur._untilLocal - Date.now()) + (compact ? "" : " left"))
    : h("span.elapsed", { "data-since": cur._startedLocal, "data-suffix": compact ? "" : " elapsed" }, fmtClock(Date.now() - cur._startedLocal) + (compact ? "" : " elapsed"));
  // Steps past the rule's own actions are its on-failure actions.
  const failing = typeof total === "number" && cur.index >= total;
  const where = failing ? "Handling failure" : `Step ${cur.index + 1} of ${total}`;
  if (compact) {
    return h("span.progress", { title: `${where}: ${cur.name}` }, h("span.pulse-dot", { "aria-hidden": "true" }), failing ? "failure · " : `${cur.index + 1}/${total} · `, timer);
  }
  const seen = cur.matches ? ` · ${cur.matches} match${cur.matches === 1 ? "" : "es"}${cur.restarts ? `, restarted ${cur.restarts}×` : ""}` : "";
  const bar = isDelay ? h("div.delay-bar", { "aria-hidden": "true" }, h("div.delay-fill", { "data-start": cur._untilLocal - cur._durationMs, "data-until": cur._untilLocal })) : null;
  return h("div.progress", {},
    h("div.progress-line", {}, h("span.pulse-dot", { "aria-hidden": "true" }), h("strong", {}, where), ` · ${cur.name} · `, timer, seen),
    bar);
}

// One ticker keeps every countdown, elapsed timer and delay bar moving.
function tickProgress() {
  const now = Date.now();
  for (const el of $$("[data-until].countdown")) el.textContent = fmtClock(Number(el.dataset.until) - now) + (el.dataset.suffix || "");
  for (const el of $$("[data-since]")) el.textContent = fmtClock(now - Number(el.dataset.since)) + (el.dataset.suffix || "");
  for (const el of $$(".delay-fill")) {
    const start = Number(el.dataset.start), until = Number(el.dataset.until);
    const pct = until > start ? Math.min(100, Math.max(0, ((now - start) / (until - start)) * 100)) : 100;
    el.style.width = `${pct}%`;
  }
}
setInterval(tickProgress, 250);

// applyRuleActivity updates rule cards from a pushed event's runs, so the
// Rules tab shows runs starting, progressing and finishing live.
function applyRuleActivity(ev) {
  for (const run of ev.runs || []) {
    const rule = state.rules.find((r) => r.id === run.ruleId);
    if (!rule) continue;
    const before = (rule.activeRuns || []).filter((x) => x.id !== run.id);
    rule.activeRuns = run.status === "running" ? before.concat(stampRun(run)) : before;
    if (run.status !== "running" && run.status !== "cooldown" && run.status !== "busy") rule.lastFiredAt = rule.lastFiredAt || run.startedAt;
    const card = $(`#rules-list .card[data-rule-id="${rule.id}"]`);
    if (card) card.replaceWith(ruleCard(rule));
  }
  tickProgress();
}

// ---------- confirm dialog ----------
// confirmDialog asks before anything that changes data or calls real
// devices. Resolves true only on the confirm button; Cancel (focused by
// default, so a stray Enter is harmless), Esc and a backdrop click are no.
function confirmDialog({ title, message, confirmLabel = "OK", danger = false, cancelLabel = "Cancel" }) {
  const dlg = $("#confirm-dlg");
  $("#confirm-title").textContent = title;
  $("#confirm-msg").replaceChildren(...[].concat(message).filter((m) => m !== null && m !== undefined && m !== ""));
  const ok = $("#confirm-ok");
  const cancel = $("#confirm-cancel");
  ok.textContent = confirmLabel || "";
  ok.className = "btn " + (danger ? "danger-solid" : "primary");
  ok.hidden = confirmLabel === null;
  cancel.textContent = cancelLabel;
  return new Promise((resolve) => {
    const finish = (answer) => {
      ok.removeEventListener("click", onOk);
      cancel.removeEventListener("click", onCancel);
      dlg.removeEventListener("close", onClose);
      dlg.removeEventListener("click", onBackdrop);
      if (dlg.open) dlg.close();
      resolve(answer);
    };
    const onOk = () => finish(true);
    const onCancel = () => finish(false);
    const onClose = () => finish(false); // Esc
    const onBackdrop = (e) => {
      const r = dlg.getBoundingClientRect();
      if (e.target === dlg && (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom)) finish(false);
    };
    ok.addEventListener("click", onOk);
    cancel.addEventListener("click", onCancel);
    dlg.addEventListener("close", onClose);
    dlg.addEventListener("click", onBackdrop);
    dlg.showModal();
    cancel.focus();
  });
}

// infoDialog shows a message with just a Close button.
const infoDialog = (title, message) => confirmDialog({ title, message, confirmLabel: null, cancelLabel: "Close" });

// ---------- history ----------
// The edit log (GET /api/history), newest first. An entry opens to a line
// diff of the item's content against its previous version, or against any
// other version of the same item.
const KIND_LABELS = { rule: "Rule", action: "Saved action", schedule: "Schedule" };
const CHANGE_LABELS = { created: "created", updated: "edited", deleted: "deleted", existing: "before history" };
const FIELD_LABELS = {
  cooldownSeconds: "cooldown", cooldownFrom: "cooldown", skipIfRunning: "ignore while running",
  request: "action", params: "parameters",
};

function wireHistory() {
  $("#history-kind").addEventListener("change", () => {
    state.historyScope = null;
    loadHistory();
  });
  $("#history-more").addEventListener("click", () => loadHistory(true));
}

// showHistory opens the History tab on one rule or saved action.
function showHistory(kind, id, name) {
  state.historyScope = { kind, id, name };
  showTab("history");
}

async function loadHistory(older) {
  const list = $("#history-list");
  const scope = state.historyScope;
  if (scope) $("#history-kind").value = scope.kind;
  $("#history-scope").classList.toggle("hidden", !scope);
  if (scope) {
    $("#history-scope").replaceChildren(h("span.pill", {},
      h("span", {}, `${KIND_LABELS[scope.kind]}: ${scope.name}`),
      h("button", { type: "button", "aria-label": "Show all changes", onclick: () => {
        state.historyScope = null;
        $("#history-kind").value = "";
        loadHistory();
      } }, "✕")));
  }
  const prev = older ? state.history || [] : [];
  const qs = new URLSearchParams({ limit: 50 });
  if ($("#history-kind").value) qs.set("kind", $("#history-kind").value);
  if (scope) qs.set("id", scope.id);
  if (older && prev.length) qs.set("before", prev[prev.length - 1].id);
  if (!older) {
    state.historyItems = {};
    list.replaceChildren(h("div.empty", {}, "Loading…"));
  }
  let page;
  try {
    page = await api("GET", `/api/history?${qs}`);
  } catch (e) {
    list.replaceChildren(h("div.empty", {}, `Couldn't load history: ${e.message}`));
    return;
  }
  state.history = prev.concat(page);
  $("#history-more").classList.toggle("hidden", page.length < 50);
  if (!state.history.length) {
    list.replaceChildren(h("div.empty", {}, scope ? "No changes recorded for this item yet." : "No changes recorded yet. Edits made from now on show up here."));
    return;
  }
  if (older) list.append(...page.map(historyEntry));
  else list.replaceChildren(...page.map(historyEntry));
}

function historyEntry(rev) {
  const body = h("div.rev-body", {});
  const head = h("button.rev-head", { type: "button", "aria-expanded": "false" },
    h(`span.badge.change-${rev.change}`, {}, CHANGE_LABELS[rev.change] || rev.change),
    h("span.muted.small.rev-kind", {}, KIND_LABELS[rev.kind] || rev.kind),
    h("strong.rev-name", {}, rev.name),
    h("span.muted.small.rev-meta", { title: fmtTime(rev.at) }, `${rev.byName ? `${rev.byName} · ` : ""}${ago(rev.at)}`));
  const el = h("div.rev", {}, head, body);
  head.addEventListener("click", () => {
    const open = !el.classList.contains("open");
    el.classList.toggle("open", open);
    head.setAttribute("aria-expanded", String(open));
    if (open && !body.childElementCount) fillRevision(body, rev);
  });
  return el;
}

// itemRevisions is every revision of one item, newest first (cached while
// the list is shown).
async function itemRevisions(kind, id) {
  const key = `${kind}:${id}`;
  state.historyItems ||= {};
  state.historyItems[key] ||= api("GET", `/api/history?kind=${kind}&id=${id}&limit=500`);
  try {
    return await state.historyItems[key];
  } catch (e) {
    delete state.historyItems[key];
    throw e;
  }
}

async function fillRevision(body, rev) {
  body.replaceChildren(h("p.muted.small", {}, "Loading…"));
  let all;
  try {
    all = await itemRevisions(rev.kind, rev.itemId);
  } catch (e) {
    body.replaceChildren(h("p.error", {}, e.message));
    return;
  }
  const others = all.filter((r) => r.id !== rev.id);
  const previous = all.find((r) => r.id < rev.id);
  const label = (r) => `${r.id < rev.id ? "Earlier" : "Later"}: ${CHANGE_LABELS[r.change] || r.change}${r.byName ? ` by ${r.byName}` : ""} · ${fmtTime(r.at)}`;
  const sel = h("select", { "aria-label": "Compare with" },
    h("option", { value: "" }, rev.change === "deleted" ? "Nothing (show what was deleted)" : "Nothing (show this version in full)"),
    others.map((r) => h("option", { value: r.id }, label(r))));
  // A deletion's snapshot is the last version, so comparing it with the
  // previous one would show nothing; show what was removed instead.
  sel.value = rev.change !== "deleted" && previous ? String(previous.id) : "";
  const showAll = h("input", { type: "checkbox" });
  const out = h("div", {});
  const render = () => {
    const other = others.find((r) => String(r.id) === sel.value);
    let from = null, to = null;
    if (!other) [from, to] = rev.change === "deleted" ? [rev.snapshot, null] : [null, rev.snapshot];
    else if (other.id < rev.id) [from, to] = [other.snapshot, rev.snapshot];
    else [from, to] = [rev.snapshot, other.snapshot];
    const keys = from && to ? changedKeys(from, to) : [];
    out.replaceChildren(
      ...(keys.length ? [h("p.small.rev-summary", {}, "Changed: ", [...new Set(keys.map((k) => FIELD_LABELS[k] || k))].join(", "))] : []),
      ...diffView(pretty(from), pretty(to), showAll.checked));
  };
  sel.addEventListener("change", render);
  showAll.addEventListener("change", render);
  render();
  body.replaceChildren(
    h("div.row.rev-tools", {},
      h("label.inline", {}, "Compare with ", sel),
      h("label.inline", {}, showAll, "Show unchanged lines")),
    out);
}

// diffView renders a diff, folding long unchanged stretches down to a few
// lines of context unless all is set.
function diffView(a, b, all) {
  const lines = lineDiff(a, b);
  if (a && b && lines.every(([t]) => t === " ")) return [h("p.muted.small", {}, "No differences.")];
  const line = ([t, s]) => h(`span.d-${{ "+": "add", "-": "del", " ": "same" }[t]}`, {}, `${t} ${s}`);
  const CONTEXT = 3;
  const rows = [];
  for (let k = 0; k < lines.length;) {
    if (all || lines[k][0] !== " ") {
      rows.push(line(lines[k++]));
      continue;
    }
    let end = k;
    while (end < lines.length && lines[end][0] === " ") end++;
    const head = k === 0 ? 0 : CONTEXT, tail = end === lines.length ? 0 : CONTEXT;
    if (end - k <= head + tail + 1) {
      while (k < end) rows.push(line(lines[k++]));
      continue;
    }
    for (let q = k; q < k + head; q++) rows.push(line(lines[q]));
    rows.push(h("span.d-skip", {}, `⋯ ${end - k - head - tail} unchanged lines`));
    for (let q = end - tail; q < end; q++) rows.push(line(lines[q]));
    k = end;
  }
  return [h("pre.diff", {}, rows)];
}

// ---------- schedules ----------
// Named sets of time windows (GET /api/schedules) that rules pick to limit
// when they run. A schedule is open while any of its windows is: windows
// add together, so overlaps are fine and change nothing.
// The time maths (coverage, scheduleActive, …) lives in logic.js.

// timeline draws a week (Mon–Sun rows, 24 hours across) with the open
// times shaded, all windows merged, and a marker at the current time in the
// schedule's zone.
function timeline(s, { compact = false } = {}) {
  const bits = scheduleCoverage(s);
  const now = zonedMinute(s.timezone, new Date());
  const pct = (m) => `${(m / 1440) * 100}%`;
  const rows = DAYS.map(([, label], d) => {
    const segs = [];
    for (let m = 0; m < 1440;) {
      if (!bits[d * 1440 + m]) { m++; continue; }
      const start = m;
      while (m < 1440 && bits[d * 1440 + m]) m++;
      segs.push(h("span.tl-seg", { style: `left:${pct(start)};width:${pct(m - start)}`, title: `${label} ${fmtMinute(start)}–${fmtMinute(m)}` }));
    }
    const isToday = now >= 0 && Math.floor(now / 1440) === d;
    return h("div.tl-row", {},
      h(`span.tl-day${isToday ? ".today" : ""}`, {}, label),
      h("div.tl-bar", { "aria-label": `${label}: ${segs.length ? segs.map((x) => x.title.slice(4)).join(", ") : "closed"}` },
        segs, isToday ? h("span.tl-now", { style: `left:${pct(now % 1440)}`, title: `Now: ${fmtMinute(now % 1440)}` }) : null));
  });
  const ticks = compact ? null : h("div.tl-row.tl-ticks", { "aria-hidden": "true" }, h("span.tl-day", {}),
    h("div.tl-scale", {}, [0, 6, 12, 18, 24].map((hr) => h("span", { style: `left:${pct(hr * 60)}` }, `${hr}`.padStart(2, "0")))));
  return h(`div.timeline${compact ? ".compact" : ""}`, { role: "img", "aria-label": "Weekly open hours" }, rows, ticks);
}


// scheduleBadge labels a rule card with its schedule and whether it's open.
function scheduleBadge(id) {
  if (!id) return null;
  const s = (state.schedules || []).find((x) => x.id === id);
  if (!s) return h("span.badge.offhours", {}, `schedule #${id} missing`);
  const on = scheduleActive(s, new Date());
  return h(`span.badge${on ? "" : ".offhours"}`, { title: `Only runs ${scheduleText(s)}, ${tzLabel(s.timezone)} time. ${on ? "Open now." : "Closed now: events are ignored."}` },
    `${s.name}${on ? "" : " · closed now"}`);
}

async function loadSchedules() {
  state.schedules = await api("GET", "/api/schedules");
  const list = $("#schedules-list");
  if (!state.schedules.length) {
    list.replaceChildren(h("div.empty", {}, isAdmin() ? "No schedules yet. Create one, then pick it in a rule." : "No schedules yet."));
    return;
  }
  list.replaceChildren(...state.schedules.map(scheduleCard));
}

function scheduleCard(s) {
  const admin = isAdmin();
  const on = scheduleActive(s, new Date());
  const card = h("div.card", {},
    h("div.card-head", {},
      h("h3", {}, s.name),
      h(`span.badge.${on ? "ok" : "offhours"}`, {}, on ? "open now" : "closed now"),
      h("span.badge", { title: s.timezone }, tzLabel(s.timezone)),
      h("button.btn.small", { onclick: () => showHistory("schedule", s.id, s.name) }, "History"),
      admin ? h("button.btn.small", { onclick: () => openScheduleEditor(s) }, "Edit") : null,
      admin ? h("button.btn.small.danger", { onclick: () => deleteSchedule(s) }, "Delete") : null,
    ),
    s.description ? h("p.summary", {}, s.description) : null,
    h("ul.windows-list", {}, s.windows.map((w) => h("li", {}, windowSummary(w)))),
    timeline(s, { compact: true }),
    h("p.summary", {}, s.usedBy.length ? ["Used by: ", s.usedBy.map((r) => h("span.badge", {}, r.name))] : "Not used by any rule yet."),
  );
  if (admin) clickToEdit(card, `${s.name}: ${on ? "open" : "closed"} now`, () => openScheduleEditor(s));
  return card;
}

// fillTzSelect fills the time zone dropdown: common US zones first, then
// every zone the browser knows, grouped by region.
function fillTzSelect(sel, value) {
  const groups = [h("optgroup", { label: "Common" }, COMMON_TZ.map(([v, l]) => h("option", { value: v }, l)))];
  let all = [];
  try { all = Intl.supportedValuesOf("timeZone"); } catch { /* older browsers: common zones only */ }
  const byRegion = {};
  for (const z of all) {
    if (COMMON_TZ.some(([v]) => v === z)) continue;
    (byRegion[z.split("/")[0]] ||= []).push(z);
  }
  for (const region of Object.keys(byRegion).sort()) {
    groups.push(h("optgroup", { label: region }, byRegion[region].map((z) => h("option", { value: z }, z.replaceAll("_", " ")))));
  }
  sel.replaceChildren(...groups);
  if (value && ![...sel.options].some((o) => o.value === value)) sel.append(h("option", { value }, value));
  sel.value = value || DEFAULT_TZ;
}

function windowRow(w = {}) {
  const row = h("div.window-row", {},
    h("div.day-picks", {}, DAYS.map(([v, l]) => h("label.day-pick", {}, h("input", { type: "checkbox", value: v, checked: !!w.days?.includes(v) }), h("span", {}, l)))),
    h("div.row", {},
      h("label.inline", {}, "From ", h("input.w-from", { type: "time", value: w.from || "" })),
      h("label.inline", {}, "to ", h("input.w-to", { type: "time", value: w.to || "" })),
      h("button.btn.small", { type: "button", onclick: () => { $(".w-from", row).value = ""; $(".w-to", row).value = ""; refreshScheduleEditor(); } }, "All day"),
      h("button.btn.small.danger", { type: "button", "aria-label": "Remove window", onclick: () => { row.remove(); refreshScheduleEditor(); } }, "✕")),
    h("span.muted.small.window-summary", {}),
    h("div.window-note", {}));
  row.addEventListener("input", refreshScheduleEditor);
  row.addEventListener("change", refreshScheduleEditor);
  $("#s-windows").append(row);
  return row;
}

function readWindow(row) {
  const w = {};
  const days = $$(".day-pick input:checked", row).map((i) => i.value);
  if (days.length && days.length < 7) w.days = days;
  const from = $(".w-from", row).value, to = $(".w-to", row).value;
  if (from || to) Object.assign(w, { from, to });
  return w;
}

function readScheduleForm() {
  return {
    name: $("#s-name").value.trim(),
    description: $("#s-desc").value.trim(),
    timezone: $("#s-tz").value,
    windows: $$("#s-windows .window-row").map(readWindow),
  };
}

// refreshScheduleEditor updates each window's summary and warnings, the
// merged weekly timeline, and whether the schedule is open right now.
function refreshScheduleEditor() {
  const rows = $$("#s-windows .window-row");
  const s = readScheduleForm();
  const problem = (w) => (!w.from !== !w.to ? "Set both a start and an end time, or neither for all day."
    : w.from && w.from === w.to ? "Start and end are the same time. Clear both times (All day) to cover the whole day."
      : "");
  const valid = s.windows.map((w) => !problem(w));
  const redundant = redundantWindows(s.windows.map((w, i) => (valid[i] ? w : { days: [] })).filter((_, i) => valid[i]));
  // Map indexes among valid windows back to row indexes.
  const validIdx = s.windows.map((_, i) => i).filter((i) => valid[i]);
  rows.forEach((row, i) => {
    const w = s.windows[i];
    $(".window-summary", row).textContent = windowSummary(w);
    const note = $(".window-note", row);
    const err = problem(w);
    const dup = !err && redundant.has(validIdx.indexOf(i));
    row.classList.toggle("has-error", !!err);
    row.classList.toggle("is-redundant", dup);
    note.replaceChildren(...(err ? [h("span.error", {}, err)]
      : dup ? [h("span.warn-text", {}, "Other windows already cover all of this one, so it adds nothing. "),
        h("button.btn.small", { type: "button", onclick: () => { row.remove(); refreshScheduleEditor(); } }, "Remove it")]
        : []));
  });
  const usable = { ...s, windows: s.windows.filter((w, i) => valid[i]) };
  $("#s-timeline").replaceChildren(timeline(usable));
  const local = (() => {
    try { return new Intl.DateTimeFormat(undefined, { timeZone: s.timezone, weekday: "short", hour: "2-digit", minute: "2-digit" }).format(new Date()); } catch { return ""; }
  })();
  $("#sched-now").textContent = usable.windows.length
    ? `It's ${local} in ${tzLabel(s.timezone)}: this schedule is ${scheduleActive(usable, new Date()) ? "open" : "closed"} now.`
    : "Add a time window.";
}

function openScheduleEditor(s) {
  state.editingSchedule = s;
  $("#sched-title").textContent = s ? `Edit: ${s.name}` : "New schedule";
  $("#sched-error").textContent = "";
  $("#s-name").value = s?.name || "";
  $("#s-desc").value = s?.description || "";
  fillTzSelect($("#s-tz"), s?.timezone || DEFAULT_TZ);
  $("#s-windows").replaceChildren();
  (s?.windows?.length ? s.windows : [{ days: ["mon", "tue", "wed", "thu", "fri"], from: "22:00", to: "06:00" }]).forEach((w) => windowRow(w));
  refreshScheduleEditor();
  if (!$("#sched-editor").open) $("#sched-editor").showModal();
  guards.schedule.snapshot();
}

function wireScheduleEditor() {
  guardEditor("schedule", $("#sched-editor"), $("#sched-cancel"), () => canonical(readScheduleForm()));
  $("#new-schedule").addEventListener("click", () => openScheduleEditor(null));
  $("#s-add-window").addEventListener("click", () => { windowRow({}); refreshScheduleEditor(); });
  $("#s-tz").addEventListener("change", refreshScheduleEditor);
  $("#sched-form").addEventListener("submit", (e) => {
    e.preventDefault();
    const err = $("#sched-error");
    const body = readScheduleForm();
    const save = async (expected) => {
      err.textContent = "";
      try {
        if (state.editingSchedule) await api("PUT", `/api/schedules/${state.editingSchedule.id}`, { ...body, ...(expected ? { updatedAt: expected } : {}) });
        else await api("POST", "/api/schedules", body);
        $("#sched-editor").close();
        loadSchedules();
      } catch (ex) {
        if (ex.status === 409 && ex.data?.conflict) {
          showConflict(err, ex, state.editingSchedule, {
            overwrite: () => save(ex.data.current.updatedAt),
            reload: () => openScheduleEditor(ex.data.current),
          });
        } else err.textContent = ex.message;
      }
    };
    save(state.editingSchedule?.updatedAt);
  });
}

async function deleteSchedule(s) {
  const yes = await confirmDialog({
    title: "Delete this schedule?",
    message: [h("strong", {}, s.name), " will be removed."],
    confirmLabel: "Delete schedule", danger: true,
  });
  if (!yes) return;
  try {
    await api("DELETE", `/api/schedules/${s.id}`);
  } catch (e) {
    await infoDialog(`Can't delete “${s.name}”`, e.data?.usedBy ? ["Rules still use it: ", e.data.usedBy.map((r) => r.name).join(", "), ". Pick another schedule (or Any time) in those rules first."] : e.message);
  }
  loadSchedules();
}
