// YourSix IFTTT — browser-only demo backend for GitHub Pages.
//
// Loaded before the real app.js. It replaces window.fetch (for /api, /auth
// and /ui paths) and window.EventSource (for the live stream) with an
// in-memory imitation of the Go server: rules with triggers and filters,
// cooldowns and "ignore while running", saved requests with parameters,
// write-only secrets, and runs that step through their actions in real
// time (delays count down; HTTP calls are simulated). Nothing leaves the
// browser, and reloading the page resets everything.
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
    lastFired: {}, lastFinished: {}, running: {},
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
    actions: [{ type: "discord", name: "Alert", message: "⚠️ {{ .data.deviceName }} at {{ .data.siteName }} went offline" }],
  });
  addRule({
    name: "West entrance armed → siren output", enabled: false, cooldownSeconds: 300, cooldownFrom: "finish",
    trigger: { topics: ["alarmgroup"], codes: ["armed"], filters: [{ field: "groupName", op: "contains", value: "west" }] },
    actions: [{ type: "http", name: "Siren output on (VAPIX)", url: "http://192.168.1.51/axis-cgi/io/port.cgi?action=1:/", auth: "digest", username: "root", password: '{{ secret "SIREN_PASS" }}' }],
  });

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
      case "secret": secretsUsed.add(String(v)); return "••••••"; // values never leave the server; masked here too
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
    if (a.type === "request") return S.requests.find((x) => x.id === a.requestId)?.name || "Saved request";
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
    res.status = 200;
    res.response = "OK";
  }

  async function runActions(e, run, rule) {
    const data = {
      event: { ...e.payload, key: `${e.payload.eventTopic}:${e.payload.eventCode}` },
      data: e.payload.eventData, rule: rule.name,
    };
    let stopped = false, ok = true;
    for (const [i, a] of rule.actions.entries()) {
      const res = { index: i, type: a.type, name: a.name || "" };
      if (stopped) { res.skipped = true; run.steps.push(res); continue; }
      const start = now();
      run.current = { index: i, type: a.type, name: stepLabel(a), startedAt: iso(start) };
      if (a.type === "delay") run.current.until = iso(start + a.delayMillis);
      changed(e);
      try {
        if (a.type === "delay") await sleep(a.delayMillis);
        else if (a.type === "discord") {
          res.target = "discord webhook";
          render(a.message, data);
          await sleep(150 + Math.random() * 200);
          res.status = 204;
        } else if (a.type === "request") {
          const sr = S.requests.find((x) => x.id === a.requestId);
          if (!sr) throw new Error(`saved request #${a.requestId}: not found`);
          if (!res.name) res.name = sr.name;
          const params = {};
          for (const p of sr.params) params[p.name] = p.default || "";
          for (const [k, v] of Object.entries(a.params || {})) if (v !== "") params[k] = render(v, data);
          const need = sr.params.find((p) => !p.default && !params[p.name]);
          if (need) throw new Error(`saved request "${sr.name}" needs a value for "${need.name}"`);
          await simulateHTTP(sr.request, { ...data, params }, res);
        } else {
          await simulateHTTP(a, data, res);
        }
        res.ok = true;
      } catch (err) {
        res.error = err.message;
        ok = false;
        if (!a.continueOnError) stopped = true;
      }
      res.durationMs = now() - start;
      run.steps.push(res);
      changed(e);
    }
    run.status = ok ? "ok" : "failed";
    run.finishedAt = iso();
    delete run.current;
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

  function dispatch(payload, opts) {
    const e = insertEvent(payload, opts);
    if (!e) return { eventRow: 0, duplicate: true, matchedRules: [] };
    const matched = [];
    for (const r of S.rules.filter((x) => x.enabled)) {
      if (!matches(r.trigger, payload)) continue;
      matched.push(r.id);
      const skip = claim(r);
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
  function validateRule(r) {
    if (!String(r.name || "").trim()) return "name is required";
    if (!r.actions || !r.actions.length) return "at least one action is required";
    for (const [i, a] of r.actions.entries()) {
      if (a.type === "http" && !a.url) return `action ${i + 1}: url is required`;
      if (a.type === "delay" && !(a.delayMillis > 0)) return `action ${i + 1}: delayMillis must be between 1 and 600000`;
      if (a.type === "discord" && !a.message && !a.discord?.embeds?.length) return `action ${i + 1}: a message or an embed is required`;
      if (a.type === "request") {
        const sr = S.requests.find((x) => x.id === a.requestId);
        if (!sr) return `action ${i + 1}: choose a saved request`;
        const need = sr.params.find((p) => !p.default && !(a.params || {})[p.name]);
        if (need) return `action ${i + 1}: saved request "${sr.name}" needs a value for "${need.name}"`;
      }
    }
    return "";
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
      return json(201, ruleView(r));
    }],
    ["GET", /^\/api\/rules\/(\d+)$/, (m) => { const r = S.rules.find((x) => x.id === +m[1]); return r ? json(200, ruleView(r)) : err(404, "not found"); }],
    ["PUT", /^\/api\/rules\/(\d+)$/, (m, body) => {
      const i = S.rules.findIndex((x) => x.id === +m[1]);
      if (i < 0) return err(404, "not found");
      const r = normalizeRule(body, { id: S.rules[i].id, createdAt: S.rules[i].createdAt, lastFiredAt: S.rules[i].lastFiredAt });
      const bad = validateRule(r);
      if (bad) return err(400, bad);
      S.rules[i] = r;
      return json(200, ruleView(r));
    }],
    ["DELETE", /^\/api\/rules\/(\d+)$/, (m) => { S.rules = S.rules.filter((x) => x.id !== +m[1]); return json(204); }],
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

    ["GET", /^\/api\/requests$/, () => json(200, [...S.requests].sort((a, b) => a.name.localeCompare(b.name)).map(requestView))],
    ["POST", /^\/api\/requests$/, (_, body) => {
      if (!String(body.name || "").trim()) return err(400, "name is required");
      if (!body.request?.url) return err(400, "url is required");
      const sr = addRequest({ ...body, request: { ...body.request, type: "http" }, createdAt: iso(), updatedAt: iso() });
      return json(201, requestView(sr));
    }],
    ["GET", /^\/api\/requests\/(\d+)$/, (m) => { const sr = S.requests.find((x) => x.id === +m[1]); return sr ? json(200, requestView(sr)) : err(404, "not found"); }],
    ["PUT", /^\/api\/requests\/(\d+)$/, (m, body) => {
      const i = S.requests.findIndex((x) => x.id === +m[1]);
      if (i < 0) return err(404, "not found");
      if (!String(body.name || "").trim()) return err(400, "name is required");
      S.requests[i] = { ...S.requests[i], ...body, request: { ...body.request, type: "http" }, id: S.requests[i].id, updatedAt: iso() };
      return json(200, requestView(S.requests[i]));
    }],
    ["DELETE", /^\/api\/requests\/(\d+)$/, (m) => {
      const used = requestView(S.requests.find((x) => x.id === +m[1]) || { id: -1 }).usedBy;
      if (used.length) return json(409, { error: `still used by ${used.length} rule(s): [${used.map((r) => r.name).join(" ")}]; remove it from them first`, usedBy: used });
      S.requests = S.requests.filter((x) => x.id !== +m[1]);
      return json(204);
    }],
    ["POST", /^\/api\/requests\/(\d+)\/test$/, (m, body) => {
      const sr = S.requests.find((x) => x.id === +m[1]);
      if (!sr) return err(404, "not found");
      const rule = { id: 0, name: `Request test: ${sr.name}`, actions: [{ type: "request", requestId: sr.id, params: body?.params || {} }] };
      const ev = sample("device", "vmd");
      ev.eventId = "";
      const e = insertEvent(ev, { test: true, sourceIp: "test:demo" });
      const run = startRun(e, rule, { test: true });
      return json(202, { run: runView(run), event: ev });
    }],

    ["GET", /^\/api\/events$/, (_, __, url) => {
      const limit = Math.min(+url.searchParams.get("limit") || 50, 200);
      const before = +url.searchParams.get("before") || 0;
      return json(200, S.events.filter((e) => !before || e.id < before).slice(0, limit).map((e) => eventView(e, false)));
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
      ...S.requests.filter((sr) => JSON.stringify(sr.request).includes(`secret \\"${s.name}\\"`)).map((sr) => `Request: ${sr.name}`),
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
    bar.setAttribute("role", "note");
    Object.assign(bar.style, {
      background: "#5865f2", color: "#fff", font: "13px/1.4 system-ui, sans-serif", padding: "7px 16px", textAlign: "center",
    });
    bar.innerHTML = '<strong>Live demo</strong> · runs entirely in your browser with made-up data. Nothing talks to real cameras or devices, and reloading resets it. Simulated YourSix events arrive every ~25s. <a href="https://github.com/Coop25/yoursix-ifttt" style="color:#fff;font-weight:600">GitHub ↗</a>';
    document.body.prepend(bar);
  });
})();
