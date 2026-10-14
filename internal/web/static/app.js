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
  for (const c of children.flat()) {
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
    const err = new Error(data.error || `HTTP ${res.status}`);
    err.status = res.status;
    throw err;
  }
  return data;
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
const state = { me: null, catalog: null, rules: [], events: [], editing: null, eventsTimer: null };
const isAdmin = () => state.me && state.me.role === "admin";

// ---------- boot ----------
document.addEventListener("DOMContentLoaded", boot);

async function boot() {
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

  $$("#tabs button").forEach((b) => b.addEventListener("click", () => showTab(b.dataset.tab)));
  $("#new-rule").addEventListener("click", () => openEditor(null));
  $("#events-refresh").addEventListener("click", () => loadEvents());
  $("#events-more").addEventListener("click", () => loadEvents(true));
  $("#events-auto").addEventListener("change", scheduleEvents);
  wireEditor();
  wireSetup();
  wireSecrets();
  $("#result-close").addEventListener("click", () => $("#result-dlg").close());

  window.addEventListener("hashchange", () => showTab(location.hash.slice(1)));
  showTab(location.hash.slice(1) || "rules");
}

function renderMe() {
  const me = state.me;
  const box = $("#me");
  box.replaceChildren(
    ...(me.avatarUrl ? [h("img", { src: me.avatarUrl, alt: "" })] : []),
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
  if (!["rules", "events", "setup", "secrets"].includes(tab) || (tab === "secrets" && !isAdmin())) tab = "rules";
  if (location.hash !== `#${tab}`) history.replaceState(null, "", `#${tab}`);
  $$("#tabs button").forEach((b) => b.classList.toggle("active", b.dataset.tab === tab));
  $$(".view").forEach((v) => v.classList.toggle("hidden", v.id !== `view-${tab}`));
  clearTimeout(state.eventsTimer);
  if (tab === "rules") loadRules();
  if (tab === "events") loadEvents();
  if (tab === "setup") loadSetup();
  if (tab === "secrets") loadSecrets();
}

// ---------- rules ----------
async function loadRules() {
  state.rules = await api("GET", "/api/rules");
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
  if (a.type === "delay") return `${label}wait ${a.delayMillis} ms`;
  if (a.type === "discord") return `${label}Discord: ${a.message}`;
  return label + a.type;
}

function ruleCard(r) {
  const admin = isAdmin();
  return h("div.card" + (r.enabled ? "" : ".disabled"), {},
    h("div.card-head", {},
      h("h3", {}, r.name),
      r.enabled ? h("span.badge.ok", {}, "enabled") : h("span.badge", {}, "disabled"),
      r.running ? h("span.badge.running", {}, r.running > 1 ? `running ×${r.running}` : "running") : null,
      r.skipIfRunning ? h("span.badge", { title: "New events are ignored while a run is in progress" }, "one at a time") : null,
      r.cooldownSeconds ? h("span.badge", { title: `Ignores events for ${r.cooldownSeconds}s after the last run ${r.cooldownFrom === "finish" ? "finished" : "started"}` }, `once per ${fmtDuration(r.cooldownSeconds)}${r.cooldownFrom === "finish" ? " after finish" : ""}`) : null,
      h("span.muted.small", { title: fmtTime(r.lastFiredAt) }, `fired ${ago(r.lastFiredAt)}`),
      admin ? h("button.btn.small", { onclick: () => testRule(r) }, "Test") : null,
      admin ? h("button.btn.small", { onclick: () => openEditor(r) }, "Edit") : null,
      admin ? h("button.btn.small.danger", { onclick: () => deleteRule(r) }, "Delete") : null,
    ),
    h("p.summary", {}, triggerSummary(r.trigger)),
    r.updatedByName && r.updatedBy !== state.me.id
      ? h("p.muted.small", {}, `Saved by ${r.updatedByName}: runs with their secrets. Saving it yourself makes it yours.`)
      : null,
    h("ol.actions-list", {}, r.actions.map((a) => h("li", {}, actionSummary(a)))),
  );
}

async function deleteRule(r) {
  if (!confirm(`Delete rule "${r.name}"?`)) return;
  await api("DELETE", `/api/rules/${r.id}`);
  loadRules();
}

async function testRule(r) {
  showResult(`Testing "${r.name}"…`, h("p.muted", {}, "Running actions with a sample event…"));
  try {
    const res = await api("POST", `/api/rules/${r.id}/test`, {});
    showResult(`Test: ${r.name}`,
      h("p", {}, `Status: `, h(`span.badge.${res.run.status}`, {}, res.run.status),
        res.matchesTrigger ? "" : h("span.muted", {}, " (sample event would not match this trigger's filters)")),
      renderSteps(res.run.steps),
      h("details", {}, h("summary", {}, "Sample event used"), h("pre", {}, JSON.stringify(res.event, null, 2))),
    );
  } catch (e) {
    showResult("Test failed", h("p.error", {}, e.message));
  }
}

function renderSteps(steps) {
  if (!steps || !steps.length) return h("p.muted", {}, "No steps recorded.");
  return h("div", {}, steps.map((s) =>
    h("div.step" + (s.skipped ? "" : s.ok ? ".ok" : ".bad"), {},
      h("div", {}, h("strong", {}, `${s.index + 1}. ${s.name || s.type}`), " ",
        s.skipped ? h("span.badge", {}, "skipped") : h(`span.badge.${s.ok ? "ok" : "failed"}`, {}, s.ok ? "ok" : "failed"),
        s.status ? h("span.badge", {}, `HTTP ${s.status}`) : null,
        h("span.muted.small", {}, ` ${s.durationMs} ms`)),
      s.target ? h("div.muted.small", {}, s.target) : null,
      s.error ? h("div.error", {}, s.error) : null,
      s.response ? h("pre", {}, s.response) : null,
    )));
}

function showResult(title, ...body) {
  $("#result-title").textContent = title;
  $("#result-body").replaceChildren(...body);
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
    actions: r.actions || [],
  };
}

