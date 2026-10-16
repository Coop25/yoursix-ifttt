"use strict";
// Pure helpers shared by the UI (app.js), the demo backend and the JS tests
// (internal/web/jstest): no DOM, no network. Loaded before app.js.

// ---------- history snapshots and diffs ----------
// contentOf is an item's editable content with sorted keys, as the server
// stores it in history snapshots, so two versions can be diffed.
const VOLATILE_KEYS = ["id", "createdAt", "updatedAt", "updatedBy", "updatedByName", "lastFiredAt", "lastFinishedAt", "running", "activeRuns", "usedBy"];
function contentOf(item) {
  const sort = (v) => (Array.isArray(v) ? v.map(sort)
    : v && typeof v === "object" ? Object.fromEntries(Object.keys(v).sort().map((k) => [k, sort(v[k])])) : v);
  const o = structuredClone(item);
  VOLATILE_KEYS.forEach((k) => delete o[k]);
  return sort(o);
}

const pretty = (v) => (v == null ? "" : JSON.stringify(v, null, 2));

function changedKeys(a, b) {
  const keys = new Set([...Object.keys(a), ...Object.keys(b)]);
  return [...keys].filter((k) => JSON.stringify(a[k]) !== JSON.stringify(b[k]));
}

// lineDiff is a longest-common-subsequence diff of two texts' lines:
// [[" " | "-" | "+", line], …].
function lineDiff(a, b) {
  const x = a ? a.split("\n") : [], y = b ? b.split("\n") : [];
  const n = x.length, m = y.length;
  const lcs = Array.from({ length: n + 1 }, () => new Int32Array(m + 1));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) lcs[i][j] = x[i] === y[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
  }
  const out = [];
  let i = 0, j = 0;
  while (i < n && j < m) {
    if (x[i] === y[j]) {
      out.push([" ", x[i++]]);
      j++;
    } else if (lcs[i + 1][j] >= lcs[i][j + 1]) out.push(["-", x[i++]]);
    else out.push(["+", y[j++]]);
  }
  while (i < n) out.push(["-", x[i++]]);
  while (j < m) out.push(["+", y[j++]]);
  return out;
}

// ---------- schedules ----------
const DAYS = [["mon", "Mon"], ["tue", "Tue"], ["wed", "Wed"], ["thu", "Thu"], ["fri", "Fri"], ["sat", "Sat"], ["sun", "Sun"]];
const DAY_KEYS = DAYS.map(([v]) => v);
const DEFAULT_TZ = "America/Chicago";
// Offered first in the time zone list, with friendly names.
const COMMON_TZ = [
  ["America/Chicago", "Central (Chicago)"], ["America/New_York", "Eastern (New York)"], ["America/Denver", "Mountain (Denver)"],
  ["America/Phoenix", "Arizona (Phoenix)"], ["America/Los_Angeles", "Pacific (Los Angeles)"], ["America/Anchorage", "Alaska (Anchorage)"],
  ["Pacific/Honolulu", "Hawaii (Honolulu)"], ["UTC", "UTC"],
];
const tzLabel = (tz) => (COMMON_TZ.find(([v]) => v === tz) || [tz, tz])[1];
const WEEK_MINUTES = 7 * 1440;
const hm = (s) => +s.slice(0, 2) * 60 + +s.slice(3, 5);
const fmtMinute = (m) => (m === 1440 ? "24:00" : `${String(Math.floor(m / 60)).padStart(2, "0")}:${String(m % 60).padStart(2, "0")}`);

// coverage is the minutes of the week (Monday 00:00 = 0) a window is open,
// as a bitmap. It mirrors rules.Window: an overnight window spills into the
// next morning, and a window with no times (or, in old data, the same start
// and end) is open all day.
function coverage(w) {
  const bits = new Uint8Array(WEEK_MINUTES);
  const days = w.days?.length ? w.days.map((d) => DAY_KEYS.indexOf(d)).filter((d) => d >= 0) : [0, 1, 2, 3, 4, 5, 6];
  const fill = (day, from, to) => bits.fill(1, day * 1440 + from, day * 1440 + to);
  for (const d of days) {
    if (!w.from || !w.to || w.from === w.to) fill(d, 0, 1440);
    else if (hm(w.from) < hm(w.to)) fill(d, hm(w.from), hm(w.to));
    else {
      fill(d, hm(w.from), 1440);
      fill((d + 1) % 7, 0, hm(w.to));
    }
  }
  return bits;
}

function scheduleCoverage(s) {
  const all = new Uint8Array(WEEK_MINUTES);
  for (const w of s.windows || []) coverage(w).forEach((v, i) => { if (v) all[i] = 1; });
  return all;
}

// zonedMinute is t's minute of the week in time zone tz, or -1 if the
// browser doesn't know the zone.
function zonedMinute(tz, t) {
  try {
    const p = Object.fromEntries(new Intl.DateTimeFormat("en-US", { timeZone: tz || DEFAULT_TZ, weekday: "short", hour: "2-digit", minute: "2-digit", hourCycle: "h23" })
      .formatToParts(t).map((x) => [x.type, x.value]));
    return DAY_KEYS.indexOf(p.weekday.slice(0, 3).toLowerCase()) * 1440 + (+p.hour % 24) * 60 + +p.minute;
  } catch {
    return -1;
  }
}

function scheduleActive(s, t) {
  const m = zonedMinute(s.timezone, t);
  return m >= 0 && scheduleCoverage(s)[m] === 1;
}

// redundantWindows lists the windows that add nothing: every minute they
// cover is covered by other windows. Of identical windows, the later ones
// are flagged and the first is kept.
function redundantWindows(windows) {
  const covers = windows.map(coverage);
  const flagged = new Set();
  for (let i = covers.length - 1; i >= 0; i--) {
    const others = new Uint8Array(WEEK_MINUTES);
    covers.forEach((c, j) => { if (j !== i && !flagged.has(j)) c.forEach((v, k) => { if (v) others[k] = 1; }); });
    if (covers[i].every((v, k) => !v || others[k])) flagged.add(i);
  }
  return flagged;
}

// windowSummary is a short label for a window, e.g. "Mon–Fri 22:00–06:00".
function windowSummary(w) {
  const days = (w.days || []).slice().sort((a, b) => DAY_KEYS.indexOf(a) - DAY_KEYS.indexOf(b));
  let dayText = "Every day";
  if (days.length && days.length < 7) {
    const idx = days.map((d) => DAY_KEYS.indexOf(d));
    const consecutive = idx.every((v, i) => i === 0 || v === idx[i - 1] + 1);
    const label = (d) => DAYS.find(([v]) => v === d)[1];
    dayText = consecutive && days.length > 2 ? `${label(days[0])}–${label(days[days.length - 1])}` : days.map(label).join(", ");
  }
  const overnight = w.from && w.from > w.to ? " (overnight)" : "";
  return `${dayText} ${w.from ? `${w.from}–${w.to}${overnight}` : "all day"}`;
}
const scheduleText = (s) => (s.windows || []).map(windowSummary).join("; ");

// Node (the JS tests) gets the helpers as a module; browsers get globals.
if (typeof module !== "undefined") {
  module.exports = {
    VOLATILE_KEYS, contentOf, pretty, changedKeys, lineDiff,
    DAYS, DAY_KEYS, DEFAULT_TZ, COMMON_TZ, tzLabel, WEEK_MINUTES, hm, fmtMinute,
    coverage, scheduleCoverage, zonedMinute, scheduleActive, redundantWindows, windowSummary, scheduleText,
  };
}
