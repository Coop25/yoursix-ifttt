// Tests for internal/web/static/logic.js. Runs under Node (run-node.mjs, used
// by CI) or a headless browser (run.html), which both provide test() and
// assert.{equal, deepEqual, ok} and load logic.js first. See run.sh.
"use strict";

// 2026-10-16 is a Friday; in October Chicago is UTC-5.
const at = (iso) => new Date(iso);
const sched = (windows, timezone = "UTC") => ({ timezone, windows });
const flagged = (windows) => [...redundantWindows(windows)].sort();

test("contentOf drops bookkeeping and sorts keys at every level", () => {
  const got = contentOf({ id: 3, updatedAt: "x", usedBy: [], name: "R", b: { z: 1, a: 2 }, a: [{ y: 1, x: 2 }] });
  assert.equal(JSON.stringify(got), '{"a":[{"x":2,"y":1}],"b":{"a":2,"z":1},"name":"R"}');
});

test("changedKeys lists top-level fields that differ", () => {
  assert.deepEqual(changedKeys({ a: 1, b: [1], c: 3 }, { a: 1, b: [2], d: 4 }), ["b", "c", "d"]);
});

test("lineDiff marks kept, removed and added lines", () => {
  assert.deepEqual(lineDiff("a\nb\nc", "a\nb\nc"), [[" ", "a"], [" ", "b"], [" ", "c"]]);
  assert.deepEqual(lineDiff("a\nb\nc", "a\nx\nc"), [[" ", "a"], ["-", "b"], ["+", "x"], [" ", "c"]]);
  assert.deepEqual(lineDiff("a\nc", "a\nb\nc"), [[" ", "a"], ["+", "b"], [" ", "c"]]);
  assert.deepEqual(lineDiff("", "a"), [["+", "a"]]);
  assert.deepEqual(lineDiff("a", ""), [["-", "a"]]);
});

test("coverage covers a day window on its days only", () => {
  const c = coverage({ days: ["mon"], from: "09:00", to: "10:00" });
  assert.equal(c[9 * 60], 1);
  assert.equal(c[10 * 60], 0, "end is exclusive");
  assert.equal(c[1440 + 9 * 60], 0, "not Tuesday");
});

test("coverage spills overnight windows into the next morning, Sunday into Monday", () => {
  const c = coverage({ days: ["sun"], from: "22:00", to: "06:00" });
  assert.equal(c[6 * 1440 + 23 * 60], 1, "Sunday evening");
  assert.equal(c[5 * 60], 1, "Monday 05:00");
  assert.equal(c[6 * 60], 0, "Monday 06:00");
});

test("coverage treats no times, and old same-time windows, as all day", () => {
  assert.ok(coverage({ days: ["tue"] }).slice(1440, 2880).every((v) => v === 1));
  assert.ok(coverage({ days: ["tue"], from: "07:00", to: "07:00" }).slice(1440, 2880).every((v) => v === 1));
  assert.ok(coverage({}).every((v) => v === 1), "no days is every day");
});

test("zonedMinute counts from Monday 00:00 in the given zone", () => {
  assert.equal(zonedMinute("UTC", at("2026-10-12T00:00:00Z")), 0);
  assert.equal(zonedMinute("America/Chicago", at("2026-10-16T14:30:00Z")), 4 * 1440 + 9 * 60 + 30);
  assert.equal(zonedMinute("Mars/Olympus", at("2026-10-16T14:30:00Z")), -1);
});

test("scheduleActive matches the server's rules", () => {
  const business = sched([{ days: ["mon", "tue", "wed", "thu", "fri"], from: "08:00", to: "17:00" }], "America/Chicago");
  assert.equal(scheduleActive(business, at("2026-10-16T14:30:00Z")), true, "Fri 09:30 Central");
  assert.equal(scheduleActive(business, at("2026-10-16T23:30:00Z")), false, "Fri 18:30 Central");
  const nights = sched([{ days: ["fri"], from: "22:00", to: "06:00" }]);
  assert.equal(scheduleActive(nights, at("2026-10-17T05:59:00Z")), true, "Saturday morning belongs to Friday");
  assert.equal(scheduleActive(nights, at("2026-10-18T05:00:00Z")), false, "Sunday morning doesn't");
  assert.equal(scheduleActive(sched([]), at("2026-10-16T12:00:00Z")), false, "no windows is never open");
  assert.equal(scheduleActive(sched([{}], "Mars/Olympus"), at("2026-10-16T12:00:00Z")), false, "unknown zone is closed");
});

test("redundantWindows flags windows others already cover", () => {
  const weekdays = { days: ["mon", "tue", "wed", "thu", "fri"], from: "08:00", to: "17:00" };
  assert.deepEqual(flagged([weekdays, { days: ["mon"], from: "09:00", to: "12:00" }]), [1], "inside another");
  assert.deepEqual(flagged([weekdays, { ...weekdays }]), [1], "of two identical, the first is kept");
  assert.deepEqual(flagged([weekdays, { days: ["mon"], from: "16:00", to: "20:00" }]), [], "partial overlap adds something");
  const morning = { days: ["sat"], from: "06:00", to: "12:00" };
  const afternoon = { days: ["sat"], from: "12:00", to: "18:00" };
  assert.deepEqual(flagged([morning, afternoon, { days: ["sat"], from: "10:00", to: "14:00" }]), [2], "covered by two together");
  assert.deepEqual(flagged([{ days: ["fri"], from: "22:00", to: "06:00" }, { days: ["sat"], from: "01:00", to: "05:00" }]), [1], "covered by an overnight spill");
});

test("windowSummary describes a window", () => {
  assert.equal(windowSummary({ days: ["mon", "tue", "wed", "thu", "fri"], from: "22:00", to: "06:00" }), "Mon–Fri 22:00–06:00 (overnight)");
  assert.equal(windowSummary({ days: ["sun", "sat"] }), "Sat, Sun all day");
  assert.equal(windowSummary({ days: ["wed", "mon"], from: "09:00", to: "10:00" }), "Mon, Wed 09:00–10:00");
  assert.equal(windowSummary({}), "Every day all day");
  assert.equal(scheduleText(sched([{ days: ["mon"] }, { days: ["tue"], from: "01:00", to: "02:00" }])), "Mon all day; Tue 01:00–02:00");
});

test("tzLabel names common zones and passes others through", () => {
  assert.equal(tzLabel("America/Chicago"), "Central (Chicago)");
  assert.equal(tzLabel("Europe/Paris"), "Europe/Paris");
  assert.equal(fmtMinute(1440), "24:00");
  assert.equal(fmtMinute(9 * 60 + 5), "09:05");
});