function wireEditor() {
  wireEditorClose();
  $("#add-filter").addEventListener("click", () => addFilterRow({ field: "deviceName", op: "equals", value: "" }));
  $$("[data-add-action]").forEach((b) => b.addEventListener("click", () => addActionBox(blankAction(b.dataset.addAction))));
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
    try {
      if (state.editing && state.editing.id) await api("PUT", `/api/rules/${state.editing.id}`, rule);
      else await api("POST", "/api/rules", rule);
      $("#editor").close();
      loadRules();
    } catch (ex) {
      err.textContent = ex.message;
    }
  });
}

function openEditor(rule) {
  state.editing = rule;
  $("#editor-title").textContent = rule ? `Edit: ${rule.name}` : "New rule";
  $("#editor-error").textContent = "";
  $("#editor-json-mode").checked = false;
  $("#editor-visual").classList.remove("hidden");
  $("#editor-json").classList.add("hidden");
  fillVisual(cleanRule(rule ? structuredClone(rule) : blankRule()));
  $("#editor").showModal();
  state.editorSnapshot = editorState();
}

function fillVisual(r) {
  $("#f-name").value = r.name;
  $("#f-enabled").checked = r.enabled;
  $("#f-cooldown").value = r.cooldownSeconds;
  $("#f-cooldown-from").value = r.cooldownFrom || "start";
  $("#f-skip-running").checked = !!r.skipIfRunning;

  const topics = $("#f-topics");
  topics.replaceChildren(...state.catalog.topics.map((t) =>
    h("label.chip", {}, h("input", { type: "checkbox", value: t, checked: r.trigger.topics.includes(t), onchange: () => renderCodes(readCodes()) }), t)));
  renderCodes(r.trigger.codes);

  $("#f-filters").replaceChildren();
  r.trigger.filters.forEach(addFilterRow);
  $("#f-actions").replaceChildren();
  r.actions.forEach(addActionBox);
}

const selectedTopics = () => $$("#f-topics input:checked").map((i) => i.value);
const readCodes = () => $$("#f-codes input:checked").map((i) => i.value);

function renderCodes(selected) {
  const topics = selectedTopics();
  const shown = topics.length ? topics : state.catalog.topics;
  const known = new Set();
  const chips = [];
  for (const t of shown) {
    for (const c of state.catalog.codes[t]) {
      // Qualify codes that exist under several topics only when ambiguous.
      known.add(c);
      chips.push(h("label.chip", { title: `${t}:${c}` }, h("input", { type: "checkbox", value: c, checked: selected.includes(c) || selected.includes(`${t}:${c}`) }), shown.length > 1 ? `${t}:${c}` : c));
    }
  }
  // Keep custom/unknown codes so JSON-entered values aren't lost.
  for (const c of selected) {
    if (!known.has(c) && !known.has(c.split(":").pop())) chips.push(h("label.chip", {}, h("input", { type: "checkbox", value: c, checked: true }), c));
  }
  $("#f-codes").replaceChildren(...chips);
}

