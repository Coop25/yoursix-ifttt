// YourSix IFTTT — browser-only demo backend for GitHub Pages.
//
// Loaded before the real app.js. It replaces window.fetch (for /api, /auth
// and /ui paths) and window.EventSource (for the live stream) with an
// in-memory imitation of the Go server: rules with triggers and filters,
// cooldowns and "ignore while running", saved actions with parameters,
// write-only secrets, and runs that step through their actions in real
// time (delays count down; HTTP calls are simulated). Nothing leaves the
// browser, and reloading the page resets everything.
//
// It mirrors the Go server by hand, so API and engine changes usually need a
// matching change here: see "Keeping the demo in step" in the README. Shared
// pure logic lives in internal/web/static/logic.js instead of being copied.
(() => {
  "use strict";

  // ---------- helpers ----------
  const now = () => Date.now();
  const iso = (t = now()) => new Date(t).toISOString();
  const clone = (v) => JSON.parse(JSON.stringify(v));
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  const uuid = () => (crypto.randomUUID ? crypto.randomUUID() : `${now()}-${Math.random().toString(16).slice(2)}`);
  const ME = { id: "demo", username: "demo", displayName: "Demo admin", role: "admin" };
  const PLATFORM = "https://platform.yoursix.com";

  // ---------- catalog (mirrors internal/yoursix/event.go) ----------
  const CATALOG = {
    alarmgroup: ["disarmed", "armed", "schedule"],
    barrier: ["alarm", "alarm_end", "access_granted", "access_denied", "control_accessed", "control_locked", "control_unlocked", "mode_change"],
    device: ["disconnected", "connected", "storage_disruption", "storage_disruption_end", "storage_healthy", "shock_detected", "tilt_detected", "video_tampering", "casing_open", "audio", "aoa", "vmd", "loitering_guard", "motion_guard", "fence_guard", "perimeter_defender", "pir", "radar_motion", "input_peripheral", "axis_imagehealth", "axis_imagehealth_end", "temperature_warning", "temperature_warning_end", "cpu_overutilization", "cpu_overutilization_end", "ram_overutilization", "ram_overutilization_end", "storage_full", "storage_full_end", "network_packetloss", "network_packetloss_end", "modbus_connected", "modbus_disconnected"],
    peripheral: ["input_activated", "input_activated_end", "output_activated", "output_activated_end", "control_pulsed", "control_activated", "control_deactivated"],
    sensor: ["outside_range", "outside_range_end"],
  };
  const FIELDS = ["eventCode", "eventTopic", "eventDescription", "eventId", "siteId", "siteName", "groupId", "groupName", "groupState", "groupActive", "deviceId", "deviceName", "deviceDescription", "videoSourceId", "barrierId", "barrierName", "peripheralId", "peripheralName", "sensorId", "sensorName"];
  const DESCRIPTIONS = { vmd: "Video motion detection", aoa: "Object analytics", disconnected: "Device disconnected", connected: "Device connected", input_activated: "Input peripheral activated", armed: "Alarm group armed", outside_range: "Sensor outside range" };

  const DEVICES = {
    "CAM-01": { id: "0eeb5774-e88d-11ef-95f8-afbaa9403ab9", desc: "Front driveway camera" },
    "CAM-02": { id: "1f1c6657-3ccc-6874-b3ea-e3755baaf62d", desc: "Back yard camera" },
    "IO-01": { id: "5a7d1b20-e88d-11ef-95f8-afbaa9403ab9", desc: "Door I/O controller" },
  };

  // sample builds a payload shaped like yoursix.Sample.
  function sample(topic, code, opts = {}) {
    const data = {
      siteId: "182140d9-d99c-4dc8-ae14-0071a01d7e98", siteName: "HQ", siteDescription: "Company headquarters",
      groupId: "cf84e116-ee61-4bd8-b078-3d2fadfcc9ac", groupName: "West entrance", groupDescription: "E1", groupActive: true, groupState: "schedule",
    };
    if (topic !== "alarmgroup") {
      const name = opts.device || (topic === "peripheral" ? "IO-01" : "CAM-01");
      const d = DEVICES[name] || DEVICES["CAM-01"];
      Object.assign(data, { deviceId: d.id, deviceName: name, deviceDescription: d.desc });
    }
    if (topic === "peripheral") Object.assign(data, { peripheralId: "6630e036-ee13-11ef-bc15-b33a54d715fa", peripheralName: opts.peripheral || "Door contact", peripheralDescription: "Front door reed switch" });
    if (topic === "barrier") Object.assign(data, { barrierId: "5d0b1f7c-ee13-11ef-bc15-b33a54d715fa", barrierName: "Front door", barrierDescription: "Main entrance door" });
    if (topic === "sensor") Object.assign(data, { sensorId: "7a41c2de-ee13-11ef-bc15-b33a54d715fa", sensorName: "Temperature", sensorDescription: "Server room temperature" });
    if (topic === "device" && ["vmd", "aoa", "motion_guard", "fence_guard", "loitering_guard"].includes(code)) data.videoSourceId = "0";
    return {
      eventTopic: topic, eventCode: code, eventDescription: DESCRIPTIONS[code] || `Sample ${topic}:${code} event`,
      eventOccurred: iso(opts.at), eventId: uuid(), eventData: data,
    };
  }

  // ---------- state ----------
  const S = {
    rules: [], requests: [], secrets: [], events: [], nextRule: 1, nextRequest: 1, nextEvent: 1, nextRun: 1,
    lastFired: {}, lastFinished: {}, running: {}, history: [], nextRev: 1, schedules: [], nextSchedule: 1,
    watchers: new Set(), // watched delays waiting for events (see watchedDelay)
  };
  const stamp = { createdAt: iso(now() - 86400e3 * 3), updatedAt: iso(now() - 3600e3), updatedBy: ME.id, updatedByName: ME.displayName };

  function addSecret(name) { S.secrets.push({ name, createdAt: stamp.createdAt, updatedAt: stamp.updatedAt }); }
  ["FRONT_CAM_PASS", "HOME_HUB_TOKEN", "SIREN_PASS"].forEach(addSecret);

  function addRequest(r) { const v = { id: S.nextRequest++, params: [], ...stamp, ...r }; S.requests.push(v); return v; }
  const ptz = addRequest({
    name: "Front cam: go to PTZ preset", description: "Axis VAPIX ptz.cgi on the front camera",
    params: [{ name: "preset", description: "Server preset name, e.g. Driveway" }, { name: "speed", default: "5", description: "Pan/tilt speed 1–100" }],
    request: { type: "http", method: "GET", url: "http://192.168.1.50/axis-cgi/com/ptz.cgi?gotoserverpresetname={{ urlquery .params.preset }}&speed={{ .params.speed }}&camera=1", auth: "digest", username: "root", password: '{{ secret "FRONT_CAM_PASS" }}', timeoutSeconds: 10 },
  });
  const light = addRequest({
    name: "Home hub: turn on a light", description: "Home Assistant light.turn_on service",
    params: [{ name: "entity", default: "light.porch", description: "Light entity ID" }],
    request: { type: "http", method: "POST", url: "http://homeassistant.local:8123/api/services/light/turn_on", headers: { Authorization: 'Bearer {{ secret "HOME_HUB_TOKEN" }}' }, body: '{"entity_id": {{ json .params.entity }}}', timeoutSeconds: 10 },
  });
  const alert = addRequest({
    name: "Security channel alert", description: "Posts to the #security Discord channel",
    params: [{ name: "headline", default: "Heads up", description: "Bold first line" }],
    request: { type: "discord", webhookUrl: "", message: "**{{ .params.headline }}**: {{ .event.eventDescription }} on {{ .data.deviceName }} ({{ .data.siteName }})", discord: { username: "YourSix", silent: false } },
  });
  addRequest({ name: "Camera settle time", description: "Wait for a PTZ move to finish", request: { type: "delay", delayMillis: 8000 } });

  function addSchedule(s) { const v = { id: S.nextSchedule++, description: "", timezone: "America/Chicago", ...stamp, ...s }; S.schedules.push(v); return v; }
  const weeknights = addSchedule({ name: "Weeknights", description: "After hours on work days", windows: [{ days: ["mon", "tue", "wed", "thu", "fri"], from: "22:00", to: "06:00" }] });
  addSchedule({ name: "Business hours", windows: [{ days: ["mon", "tue", "wed", "thu", "fri"], from: "08:00", to: "17:00" }, { days: ["sat"], from: "09:00", to: "12:00" }] });

  function addRule(r) {
    const v = { id: S.nextRule++, enabled: true, cooldownSeconds: 0, cooldownFrom: "start", skipIfRunning: false, ...stamp, ...r };
    v.trigger = { topics: [], codes: [], filters: [], ...(r.trigger || {}) };
    S.rules.push(v);
    return v;
  }
  addRule({
    name: "CAM-01 motion → pulse output + PTZ preset", cooldownSeconds: 30,
    trigger: { topics: ["device"], codes: ["vmd", "aoa"], filters: [{ field: "deviceName", op: "equals", value: "CAM-01" }] },
    actions: [
      { type: "http", name: "Pulse output port 1 (VAPIX)", method: "GET", url: "http://192.168.1.50/axis-cgi/io/port.cgi?action=2:/", auth: "digest", username: "root", password: '{{ secret "FRONT_CAM_PASS" }}' },
      { type: "delay", name: "Let the camera settle", delayMillis: 12000 },
      { type: "request", requestId: ptz.id, params: { preset: "Driveway" } },
      { type: "http", name: "Notify home hub", method: "POST", url: "http://homeassistant.local:8123/api/webhook/yoursix", body: '{"source": {{ json .data.deviceName }}, "event": {{ json .event.key }}}', continueOnError: true },
    ],
    onFailure: [
      { type: "request", requestId: alert.id, params: { headline: "⚠️ {{ .rule }}: {{ .failure.step }} failed" } },
    ],
  });
  addRule({
    name: "Front door opened → porch light on", skipIfRunning: true,
    trigger: { topics: ["peripheral"], codes: ["input_activated"], filters: [{ field: "peripheralName", op: "equals", value: "Door contact" }] },
    actions: [
      { type: "request", requestId: light.id, params: { entity: "light.porch" } },
      { type: "delay", name: "Keep it on", delayMillis: 5000 },
      { type: "discord", name: "Tell the group", webhookUrl: "", message: "🚪 {{ .data.peripheralName }} opened at {{ .data.siteName }}" },
    ],
  });
  addRule({
    name: "Camera offline → Discord alert", cooldownSeconds: 900,
    trigger: { topics: ["device"], codes: ["disconnected"] },
    actions: [{ type: "request", requestId: alert.id, params: { headline: "⚠️ Camera offline" } }],
  });
  addRule({
    name: "West entrance armed → siren output", enabled: false, cooldownSeconds: 300, cooldownFrom: "finish",
    scheduleId: weeknights.id,
    trigger: { topics: ["alarmgroup"], codes: ["armed"], filters: [{ field: "groupName", op: "contains", value: "west" }] },
    actions: [{ type: "http", name: "Siren output on (VAPIX)", url: "http://192.168.1.51/axis-cgi/io/port.cgi?action=1:/", auth: "digest", username: "root", password: '{{ secret "SIREN_PASS" }}' }],
  });

  // ---------- edit history ----------
  // Like the server: snapshots hold an item's editable content only, with
  // sorted keys, and saves that change nothing aren't logged.
  const VOLATILE = ["id", "createdAt", "updatedAt", "updatedBy", "updatedByName", "lastFiredAt", "lastFinishedAt", "running", "activeRuns", "usedBy"];
  function sortKeys(v) {
    if (Array.isArray(v)) return v.map(sortKeys);
    if (v && typeof v === "object") return Object.fromEntries(Object.keys(v).sort().map((k) => [k, sortKeys(v[k])]));
    return v;
  }
  // Credentials typed into an action (not {{ secret }}) are hidden, like
  // store.redact.
  const CREDENTIAL_HEADERS = ["authorization", "proxy-authorization", "x-api-key"];
  const literal = (s) => typeof s === "string" && s !== "" && !s.includes("{{");
  function redact(v) {
    if (Array.isArray(v)) return v.forEach(redact);
    if (!v || typeof v !== "object") return;
    for (const [k, val] of Object.entries(v)) {
      if (k === "password" && literal(val)) v[k] = "‹hidden›";
      else if (k === "headers" && val && typeof val === "object") {
        for (const [name, hv] of Object.entries(val)) if (CREDENTIAL_HEADERS.includes(name.toLowerCase()) && literal(hv)) val[name] = "‹hidden›";
      } else redact(val);
    }
  }
  function snapshot(item) {
    const o = clone(item);
    VOLATILE.forEach((k) => delete o[k]);
    redact(o);
    return sortKeys(o);
  }
  function record(kind, item, change, by = ME, at = iso()) {
    const snap = snapshot(item);
    const last = S.history.findLast((r) => r.kind === kind && r.itemId === item.id);
    if (change === "updated" && last && JSON.stringify(last.snapshot) === JSON.stringify(snap)) return;
    S.history.push({ id: S.nextRev++, kind, itemId: item.id, name: item.name, change, snapshot: snap, byId: by.id, byName: by.displayName, at });
  }
  // Seed history: everything created three days ago, and the first rule
  // tuned since by someone else, so there's a diff to look at.
  const ALEX = { id: "alex", displayName: "Alex (ops)" };
  for (const sr of S.requests) record("action", sr, "created", ME, stamp.createdAt);
  for (const sc of S.schedules) record("schedule", sc, "created", ME, stamp.createdAt);
  for (const r of S.rules) {
    if (r.id !== 1) { record("rule", r, "created", ME, stamp.createdAt); continue; }
    const first = clone(r);
    first.cooldownSeconds = 60;
    first.actions = first.actions.filter((a) => a.type !== "request").map((a) => (a.type === "delay" ? { ...a, delayMillis: 8000 } : a));
    record("rule", first, "created", ALEX, iso(now() - 86400e3 * 3));
    record("rule", r, "updated", ME, iso(now() - 3600e3));
  }

  // ---------- templates (a small subset of Go text/template) ----------
  function lookup(path, data) {
    let v = data;
    for (const k of path.replace(/^\./, "").split(".")) {
      if (!k) continue;
      v = v == null ? undefined : v[k];
    }
    return v;
  }
  function evalExpr(expr, data, secretsUsed) {
    const parts = expr.split("|").map((p) => p.trim());
    const term = (t) => {
      const m = t.match(/^(\w+)\s+(.+)$/);
      if (m && ["secret", "json", "urlquery", "lower", "upper"].includes(m[1])) {
        const arg = term(m[2]);
        return applyFn(m[1], arg, secretsUsed);
      }
      if (/^".*"$/.test(t)) return t.slice(1, -1);
      if (t.startsWith(".")) return lookup(t, data);
      return t;
    };
    let v = term(parts[0]);
    for (const fn of parts.slice(1)) v = applyFn(fn, v, secretsUsed);
    return v;
  }
  function applyFn(fn, v, secretsUsed) {
    switch (fn) {
      case "secret": secretsUsed.add(String(v)); return `‹secret ${v}›`; // values never leave the server; masked here too
      case "json": return JSON.stringify(v ?? null);
      case "urlquery": return encodeURIComponent(v ?? "");
      case "lower": return String(v ?? "").toLowerCase();
      case "upper": return String(v ?? "").toUpperCase();
    }
    return v;
  }
  function render(s, data, secretsUsed = new Set()) {
    if (!s || !s.includes("{{")) return s || "";
    return s.replace(/\{\{\s*(.*?)\s*\}\}/g, (_, expr) => {
      const v = evalExpr(expr, data, secretsUsed);
      return v === undefined || v === null ? "" : typeof v === "object" ? JSON.stringify(v) : String(v);
    });
  }

  // ---------- matching (mirrors internal/rules) ----------
  function field(ev, name) {
    const top = { eventId: ev.eventId, eventOccurred: ev.eventOccurred, eventCode: ev.eventCode, eventTopic: ev.eventTopic, eventDescription: ev.eventDescription };
    if (name in top) return [top[name], true];
    const k = name.replace(/^data\./, "");
    return [ev.eventData[k], k in ev.eventData];
  }
  function matches(t, ev) {
    const topic = ev.eventTopic.toLowerCase(), code = ev.eventCode.toLowerCase();
    if (t.topics?.length && !t.topics.includes(topic)) return false;
    if (t.codes?.length && !t.codes.includes(code) && !t.codes.includes(`${topic}:${code}`)) return false;
    for (const f of t.filters || []) {
      const [raw, ok] = field(ev, f.field);
      if (f.op === "exists") { if (!(ok && raw != null)) return false; continue; }
      if (f.op === "not_exists") { if (ok && raw != null) return false; continue; }
      const got = ok && raw != null ? String(raw).toLowerCase() : "";
      const want = String(f.value || "").toLowerCase();
      const pass = { equals: got === want, not_equals: got !== want, contains: got.includes(want), starts_with: got.startsWith(want) }[f.op];
      if (f.op === "regex") { try { if (!new RegExp(f.value).test(ok ? String(raw) : "")) return false; } catch { return false; } continue; }
      if (!pass) return false;
    }
    return true;
  }

  // ---------- events and runs ----------
  function matrixUrl(d, at) {
    const q = new URLSearchParams();
    if (d.deviceId) q.set("deviceIds", d.deviceId + (d.videoSourceId ? `_${d.videoSourceId}` : ""));
    else if (d.groupId) q.set("alarmGroupId", d.groupId);
    else return "";
    q.set("timestamp", (at / 1000).toFixed(3));
    return `${PLATFORM}/matrix?${q}`;
  }

  function runView(run) {
    const v = { ...run, steps: clone(run.steps) };
    delete v._rule;
    if (run.status === "running" && run.current) {
      const c = { ...run.current };
      c.elapsedMs = Math.max(0, now() - Date.parse(c.startedAt));
      if (c.until) c.remainingMs = Math.max(0, Date.parse(c.until) - now());
      v.current = c;
    } else delete v.current;
    return v;
  }
  function eventView(e, withPayload) {
    const d = e.payload.eventData || {};
    const v = {
      id: e.id, eventId: e.payload.eventId || "", topic: e.payload.eventTopic, code: e.payload.eventCode,
      description: e.payload.eventDescription, occurred: e.payload.eventOccurred, receivedAt: e.receivedAt,
      sourceIp: e.sourceIp, test: e.test, runs: e.runs.map(runView),
      deviceId: d.deviceId, videoSourceId: d.videoSourceId, groupId: d.groupId,
      matrixUrl: matrixUrl(d, Date.parse(e.payload.eventOccurred) || Date.parse(e.receivedAt)),
    };
    if (withPayload) v.payload = e.payload;
    return v;
  }

  // Live stream: coalesce changes for 200ms like the server's hub.
  const streams = new Set();
  const pending = new Set();
  let flushTimer = null;
  function changed(e) {
    pending.add(e.id);
    if (!flushTimer) {
      flushTimer = setTimeout(() => {
        flushTimer = null;
        for (const id of pending) {
          const ev = S.events.find((x) => x.id === id);
          if (!ev) continue;
          const msg = JSON.stringify(eventView(ev, false));
          for (const s of streams) s._emit("event", msg);
        }
        pending.clear();
      }, 200);
    }
  }

  function claim(r) {
    const t = now();
    if (r.skipIfRunning && (S.running[r.id] || 0) > 0) return "busy";
    if (r.cooldownSeconds > 0) {
      const win = r.cooldownSeconds * 1000;
      if (r.cooldownFrom === "finish") {
        if ((S.running[r.id] || 0) > 0) return "cooldown";
        if (S.lastFinished[r.id] && t - S.lastFinished[r.id] < win) return "cooldown";
      } else if (S.lastFired[r.id] && t - S.lastFired[r.id] < win) return "cooldown";
    }
    S.lastFired[r.id] = t;
    S.running[r.id] = (S.running[r.id] || 0) + 1;
    r.lastFiredAt = iso(t);
    return "";
  }

  function stepLabel(a) {
    if (a.name) return a.name;
    if (a.type === "request") return S.requests.find((x) => x.id === a.requestId)?.name || "Saved action";
    return { delay: "Delay", discord: "Discord message" }[a.type] || "HTTP request";
  }

  // simulateHTTP pretends to call a device: a short random latency, then 200 OK.
  async function simulateHTTP(a, data, res) {
    const used = new Set();
    const url = render(a.url, data, used);
    res.target = `${(a.method || "GET").toUpperCase()} ${url}`;
    render(a.body, data, used);
    Object.values(a.headers || {}).forEach((h) => render(h, data, used));
    render(a.password, data, used);
    const missing = [...used].filter((n) => !S.secrets.some((s) => s.name === n));
    if (missing.length) throw new Error(`secret "${missing[0]}" is not available to this rule`);
    await sleep(120 + Math.random() * 380);
    // Lets the demo show failures (and on-failure actions): any host with
    // "offline" in it doesn't answer.
    if (/^\w+:\/\/[^/]*offline/i.test(url)) throw new Error(`dial tcp: connect: connection refused (${url.split("/")[2]} is offline in this demo)`);
    res.status = 200;
    res.response = "OK";
  }

  // runStep runs one plain (non-saved) action.
  async function runStep(a, data, res) {
    if (a.type === "delay") return sleep(a.delayMillis);
    if (a.type === "discord") {
      res.target = "discord webhook";
      render(a.message, data);
      render(a.webhookUrl, data);
      await sleep(150 + Math.random() * 200);
      res.status = 204;
      return;
    }
    return simulateHTTP(a, data, res);
  }

  // runActions runs a rule's actions, then its on-failure actions if a step
  // failed (appended to the run's steps, marked onFailure).
  async function runActions(e, run, rule) {
    const data = {
      event: { ...e.payload, key: `${e.payload.eventTopic}:${e.payload.eventCode}` },
      data: e.payload.eventData, rule: rule.name,
    };
    const ok = await runList(e, run, rule.actions, data, 0, false, rule);
    // Like engine.FailureData: failures of continue-on-error steps were
    // chosen to be ignored and don't trigger on-failure actions.
    const f = run.steps.find((s) => !s.ok && !s.skipped && s.error && !rule.actions[s.index]?.continueOnError);
    if (!ok && f && rule.onFailure?.length) {
      data.failure = { step: f.name || f.type, stepNumber: f.index + 1, error: f.error };
      await runList(e, run, rule.onFailure, data, rule.actions.length, true);
    }
    run.status = ok ? "ok" : "failed";
    run.finishedAt = iso();
    delete run.current;
  }

  // watchedDelay mirrors actions.watchedDelay: wait, reacting to matching
  // events (passed in by dispatch through S.watchers) as a.watch says.
  async function watchedDelay(e, run, a, data, res, rule) {
    const w = a.watch, start = now();
    let until = start + a.delayMillis, last = null, failed = false;
    const limit = w.restart ? start + w.maxMillis : until;
    const out = res.watch = { matches: 0 };
    const own = (w.trigger?.filters || []).map((f) => ({ ...f, value: render(f.value, data) }));
    const t = w.sameAsTrigger
      ? { topics: rule.trigger.topics, codes: rule.trigger.codes, filters: [...(rule.trigger.filters || []), ...own] }
      : { topics: w.trigger?.topics || [], codes: w.trigger?.codes || [], filters: own };
    const queue = [];
    let wake = null;
    const sub = { trigger: t, push: (p) => { queue.push(p); wake?.(); } };
    const publish = () => {
      data.watch = { matches: out.matches, restarts: out.restarts || 0, stopped: !!out.stopped, waitedMs: now() - start };
      if (last) Object.assign(data.watch, { event: { ...last, key: `${last.eventTopic}:${last.eventCode}` }, data: last.eventData });
    };
    const runActs = async (acts, match) => {
      const inner = { steps: [] };
      if (!(await runList(null, inner, acts, data, 0))) failed = true;
      out.steps = (out.steps || []).concat(inner.steps.map((s) => ({ ...s, match })));
    };
    S.watchers.add(sub);
    try {
      for (;;) {
        if (!queue.length) {
          const left = until - now();
          if (left <= 0) break;
          await new Promise((r) => { wake = r; setTimeout(r, left); });
          wake = null;
          continue;
        }
        last = queue.shift();
        out.matches++;
        if (w.restart) {
          const next = Math.min(now() + a.delayMillis, limit);
          if (next > until) { until = next; out.restarts = (out.restarts || 0) + 1; run.current.startedAt = iso(); }
        }
        if (w.stop) out.stopped = true;
        publish();
        Object.assign(run.current, { until: iso(until), matches: out.matches, restarts: out.restarts });
        changed(e);
        if (w.actions?.length && (w.everyMatch || out.matches === 1)) await runActs(w.actions, out.matches);
        if (w.stop) return { stop: true, failed };
      }
    } finally {
      S.watchers.delete(sub);
    }
    publish();
    if (!out.matches && w.ifNoMatch?.length) await runActs(w.ifNoMatch, 0);
    return { stop: false, failed };
  }

  // runList runs actions, recording each step in run.steps. With e null
  // (a watch's actions), nothing is pushed to the live view.
  async function runList(e, run, list, data, offset, onFailure = false, rule = null) {
    let stopped = false, ok = true;
    for (const [n, a] of list.entries()) {
      const i = n + offset;
      const res = { index: i, type: a.type, name: a.name || "", ...(onFailure ? { onFailure: true } : {}) };
      if (stopped) { res.skipped = true; run.steps.push(res); continue; }
      const start = now();
      run.current = { index: i, type: a.type, name: stepLabel(a), startedAt: iso(start) };
      const saved = a.type === "request" ? S.requests.find((x) => x.id === a.requestId) : null;
      const waitMs = a.type === "delay" ? a.delayMillis : saved?.request.type === "delay" ? saved.request.delayMillis : 0;
      if (waitMs) run.current.until = iso(start + waitMs);
      if (e) changed(e);
      try {
        if (a.type === "delay" && a.watch && rule) {
          const { stop, failed } = await watchedDelay(e, run, a, data, res, rule);
          // The watch's own failures don't fail this step or stop the rule.
          if (failed) ok = false;
          if (stop) stopped = true;
        } else if (a.type === "request") {
          const sr = saved;
          if (!sr) throw new Error(`saved action #${a.requestId}: not found`);
          if (!res.name) res.name = sr.name;
          const params = {};
          for (const p of sr.params) params[p.name] = p.default || "";
          for (const [k, v] of Object.entries(a.params || {})) if (v !== "") params[k] = render(v, data);
          const need = sr.params.find((p) => !p.default && !params[p.name]);
          if (need) throw new Error(`saved action "${sr.name}" needs a value for "${need.name}"`);
          await runStep(sr.request, { ...data, params }, res);
        } else {
          await runStep(a, data, res);
        }
        res.ok = true;
      } catch (err) {
        res.error = err.message;
        ok = false;
        if (!a.continueOnError) stopped = true;
      }
      res.durationMs = now() - start;
      run.steps.push(res);
      if (e) changed(e);
    }
    return ok;
  }

  function startRun(e, rule, { test = false } = {}) {
    const run = { id: S.nextRun++, eventRow: e.id, ruleId: rule.id, ruleName: rule.name, status: "running", startedAt: iso(), steps: [], totalSteps: rule.actions.length };
    e.runs.push(run);
    changed(e);
    runActions(e, run, rule).finally(() => {
      if (!test) {
        S.running[rule.id] = Math.max(0, (S.running[rule.id] || 1) - 1);
        S.lastFinished[rule.id] = now();
        rule.lastFinishedAt = iso();
      }
      changed(e);
    });
    return run;
  }

  function insertEvent(payload, { test = false, sourceIp = "44.198.142.131", at = now() } = {}) {
    if (payload.eventId && S.events.some((x) => x.payload.eventId === payload.eventId)) return null;
    const e = { id: S.nextEvent++, payload, receivedAt: iso(at), sourceIp, test, runs: [] };
    S.events.unshift(e);
    return e;
  }

  // Schedules are checked with scheduleActive from logic.js (loaded by the
  // page), the same code the UI uses, at the event's own time like
  // engine.eventTime: eventOccurred, or now if it's missing or in the future.
  function eventTime(payload) {
    const t = Date.parse(payload.eventOccurred);
    return new Date(Number.isFinite(t) && t <= now() ? t : now());
  }

  function dispatch(payload, opts) {
    const e = insertEvent(payload, opts);
    if (!e) return { eventRow: 0, duplicate: true, matchedRules: [] };
    // Like engine.notifyWatchers: watched delays see the event before rules do.
    for (const w of S.watchers) if (matches(w.trigger, payload)) w.push(payload);
    const matched = [];
    for (const r of S.rules.filter((x) => x.enabled)) {
      if (!matches(r.trigger, payload)) continue;
      matched.push(r.id);
      const sched = r.scheduleId && S.schedules.find((s) => s.id === r.scheduleId);
      const skip = sched && !scheduleActive(sched, eventTime(payload)) ? "offhours" : claim(r);
      if (skip) e.runs.push({ id: S.nextRun++, eventRow: e.id, ruleId: r.id, ruleName: r.name, status: skip, startedAt: iso(), finishedAt: iso(), steps: [], totalSteps: r.actions.length });
      else startRun(e, r);
    }
    changed(e);
    return { eventRow: e.id, duplicate: false, matchedRules: matched };
  }

  // ---------- seed history (finished runs, minutes ago) ----------
  (function seedHistory() {
    const past = (min) => now() - min * 60e3;
    const done = (e, rule, status, steps) => e.runs.push({ id: S.nextRun++, eventRow: e.id, ruleId: rule.id, ruleName: rule.name, status, startedAt: e.receivedAt, finishedAt: e.receivedAt, steps, totalSteps: rule.actions.length });
    const ok = (i, type, name, extra = {}) => ({ index: i, type, name, ok: true, durationMs: 180, ...extra });
    const [cam, porch, offline] = S.rules;
    insertEvent(sample("device", "connected", { at: past(42) }), { at: past(42) });
    insertEvent(sample("sensor", "outside_range", { at: past(37) }), { at: past(37) });
    let e = insertEvent(sample("device", "disconnected", { device: "CAM-02", at: past(31) }), { at: past(31) });
    done(e, offline, "ok", [ok(0, "discord", "Alert", { status: 204, target: "discord webhook" })]);
    e = insertEvent(sample("device", "disconnected", { device: "CAM-02", at: past(29) }), { at: past(29) });
    done(e, offline, "cooldown", []);
    e = insertEvent(sample("peripheral", "input_activated", { at: past(18) }), { at: past(18) });
    done(e, porch, "ok", [
      ok(0, "request", "Home hub: turn on a light", { status: 200, target: "POST http://homeassistant.local:8123/api/services/light/turn_on", response: "OK" }),
      ok(1, "delay", "Keep it on", { durationMs: 5000 }),
      ok(2, "discord", "Tell the group", { status: 204, target: "discord webhook" }),
    ]);
    e = insertEvent(sample("peripheral", "input_activated", { at: past(18) + 900 }), { at: past(18) + 900 });
    done(e, porch, "busy", []);
    insertEvent(sample("alarmgroup", "armed", { at: past(9) }), { at: past(9) });
    e = insertEvent(sample("device", "vmd", { at: past(4) }), { at: past(4) });
    done(e, cam, "ok", [
      ok(0, "http", "Pulse output port 1 (VAPIX)", { status: 200, target: "GET http://192.168.1.50/axis-cgi/io/port.cgi?action=2:/", response: "OK" }),
      ok(1, "delay", "Let the camera settle", { durationMs: 12000 }),
      ok(2, "request", "Front cam: go to PTZ preset", { status: 200, target: "GET http://192.168.1.50/axis-cgi/com/ptz.cgi?gotoserverpresetname=Driveway&speed=5&camera=1", response: "OK" }),
      ok(3, "http", "Notify home hub", { status: 200, target: "POST http://homeassistant.local:8123/api/webhook/yoursix", response: "OK" }),
    ]);
    cam.lastFiredAt = iso(past(4)); porch.lastFiredAt = iso(past(18)); offline.lastFiredAt = iso(past(31));
  })();

  // Simulated YourSix traffic so the demo stays lively.
  const SCRIPT = [
    () => sample("device", "vmd"),
    () => sample("peripheral", "input_activated"),
    () => sample("device", "connected", { device: "CAM-02" }),
    () => sample("device", "aoa"),
    () => sample("sensor", "outside_range"),
    () => sample("device", "disconnected", { device: "CAM-02" }),
  ];
  let scriptPos = 0;
  setTimeout(function tick() {
    dispatch(SCRIPT[scriptPos++ % SCRIPT.length]());
    setTimeout(tick, 20000 + Math.random() * 10000);
  }, 4000);

  // ---------- API ----------
  const json = (status, body) => new Response(body === undefined ? null : JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
  const err = (status, msg) => json(status, { error: msg });

  function ruleView(r) {
    const active = [];
    for (const e of S.events) for (const run of e.runs) if (run.ruleId === r.id && run.status === "running") active.push(runView(run));
    const v = clone(r);
    v.running = active.length;
    if (active.length) v.activeRuns = active;
    return v;
  }
  function requestView(sr) {
    return { ...clone(sr), usedBy: S.rules.filter((r) => r.actions.some((a) => a.type === "request" && a.requestId === sr.id)).map((r) => ({ id: r.id, name: r.name })) };
  }
  function scheduleView(sc) {
    return { ...clone(sc), usedBy: S.rules.filter((r) => r.scheduleId === sc.id).map((r) => ({ id: r.id, name: r.name })) };
  }
  function validateSchedule(s) {
    if (!String(s.name || "").trim()) return "name is required";
    if (!s.windows?.length) return "add at least one time window";
    for (const [i, w] of s.windows.entries()) {
      if (!w.from !== !w.to) return `window ${i + 1}: set both a start and an end time, or neither`;
      if (w.from && w.from === w.to) return `window ${i + 1}: start and end are the same time; clear both times for all day`;
    }
    return "";
  }
  function validateSaved(body) {
    if (!String(body.name || "").trim()) return "name is required";
    const a = body.request || {};
    if (!["http", "delay", "discord", undefined].includes(a.type)) return `a saved action can't be of type "${a.type}"`;
    if (a.watch) return "a saved delay can't watch for events; add the watch to a delay in the rule";
    return validateRule({ name: "x", actions: [{ ...a, type: a.type || "http" }] }).replace(/^action 1: /, "");
  }
  function validateRule(r) {
    if (!String(r.name || "").trim()) return "name is required";
    if (!r.actions || !r.actions.length) return "at least one action is required";
    if (r.scheduleId && !S.schedules.some((s) => s.id === r.scheduleId)) return `schedule #${r.scheduleId} doesn't exist`;
    const bad = validateActions(r.actions, "action", true);
    if (bad) return bad;
    if (r.onFailure?.some((a) => a.watch)) return `on-failure action ${r.onFailure.findIndex((a) => a.watch) + 1}: only the rule's own delays can watch for events`;
    return validateActions(r.onFailure || [], "on-failure action", false);
  }
  function validateActions(list, label, watchable) {
    for (const [i, a] of list.entries()) {
      const at = `${label} ${i + 1}`;
      if (a.watch && a.type !== "delay") return `${at}: only delays can watch for events`;
      if (a.type === "http" && !a.url) return `${at}: url is required`;
      if (a.type === "delay" && !(a.delayMillis > 0 && a.delayMillis <= 600000)) return `${at}: delayMillis must be between 1 and 600000`;
      if (a.type === "discord" && !a.message && !a.discord?.embeds?.length) return `${at}: a message or an embed is required`;
      if (a.type === "request") {
        const sr = S.requests.find((x) => x.id === a.requestId);
        if (!sr) return `${at}: choose a saved action`;
        const need = sr.params.find((p) => !p.default && !(a.params || {})[p.name]);
        if (need) return `${at}: saved action "${sr.name}" needs a value for "${need.name}"`;
      }
      if (a.watch) {
        const bad = validateWatch(a.watch, a.delayMillis, watchable);
        if (bad) return `${at}: ${bad}`;
      }
    }
    return "";
  }
  // validateWatch mirrors rules.Watch.validate, normalising w in place.
  function validateWatch(w, delayMillis, watchable) {
    if (!watchable) return "actions that run while waiting can't watch for events themselves";
    const t = w.trigger = { topics: [], codes: [], filters: [], ...(w.trigger || {}) };
    if (w.sameAsTrigger) { t.topics = []; t.codes = []; }
    else if (!t.topics.length && !t.codes.length && !t.filters.length) return "watch: pick the events to watch for, or use the rule's trigger";
    if (!w.restart && !w.stop && !w.actions?.length && !w.ifNoMatch?.length) return "watch: choose what happens (restart the countdown, run actions, stop the rule, or run actions if nothing matches)";
    if (w.restart && !(w.maxMillis >= delayMillis && w.maxMillis <= 3600000)) return "watch: the longest wait must be between the delay and 3600000 ms (1 hour)";
    if (!w.restart) delete w.maxMillis;
    if (!w.actions?.length) delete w.everyMatch;
    for (const [name, list] of [["on-match action", w.actions || []], ["if-no-match action", w.ifNoMatch || []]]) {
      const bad = validateActions(list, `watch: ${name}`, false);
      if (bad) return bad;
    }
    return "";
  }
  function conflict(what, item, current) {
    return json(409, { error: `${item.updatedByName || "Someone else"} changed this ${what} at ${item.updatedAt}, after you opened it`, conflict: true, current });
  }

  // previewAction mirrors actions.Runner.Preview.
  function previewAction(a, data) {
    const out = [];
    const add = (field, t) => { if (t) out.push({ field, value: render(t, data) }); };
    if (a.type === "delay") {
      (a.watch?.trigger?.filters || []).forEach((f) => { if (String(f.value || "").includes("{{")) add(`Watch filter ${f.field}`, f.value); });
    } else if (a.type === "http") {
      add("URL", a.url);
      Object.keys(a.headers || {}).sort().forEach((k) => add(`Header ${k}`, a.headers[k]));
      add("Body", a.body);
      add("Password", a.password);
    } else if (a.type === "discord") {
      add("Webhook URL", a.webhookUrl);
      add("Message", a.message);
      const d = a.discord || {};
      add("Username", d.username);
      add("Avatar URL", d.avatarUrl);
      add("Thread name", d.threadName);
      (d.embeds || []).forEach((e, i) => {
        const p = `Embed ${i + 1} `;
        add(p + "title", e.title); add(p + "link", e.url); add(p + "description", e.description); add(p + "timestamp", e.timestamp);
        add(p + "author", e.authorName); add(p + "footer", e.footerText); add(p + "image", e.imageUrl); add(p + "thumbnail", e.thumbnailUrl);
        (e.fields || []).forEach((f) => add(`${p}field ${f.name}`, f.value));
      });
    } else if (a.type === "request") {
      const sr = S.requests.find((x) => x.id === a.requestId);
      if (!sr) return [{ field: "Saved action", error: `saved action #${a.requestId}: not found` }];
      const params = {};
      for (const p of sr.params) params[p.name] = p.default || "";
      for (const [k, v] of Object.entries(a.params || {})) if (v !== "") params[k] = render(v, data);
      for (const p of sr.params) out.push({ field: `Parameter ${p.name}`, value: params[p.name] });
      return out.concat(previewAction(sr.request, { ...data, params }));
    }
    return out;
  }

  // Event search, like internal/store: free words and "phrases" match
  // anywhere; key:value matches one field. "http://…" is free text.
  function parseSearch(s) {
    const out = [];
    const re = /(?:([A-Za-z_]\w*):(?=[^\s/]))?(?:"([^"]*)"?|(\S+))/g;
    for (const m of s.matchAll(re)) {
      const value = (m[2] ?? m[3] ?? "").trim();
      if (value) out.push({ key: m[1] || "", value: value.toLowerCase() });
    }
    return out;
  }
  function termMatches(t, e) {
    const has = (v) => String(v ?? "").toLowerCase().includes(t.value);
    const d = e.payload.eventData || {};
    const key = t.key.toLowerCase();
    switch (key) {
      case "": return has([`${e.payload.eventTopic}:${e.payload.eventCode}`, e.payload.eventDescription, e.sourceIp, JSON.stringify(e.payload), ...e.runs.map((r) => r.ruleName)].join("\n"));
      case "topic": return has(e.payload.eventTopic);
      case "code": return has(e.payload.eventCode);
      case "rule": return e.runs.some((r) => has(r.ruleName));
      case "status": return e.runs.some((r) => r.status === t.value);
      case "ip": return has(e.sourceIp);
    }
    const field = { device: "deviceName", site: "siteName", group: "groupName", peripheral: "peripheralName" }[key] || t.key;
    return has(d[field]) || `${e.payload.eventTopic}:${e.payload.eventCode}`.includes(`${key}:${t.value}`);
  }

  function normalizeRule(body, base = {}) {
    const r = { ...base, ...body };
    r.name = String(r.name || "").trim();
    r.trigger = { topics: [], codes: [], filters: [], ...(r.trigger || {}) };
    r.cooldownFrom = r.cooldownFrom || "start";
    r.updatedAt = iso(); r.updatedBy = ME.id; r.updatedByName = ME.displayName;
    return r;
  }

  const routes = [
    ["GET", /^\/api\/me$/, () => json(200, { user: ME, version: "demo", devAuth: false })],
    ["GET", /^\/api\/catalog$/, () => json(200, { topics: Object.keys(CATALOG).sort(), codes: CATALOG, fields: FIELDS })],
    ["GET", /^\/api\/info$/, () => json(200, { webhookUrl: "https://y6.example.com/hook/3f9c2b7e8a41d6c0e5b9a27f4d18c63e", requireUserAgent: true, verifySourceIp: true, clientIpHeader: "CF-Connecting-IP", retentionDays: 30, secretsEnabled: true })],

    ["GET", /^\/api\/rules$/, () => json(200, [...S.rules].sort((a, b) => a.name.localeCompare(b.name)).map(ruleView))],
    ["POST", /^\/api\/rules$/, (_, body) => {
      const r = normalizeRule(body, { id: S.nextRule, createdAt: iso() });
      const bad = validateRule(r);
      if (bad) return err(400, bad);
      S.nextRule++; S.rules.push(r);
      record("rule", r, "created");
      return json(201, ruleView(r));
    }],
    ["GET", /^\/api\/rules\/(\d+)$/, (m) => { const r = S.rules.find((x) => x.id === +m[1]); return r ? json(200, ruleView(r)) : err(404, "not found"); }],
    ["PUT", /^\/api\/rules\/(\d+)$/, (m, body) => {
      const i = S.rules.findIndex((x) => x.id === +m[1]);
      if (i < 0) return err(404, "not found");
      if (body.updatedAt && body.updatedAt !== S.rules[i].updatedAt) return conflict("rule", S.rules[i], ruleView(S.rules[i]));
      const r = normalizeRule(body, { id: S.rules[i].id, createdAt: S.rules[i].createdAt, lastFiredAt: S.rules[i].lastFiredAt });
      const bad = validateRule(r);
      if (bad) return err(400, bad);
      S.rules[i] = r;
      record("rule", r, "updated");
      return json(200, ruleView(r));
    }],
    ["DELETE", /^\/api\/rules\/(\d+)$/, (m) => {
      const r = S.rules.find((x) => x.id === +m[1]);
      if (!r) return err(404, "not found");
      S.rules = S.rules.filter((x) => x !== r);
      record("rule", r, "deleted");
      return json(204);
    }],
    ["POST", /^\/api\/rules\/(\d+)\/test$/, (m) => {
      const r = S.rules.find((x) => x.id === +m[1]);
      if (!r) return err(404, "not found");
      const ev = sample(r.trigger.topics[0] || "device", r.trigger.codes[0] || "vmd");
      ev.eventId = "";
      const e = insertEvent(ev, { test: true, sourceIp: "test:demo" });
      const run = startRun(e, r, { test: true });
      changed(e);
      return json(202, { run: runView(run), event: ev, matchesTrigger: matches(r.trigger, ev) });
    }],

    ["GET", /^\/api\/runs\/(\d+)$/, (m) => {
      for (const e of S.events) for (const run of e.runs) if (run.id === +m[1]) return json(200, runView(run));
      return err(404, "not found");
    }],

    ["GET", /^\/api\/schedules$/, () => json(200, [...S.schedules].sort((a, b) => a.name.localeCompare(b.name)).map(scheduleView))],
    ["POST", /^\/api\/schedules$/, (_, body) => {
      const bad = validateSchedule(body);
      if (bad) return err(400, bad);
      const sc = addSchedule({ ...body, timezone: body.timezone || "America/Chicago", createdAt: iso(), updatedAt: iso() });
      record("schedule", sc, "created");
      return json(201, scheduleView(sc));
    }],
    ["GET", /^\/api\/schedules\/(\d+)$/, (m) => { const sc = S.schedules.find((x) => x.id === +m[1]); return sc ? json(200, scheduleView(sc)) : err(404, "not found"); }],
    ["PUT", /^\/api\/schedules\/(\d+)$/, (m, body) => {
      const i = S.schedules.findIndex((x) => x.id === +m[1]);
      if (i < 0) return err(404, "not found");
      const bad = validateSchedule(body);
      if (bad) return err(400, bad);
      if (body.updatedAt && body.updatedAt !== S.schedules[i].updatedAt) return conflict("schedule", S.schedules[i], scheduleView(S.schedules[i]));
      S.schedules[i] = { ...S.schedules[i], ...body, timezone: body.timezone || "America/Chicago", id: S.schedules[i].id, updatedAt: iso(), updatedBy: ME.id, updatedByName: ME.displayName };
      record("schedule", S.schedules[i], "updated");
      return json(200, scheduleView(S.schedules[i]));
    }],
    ["DELETE", /^\/api\/schedules\/(\d+)$/, (m) => {
      const sc = S.schedules.find((x) => x.id === +m[1]);
      if (!sc) return err(404, "not found");
      const used = scheduleView(sc).usedBy;
      if (used.length) return json(409, { error: `still used by ${used.length} rule(s); remove it from them first`, usedBy: used });
      S.schedules = S.schedules.filter((x) => x !== sc);
      record("schedule", sc, "deleted");
      return json(204);
    }],

    ["GET", /^\/api\/requests$/, () => json(200, [...S.requests].sort((a, b) => a.name.localeCompare(b.name)).map(requestView))],
    ["POST", /^\/api\/requests$/, (_, body) => {
      const bad = validateSaved(body);
      if (bad) return err(400, bad);
      const sr = addRequest({ ...body, request: { ...body.request, type: body.request.type || "http" }, createdAt: iso(), updatedAt: iso() });
      record("action", sr, "created");
      return json(201, requestView(sr));
    }],
    ["GET", /^\/api\/requests\/(\d+)$/, (m) => { const sr = S.requests.find((x) => x.id === +m[1]); return sr ? json(200, requestView(sr)) : err(404, "not found"); }],
    ["PUT", /^\/api\/requests\/(\d+)$/, (m, body) => {
      const i = S.requests.findIndex((x) => x.id === +m[1]);
      if (i < 0) return err(404, "not found");
      const bad = validateSaved(body);
      if (bad) return err(400, bad);
      if (body.updatedAt && body.updatedAt !== S.requests[i].updatedAt) return conflict("saved action", S.requests[i], requestView(S.requests[i]));
      S.requests[i] = { ...S.requests[i], ...body, request: { ...body.request, type: body.request.type || "http" }, id: S.requests[i].id, updatedAt: iso(), updatedBy: ME.id, updatedByName: ME.displayName };
      record("action", S.requests[i], "updated");
      return json(200, requestView(S.requests[i]));
    }],
    ["DELETE", /^\/api\/requests\/(\d+)$/, (m) => {
      const used = requestView(S.requests.find((x) => x.id === +m[1]) || { id: -1 }).usedBy;
      if (used.length) return json(409, { error: `still used by ${used.length} rule(s): [${used.map((r) => r.name).join(" ")}]; remove it from them first`, usedBy: used });
      const sr = S.requests.find((x) => x.id === +m[1]);
      if (!sr) return err(404, "not found");
      S.requests = S.requests.filter((x) => x !== sr);
      record("action", sr, "deleted");
      return json(204);
    }],
    ["POST", /^\/api\/requests\/(\d+)\/test$/, (m, body) => {
      const sr = S.requests.find((x) => x.id === +m[1]);
      if (!sr) return err(404, "not found");
      const rule = { id: 0, name: `Saved action test: ${sr.name}`, actions: [{ type: "request", requestId: sr.id, params: body?.params || {} }] };
      const ev = sample("device", "vmd");
      ev.eventId = "";
      const e = insertEvent(ev, { test: true, sourceIp: "test:demo" });
      const run = startRun(e, rule, { test: true });
      return json(202, { run: runView(run), event: ev });
    }],

    ["GET", /^\/api\/events$/, (_, __, url) => {
      const limit = Math.min(+url.searchParams.get("limit") || 50, 200);
      const before = +url.searchParams.get("before") || 0;
      const outcome = url.searchParams.get("outcome") || "";
      const outcomes = {
        "": () => true, matched: (e) => e.runs.length > 0, nomatch: (e) => !e.runs.length, test: (e) => e.test,
        ok: (e) => e.runs.some((r) => r.status === "ok"), failed: (e) => e.runs.some((r) => r.status === "failed"),
        skipped: (e) => e.runs.some((r) => ["cooldown", "busy", "offhours"].includes(r.status)),
      };
      if (!outcomes[outcome]) return err(400, `unknown outcome "${outcome}"`);
      const since = Date.parse(url.searchParams.get("since") || "") || 0;
      const until = Date.parse(url.searchParams.get("until") || "") || Infinity;
      const terms = parseSearch(url.searchParams.get("q") || "");
      return json(200, S.events.filter((e) => (!before || e.id < before) && terms.every((t) => termMatches(t, e)) && outcomes[outcome](e)
        && Date.parse(e.receivedAt) >= since && Date.parse(e.receivedAt) < until).slice(0, limit).map((e) => eventView(e, false)));
    }],
    ["POST", /^\/api\/preview$/, (_, body) => {
      const r = body?.rule || {};
      const trigger = { topics: [], codes: [], filters: [], ...(r.trigger || {}) };
      let [topic, code] = [trigger.topics[0] || "device", trigger.codes[0] || "vmd"];
      if (code.includes(":")) [topic, code] = code.split(":");
      const ev = sample(topic, code);
      const data = { event: { ...ev, key: `${ev.eventTopic}:${ev.eventCode}` }, data: ev.eventData, rule: r.name };
      // Like engine.Preview: watch actions, and the steps after a watched
      // delay, see a sample .watch.
      const actions = [], watches = {};
      (r.actions || []).forEach((a, i) => {
        actions.push(previewAction(a, data));
        const w = a.watch;
        if (!w) return;
        const matched = { ...data, watch: { matches: 1, restarts: 0, stopped: !!w.stop, waitedMs: 0, event: data.event, data: data.data } };
        if (w.actions?.length || w.ifNoMatch?.length) {
          const quiet = { ...data, watch: { matches: 0, restarts: 0, stopped: false, waitedMs: a.delayMillis } };
          watches[i] = { actions: (w.actions || []).map((x) => previewAction(x, matched)), ifNoMatch: (w.ifNoMatch || []).map((x) => previewAction(x, quiet)) };
        }
        data.watch = matched.watch;
      });
      data.failure = { step: r.actions?.[0]?.name || "Action 1", stepNumber: 1, error: "sample error: connection refused" };
      return json(200, { event: ev, matchesTrigger: matches(trigger, ev), actions, onFailure: (r.onFailure || []).map((a) => previewAction(a, data)), watches });
    }],
    ["GET", /^\/api\/history$/, (_, __, url) => {
      const p = url.searchParams;
      const limit = Math.min(+p.get("limit") || 100, 500);
      return json(200, S.history.filter((r) => (!p.get("kind") || r.kind === p.get("kind")) && (!+p.get("id") || r.itemId === +p.get("id")) && (!+p.get("before") || r.id < +p.get("before")))
        .sort((a, b) => b.at.localeCompare(a.at) || b.id - a.id).slice(0, limit));
    }],
    ["GET", /^\/api\/events\/(\d+)$/, (m) => { const e = S.events.find((x) => x.id === +m[1]); return e ? json(200, eventView(e, true)) : err(404, "not found"); }],
    ["POST", /^\/api\/events\/(\d+)\/replay$/, (m) => {
      const e = S.events.find((x) => x.id === +m[1]);
      if (!e) return err(404, "not found");
      return json(200, dispatch({ ...clone(e.payload), eventId: "" }, { test: true, sourceIp: "simulate:demo" }));
    }],
    ["POST", /^\/api\/simulate$/, (_, body) => {
      if (!body?.eventTopic || !body?.eventCode) return err(400, "event is missing eventTopic or eventCode");
      return json(200, dispatch({ ...body, eventId: "", eventData: body.eventData || {} }, { test: true, sourceIp: "simulate:demo" }));
    }],

    ["GET", /^\/api\/secrets$/, () => json(200, { enabled: true, secrets: S.secrets.map((s) => ({ ...s, usedBy: [
      ...S.rules.filter((r) => JSON.stringify(r.actions).includes(`secret \\"${s.name}\\"`)).map((r) => r.name),
      ...S.requests.filter((sr) => JSON.stringify(sr.request).includes(`secret \\"${s.name}\\"`)).map((sr) => `Saved action: ${sr.name}`),
    ] })) })],
    ["PUT", /^\/api\/secrets\/([^/]+)$/, (m, body) => {
      const name = decodeURIComponent(m[1]).toUpperCase();
      if (!/^[A-Z][A-Z0-9_]{0,63}$/.test(name)) return err(400, "invalid secret: names must start with a letter and use only A-Z, 0-9 and _ (max 64)");
      if (!body?.value) return err(400, "invalid secret: value is required");
      const s = S.secrets.find((x) => x.name === name);
      if (s) s.updatedAt = iso(); else S.secrets.push({ name, createdAt: iso(), updatedAt: iso() });
      S.secrets.sort((a, b) => a.name.localeCompare(b.name));
      return json(200, { name }); // the value is discarded, as on the real server it's write-only
    }],
    ["DELETE", /^\/api\/secrets\/([^/]+)$/, (m) => { S.secrets = S.secrets.filter((x) => x.name !== decodeURIComponent(m[1]).toUpperCase()); return json(204); }],

    ["POST", /^\/auth\/logout$/, () => json(204)],
    ["POST", /^\/ui\/clear-cache$/, () => json(204)],
  ];

  const realFetch = window.fetch.bind(window);
  window.fetch = async (input, opts = {}) => {
    const url = new URL(typeof input === "string" ? input : input.url, location.href);
    const path = url.pathname.replace(/^.*?(\/(api|auth|ui)\/)/, "$1");
    if (!/^\/(api|auth|ui)\//.test(path)) return realFetch(input, opts);
    const method = (opts.method || "GET").toUpperCase();
    let body;
    try { body = opts.body ? JSON.parse(opts.body) : undefined; } catch { body = undefined; }
    await sleep(40 + Math.random() * 60); // feel like a network
    for (const [m, re, handler] of routes) {
      const match = method === m && path.match(re);
      if (match) return handler(match, body, url);
    }
    return err(404, `demo: ${method} ${path} is not implemented`);
  };

  // ---------- live stream ----------
  class DemoEventSource {
    static CONNECTING = 0;
    static OPEN = 1;
    static CLOSED = 2;
    constructor(url) {
      this.url = url;
      this.readyState = 0;
      this._handlers = {};
      streams.add(this);
      setTimeout(() => {
        if (this.readyState === 2) return;
        this.readyState = 1;
        this._emit("open");
      }, 150);
    }
    addEventListener(type, fn) { (this._handlers[type] ||= []).push(fn); }
    removeEventListener(type, fn) { this._handlers[type] = (this._handlers[type] || []).filter((f) => f !== fn); }
    close() { this.readyState = 2; streams.delete(this); }
    _emit(type, data) {
      const ev = data === undefined ? new Event(type) : new MessageEvent(type, { data });
      for (const fn of this._handlers[type] || []) fn(ev);
      if (typeof this["on" + type] === "function") this["on" + type](ev);
    }
  }
  window.EventSource = DemoEventSource;

  // ---------- banner ----------
  document.addEventListener("DOMContentLoaded", () => {
    const bar = document.createElement("div");
    bar.id = "demo-banner";
    bar.setAttribute("role", "note");
    Object.assign(bar.style, {
      background: "#5865f2", color: "#fff", font: "13px/1.4 system-ui, sans-serif", padding: "7px 16px", textAlign: "center",
    });
    bar.innerHTML = '<strong>Live demo</strong> · runs entirely in your browser with made-up data. Nothing talks to real cameras or devices, and reloading resets it. Simulated YourSix events arrive every ~25s. <a href="https://github.com/Coop25/yoursix-ifttt" style="color:#fff;font-weight:600">GitHub ↗</a>';
    document.body.prepend(bar);
  });
})();