function addFilterRow(f) {
  const row = $("#tpl-filter").content.firstElementChild.cloneNode(true);
  $(".ff-field", row).value = f.field || "";
  $(".ff-op", row).value = f.op || "equals";
  $(".ff-value", row).value = f.value || "";
  $(".ff-del", row).addEventListener("click", () => row.remove());
  $("#f-filters").append(row);
}

const field = (label, input) => [h("span.muted", {}, label), input];

function addActionBox(a) {
  const box = h("div.action-box", {});
  box.dataset.type = a.type;
  const head = h("div.action-head", {},
    h("strong", {}, { http: "HTTP request", delay: "Delay", discord: "Discord message" }[a.type] || a.type),
    h("button.btn.small", { type: "button", onclick: () => box.previousElementSibling && box.parentNode.insertBefore(box, box.previousElementSibling) }, "↑"),
    h("button.btn.small", { type: "button", onclick: () => box.nextElementSibling && box.parentNode.insertBefore(box.nextElementSibling, box) }, "↓"),
    h("button.btn.small.danger", { type: "button", onclick: () => box.remove() }, "✕"),
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
      field("Name", inp("name", { placeholder: "optional label" })),
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
        h("label.inline", {}, h("input", { type: "checkbox", "data-k": "continueOnError", checked: !!a.continueOnError }), "Continue on error"))),
    );
  } else if (a.type === "delay") {
    grid = h("div.action-grid", {},
      field("Name", inp("name", { placeholder: "optional label" })),
      field("Delay (ms)", inp("delayMillis", { type: "number", min: 1, max: 600000 })));
  } else if (a.type === "discord") {
    const msg = h("textarea", { "data-k": "message", rows: 2 });
    msg.value = a.message || "";
    grid = h("div.action-grid", {},
      field("Name", inp("name", { placeholder: "optional label" })),
      field("Webhook URL", inp("webhookUrl", { placeholder: "blank = Y6IFTTT_DISCORD_WEBHOOK_URL" })),
      field("Message", msg),
      field("Options", h("label.inline", {}, h("input", { type: "checkbox", "data-k": "continueOnError", checked: !!a.continueOnError }), "Continue on error")));
  } else {
    grid = h("pre", {}, JSON.stringify(a, null, 2));
    box.dataset.raw = JSON.stringify(a);
  }
  box.append(head, grid);
  $("#f-actions").append(box);
}

function readAction(box) {
  if (box.dataset.raw) return JSON.parse(box.dataset.raw);
  const a = { type: box.dataset.type };
  for (const el of $$("[data-k]", box)) {
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
    } else if (["timeoutSeconds", "delayMillis"].includes(k)) {
      if (el.value !== "") a[k] = Number(el.value);
    } else if (el.value !== "") {
      a[k] = el.value;
    }
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
      filters: $$("#f-filters .filter-row").map((row) => ({
        field: $(".ff-field", row).value.trim(),
        op: $(".ff-op", row).value,
        value: $(".ff-value", row).value,
      })).filter((f) => f.field),
    },
    actions: $$("#f-actions .action-box").map(readAction),
  };
}

// ---------- events ----------
async function loadEvents(older) {
  const before = older && state.events.length ? state.events[state.events.length - 1].id : 0;
  const page = await api("GET", `/api/events?limit=50${before ? `&before=${before}` : ""}`);
  state.events = older ? state.events.concat(page) : page;
  const open = new Set($$("#events-body tr.detail").map((r) => r.dataset.id));
  $("#events-body").replaceChildren(...(state.events.length
    ? state.events.flatMap((e) => eventRows(e, open.has(String(e.id))))
    : [h("tr", {}, h("td.empty", { colspan: 4 }, "No events yet. Point a YourSix webhook at this service or simulate one from Setup."))]));
  $("#events-more").classList.toggle("hidden", page.length < 50);
  scheduleEvents();
}

function scheduleEvents() {
  clearTimeout(state.eventsTimer);
  if ($("#events-auto").checked && !$("#view-events").classList.contains("hidden")) {
    state.eventsTimer = setTimeout(() => loadEvents(), 5000);
  }
}

function eventRows(e, expanded) {
  const tr = h("tr.ev", {},
    h("td", { title: fmtTime(e.receivedAt) }, ago(e.receivedAt)),
    h("td", {}, h("code", {}, `${e.topic}:${e.code}`), " ", h("span.muted", {}, e.description), e.test ? h("span.badge.test", {}, "test") : null),
    h("td.muted.small", {}, e.sourceIp),
    h("td", {}, e.runs.length ? e.runs.map((r) => h(`span.badge.${r.status}`, { title: r.status }, r.ruleName)) : h("span.muted.small", {}, "no match")),
  );
  tr.addEventListener("click", () => toggleEvent(tr, e));
  const rows = [tr];
  if (expanded) {
    const d = h("tr.detail", {}, h("td", { colspan: 4 }, h("p.muted", {}, "Loading…")));
    d.dataset.id = e.id;
    rows.push(d);
    fillEventDetail(d, e.id);
  }
  return rows;
}

function toggleEvent(tr, e) {
  const next = tr.nextElementSibling;
  if (next && next.classList.contains("detail")) {
    next.remove();
    return;
  }
  const d = h("tr.detail", {}, h("td", { colspan: 4 }, h("p.muted", {}, "Loading…")));
  d.dataset.id = e.id;
  tr.after(d);
  fillEventDetail(d, e.id);
}

const skipReason = {
  cooldown: "Event accepted but ignored: the rule is inside its cooldown window.",
  busy: "Event accepted but ignored: a previous run of this rule was still in progress.",
};

async function fillEventDetail(row, id) {
  const ev = await api("GET", `/api/events/${id}`);
  const cell = h("td", { colspan: 4 },
    ev.runs.map((r) => h("div", {},
      h("h3", {}, r.ruleName, " ", h(`span.badge.${r.status}`, {}, r.status)),
      skipReason[r.status] ? h("p.muted", {}, skipReason[r.status]) : renderSteps(r.steps))),
    h("details", {}, h("summary", {}, "Payload"), h("pre", {}, JSON.stringify(ev.payload, null, 2))),
    isAdmin() ? h("div.row", {}, h("button.btn.small", {
      onclick: async (evt) => {
        evt.stopPropagation();
        const res = await api("POST", `/api/events/${id}/replay`);
        alert(`Replayed: ${res.matchedRules.length} rule(s) matched.`);
        loadEvents();
      },
    }, "Replay through rules")) : null,
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
          const warn = s.usedBy.length ? `\n\nRules using it will fail: ${s.usedBy.join(", ")}` : "";
          if (!confirm(`Delete secret ${s.name}?${warn}`)) return;
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

const editorDirty = () => state.editorSnapshot !== undefined && editorState() !== state.editorSnapshot;

// Close the editor, asking first when there's something to lose.
function requestEditorClose() {
  if (!editorDirty()) {
    $("#editor").close();
    return;
  }
  const confirmDlg = $("#discard-dlg");
  if (!confirmDlg.open) confirmDlg.showModal();
}

function wireEditorClose() {
  const dlg = $("#editor");
  $("#editor-cancel").addEventListener("click", requestEditorClose);

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
    if (pressedOutside && e.target === dlg && outside(e)) requestEditorClose();
    pressedOutside = false;
  });
  // Esc. Chrome doesn't always let a page cancel the dialog's own "cancel"
  // (close-watcher rules), so take the key before the dialog acts on it.
  // The cancel handler stays as a backstop for other close requests.
  dlg.addEventListener("keydown", (e) => {
    if (e.key !== "Escape" || $("#discard-dlg").open) return;
    e.preventDefault();
    e.stopPropagation();
    requestEditorClose();
  });
  dlg.addEventListener("cancel", (e) => {
    e.preventDefault();
    requestEditorClose();
  });
  dlg.addEventListener("close", () => { state.editorSnapshot = undefined; });

  const confirmDlg = $("#discard-dlg");
  $("#discard-keep").addEventListener("click", () => confirmDlg.close());
  $("#discard-yes").addEventListener("click", () => {
    confirmDlg.close();
    dlg.close();
  });
  // Clicking the confirm dialog's own backdrop means "keep editing".
  confirmDlg.addEventListener("click", (e) => {
    const r = confirmDlg.getBoundingClientRect();
    if (e.target === confirmDlg && (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom)) confirmDlg.close();
  });
}
