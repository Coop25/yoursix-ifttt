# Friendlier Rule and Saved-Action UI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace engineer-facing inputs (milliseconds, free-text field names, flat editor) with plain-English controls, and let saved delays watch for events.

**Architecture:** Backend: drop the saved-delay watch rejection, route watching saved delays through the existing `watchedDelay` path in the runner, and add a read-only `/api/catalog/fields` endpoint that aggregates recent event payloads. Frontend: pure helpers (durations, labels, summaries) go in `logic.js` with unit tests; `app.js` gains a duration input, a condition-row combo box, and a sectioned rule editor (layout A). Stored JSON is unchanged.

**Tech Stack:** Go 1.26 (`net/http`, `database/sql` + SQLite), vanilla JS (`h()` helper in `app.js`), Node-run JS tests.

**Spec:** `docs/superpowers/specs/2026-11-03-friendlier-rule-ui-design.md`

## Global Constraints

- Stored JSON and DB schema do not change: `delayMillis`, `cooldownSeconds`, `watch.maxMillis`, `timeoutSeconds`, `trigger.filters[{field,op,value}]`.
- Server limits unchanged: delay ≤ 600000 ms (10 min), watch `maxMillis` ≤ 3600000 ms (1 hour).
- No new frontend dependencies; follow `h()` / `$` / `$$` idioms in `app.js`.
- Light and dark themes work; editor usable at 375 px width with no horizontal scroll.
- "Edit as JSON" stays available.
- Go: `go test -race ./...` and `gofmt -l .` clean. JS: `sh internal/web/jstest/run.sh` passes.
- Commit messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. A rule saved before this change with `delayMillis: 1500` (not whole seconds) must round-trip through the visual editor unchanged if the user doesn't touch that delay. Test in Task 1 (`spanInputValue` keeps original) and Task 6 manual check.
2. A saved delay that watches, used inside another delay's watch actions (nested), must fail that action with a clear error rather than silently waiting with no watch. Test in Task 3.
3. Testing a saved delay with `sameAsTrigger` from the Saved actions tab (no rule) must finish and say nothing could match, not hang or error. Test in Task 3.
4. Events whose `eventData` holds objects/arrays or very long strings must not break or bloat `/api/catalog/fields`. Test in Task 5 (`TestFieldStatsSkipsComplexAndLong`).
5. Filters loaded from JSON whose field isn't in any suggestion list (e.g. `data.custom`) must display and save unchanged. Test in Task 7 (`fieldOptions` "Use …" entry test plus the manual round-trip check).

---

## File Structure

| File | Change |
| --- | --- |
| `internal/rules/request.go` | allow `Watch` on saved delays |
| `internal/rules/watch_test.go` | replace `TestSavedDelayCantWatch` |
| `internal/actions/runner.go`, `internal/actions/watch.go` | watching saved delays; nested rejection |
| `internal/actions/runner_test.go` | saved-delay watch tests |
| `internal/engine/watch_test.go` | end-to-end saved-delay watch |
| `internal/web/requests_api.go` | save-time nested check |
| `internal/store/fields.go` (new), `internal/store/fields_test.go` (new) | field/value aggregation |
| `internal/web/api.go`, `internal/web/server.go` | `/api/catalog/fields` |
| `internal/web/static/logic.js`, `internal/web/jstest/logic.test.js` | pure helpers |
| `internal/web/static/app.js`, `index.html`, `app.css` | UI |
| `demo/mock-api.js` | parity |
| `README.md`, `docs/screenshots/*` | docs |

---

### Task 1: Duration and label helpers (`logic.js`)

**Files:**
- Modify: `internal/web/static/logic.js`
- Test: `internal/web/jstest/logic.test.js`

**Interfaces:**
- Produces:
  - `splitSpan(ms: number) -> {h, m, s, exact: boolean}` — `exact` false when `ms % 1000 !== 0`; `s` rounded.
  - `joinSpan(h, m, s) -> number` (ms; blanks/NaN count as 0).
  - `fmtSpan(ms: number) -> string` — "45 s", "5 m", "1 h 30 m", "1 m 5 s", "0 s"; omits zero parts.
  - `OP_LABELS: [[value, label]]` — `equals`→"is", `not_equals`→"is not", `contains`→"contains", `starts_with`→"starts with", `exists`→"exists", `not_exists`→"doesn't exist", `regex`→"matches pattern (advanced)". Order as listed.
  - `opNeedsValue(op) -> boolean` — false for `exists`, `not_exists`.

- [ ] **Step 1: Write failing tests**

```js
test("splitSpan and joinSpan round-trip whole seconds", () => {
  assert.deepEqual(splitSpan(5430000), { h: 1, m: 30, s: 30, exact: true });
  assert.equal(joinSpan(1, 30, 30), 5430000);
  assert.equal(joinSpan("", "5", ""), 300000);
});
test("splitSpan flags sub-second values", () => {
  assert.deepEqual(splitSpan(1500), { h: 0, m: 0, s: 2, exact: false });
});
test("fmtSpan reads naturally", () => {
  assert.equal(fmtSpan(45000), "45 s");
  assert.equal(fmtSpan(300000), "5 m");
  assert.equal(fmtSpan(5400000), "1 h 30 m");
  assert.equal(fmtSpan(65000), "1 m 5 s");
  assert.equal(fmtSpan(0), "0 s");
  assert.equal(fmtSpan(1500), "1.5 s");
});
test("operator labels and value need", () => {
  assert.equal(OP_LABELS.find(([v]) => v === "not_exists")[1], "doesn't exist");
  assert.equal(opNeedsValue("exists"), false);
  assert.equal(opNeedsValue("equals"), true);
});
```

- [ ] **Step 2: Run** `sh internal/web/jstest/run.sh` — Expected: FAIL, `splitSpan is not defined`.
- [ ] **Step 3: Implement the helpers in `logic.js`** (sub-second `fmtSpan` shows one decimal of seconds only when under 60 s).
- [ ] **Step 4: Run** `sh internal/web/jstest/run.sh` — Expected: PASS.
- [ ] **Step 5: Commit** `git add internal/web/static/logic.js internal/web/jstest/logic.test.js && git commit -m "Add duration and operator label helpers"`

---

### Task 2: Allow saved delays to watch (validation + save-time nested check)

**Files:**
- Modify: `internal/rules/request.go` (remove the `a saved delay can't watch for events` block in `Validate`), `internal/rules/watch_test.go`
- Modify: `internal/web/requests_api.go`, test in `internal/web/requests_test.go`

**Interfaces:**
- Produces: `func (s *Server) checkNestedWatch(ctx context.Context, w *rules.Watch) error` in `requests_api.go` — returns `&rules.ActionError{...}`-style error text `watch: on-match action N: saved action "X" watches for events, so it can't run while another delay is watching` (same for `if-no-match action N`). Called from `checkRequestActions` for each delay action with a watch (rules), and from `handleCreateRequest`/`handleUpdateRequest` when `sr.Request.Watch != nil`.

- [ ] **Step 1: Replace `TestSavedDelayCantWatch` with `TestSavedDelayCanWatch`**: the same `SavedRequest` validates with no error, and `sr.Request.Watch.Trigger.Topics` is normalised to `[]string{}`.
- [ ] **Step 2: Add `TestSavedActionRejectsNestedWatchingDelay` to `internal/web/requests_test.go`**: create saved delay A with a watch (`stop: true`); creating saved delay B whose watch `actions` contain `{type: request, requestId: A}` returns 400 with body containing `can't run while another delay is watching`. Same check through rule save (`POST /api/rules` with a delay whose watch runs A) returns 400.
- [ ] **Step 3: Run** `go test ./internal/rules/ ./internal/web/ -run 'SavedDelay|NestedWatching'` — Expected: FAIL.
- [ ] **Step 4: Implement** the validation removal and `checkNestedWatch`.
- [ ] **Step 5: Run** the same command — Expected: PASS. Then `go test -race ./...` — Expected: PASS.
- [ ] **Step 6: Commit** `"Let saved delays watch for events"`.

---

### Task 3: Runner runs watching saved delays

**Files:**
- Modify: `internal/actions/runner.go`, `internal/actions/watch.go`
- Test: `internal/actions/runner_test.go`

**Interfaces:**
- Consumes: `rules.SavedRequest` with `Request.Watch` (Task 2).
- Produces:
  - `func (r *Runner) resolveSaved(ctx context.Context, a rules.Action, data map[string]any, lk SecretLookup, owner string, res *StepResult) (rules.SavedRequest, map[string]any, error)` — extracted from `runSavedRequest`: loads the saved action, checks secret ownership, renders params, returns the saved action and data with `params`. `runSavedRequest` calls it.
  - `RunOptions.inWatch bool` (unexported) — set by `watchedDelay` on the `RunOptions` it passes to `RunWith` for watch actions (`watch.go:61`).
- Behavior in `RunWith`: when `a.Type == rules.ActionRequest`, resolve it; if `sr.Request.Type == rules.ActionDelay && sr.Request.Watch != nil`:
  - if `opts.inWatch`: step error `saved action "X" watches for events, so it can't run while another delay is watching`.
  - else call `r.watchedDelay(ctx, i, sr.Request, withParams, opts, start, &res)` with the same stop/failed handling as a rule delay.
- `sameAsTrigger` with an empty `opts.RuleTrigger` (no topics, codes or filters, i.e. a saved-action test run): skip subscribing, and set `res.Watch.Note = "nothing to compare against: this action is being tested without a rule"`. Add `Note string \`json:"note,omitempty"\`` to `WatchResult`.

- [ ] **Step 1: Write failing tests** using a fake `SavedRequestSource` (map of id → `rules.SavedRequest`):
  - `TestSavedDelayWatchRestarts`: saved delay 30 ms, watch `{Trigger: {Codes: ["vmd"]}, Restart: true, MaxMillis: 200}`; `RunOptions.Watch` returns a channel that receives one `vmd` event at ~20 ms; assert step OK, `res[0].Watch.Restarts == 1`, `DurationMs >= 45`.
  - `TestSavedDelayWatchUsesParams`: watch filter value `{{ .params.cam }}`, rule params `{cam: "CAM-01"}`; assert the trigger passed to `opts.Watch` has filter value `CAM-01`.
  - `TestSavedDelayWatchSameAsCallingRule`: `sameAsTrigger: true`, `RunOptions.RuleTrigger{Codes: ["pir"]}`; assert subscribed trigger codes == `["pir"]`.
  - `TestSavedDelayWatchWithoutRule`: `sameAsTrigger: true`, empty `RuleTrigger`, `Watch` func that `t.Fatal`s if called; assert ok and `res[0].Watch.Note` non-empty.
  - `TestNestedWatchingSavedDelayFails`: rule delay with watch whose `IfNoMatch` runs saved watching delay; assert `res[0].Watch.Steps[0].Error` contains `can't run while another delay is watching`, overall `ok == false`.
- [ ] **Step 2: Run** `go test ./internal/actions/ -run SavedDelay\|NestedWatching -race` — Expected: FAIL.
- [ ] **Step 3: Implement** `resolveSaved`, the `RunWith` branch, `inWatch`, and the no-rule note.
- [ ] **Step 4: Run** the same — Expected: PASS; then `go test -race ./...` PASS.
- [ ] **Step 5: Commit** `"Run watching saved delays through the watch path"`.

---

### Task 4: Engine end-to-end for saved-delay watch

**Files:** Test: `internal/engine/watch_test.go`

- [ ] **Step 1: Write `TestSavedDelayWatchRestartsInEngine`**, mirroring `TestWatchRestartsDelay`: create a saved delay (watch `sameAsTrigger`, `restart`, `maxMillis` 2× delay) via `h.st.CreateRequest`, a rule with `[request→saved delay, http→recorder?msg=done]`; fire the trigger event, fire a second matching event mid-wait; assert the run's step 0 `Watch.Restarts == 1` and `done` was recorded after the restarted deadline.
- [ ] **Step 2: Run** `go test ./internal/engine/ -run SavedDelayWatch -race` — Expected: PASS (Task 3 provides behavior). If it fails, fix the engine wiring of `RunOptions.Watch`/`RuleTrigger` for request steps.
- [ ] **Step 3: Commit** `"Test saved-delay watches end to end"`.

---

### Task 5: `/api/catalog/fields`

**Files:**
- Create: `internal/store/fields.go`, `internal/store/fields_test.go`
- Modify: `internal/web/api.go`, `internal/web/server.go`, `demo/mock-api.js`
- Test: `internal/web/server_test.go`

**Interfaces:**
- Produces:
  - `type FieldStat struct { Name string \`json:"name"\`; Seen int \`json:"seen"\`; Values []ValueCount \`json:"values"\` }`, `type ValueCount struct { Value string \`json:"value"\`; Count int \`json:"count"\` }`.
  - `func (s *Store) FieldStats(ctx context.Context, topics, codes []string, limit int) ([]FieldStat, error)` — reads `payload` of the latest `limit` rows (`ORDER BY id DESC`) where topic ∈ topics (if any) and code matches codes (bare `vmd` or `device:vmd`, if any). Aggregates `eventCode`, `eventTopic`, `eventDescription` plus each top-level `eventData` key. Values: strings, numbers, bools stringified; objects, arrays, nulls and strings > 100 chars count toward `Seen` but add no value. Top 20 values by count (ties by value asc); fields sorted by `Seen` desc then name.
  - `GET /api/catalog/fields?topics=a,b&codes=x,y` (viewer) → `{"fields": []FieldStat, "known": []string}`, `limit` 500; `known` = the current documented list in `handleCatalog` minus names in `fields`. Move that list to a package var `knownFields`.
- [ ] **Step 1: Write `fields_test.go`**: `TestFieldStatsNarrowsAndCounts` (3 device:vmd events CAM-01 ×2, CAM-02 ×1 and 1 peripheral event; `FieldStats(["device"], nil, 500)` gives `deviceName` seen 3 with values `[{CAM-01,2},{CAM-02,1}]` and no `peripheralName`; codes `["device:vmd"]` and `["vmd"]` give the same); `TestFieldStatsSkipsComplexAndLong` (eventData `{"obj": {"a":1}, "long": <101 chars>}` → both fields present with `Values` empty); `TestFieldStatsCapsValues` (25 distinct values → 20).
- [ ] **Step 2: Add `TestCatalogFieldsEndpoint`** in `server_test.go`: viewer GET returns 200 with `fields` and `known` arrays; `known` excludes names present in `fields`.
- [ ] **Step 3: Run** `go test ./internal/store/ ./internal/web/ -run 'FieldStats|CatalogFields'` — FAIL.
- [ ] **Step 4: Implement**; add matching route to `demo/mock-api.js` computed from its sample events.
- [ ] **Step 5: Run** — PASS; `go test -race ./...` PASS.
- [ ] **Step 6: Commit** `"Add event field suggestions endpoint"`.

---

### Task 6: Duration inputs and readable summaries in the UI

**Files:** Modify `internal/web/static/app.js`, `app.css`, `index.html`

**Interfaces:**
- Consumes: `splitSpan`, `joinSpan`, `fmtSpan` (Task 1).
- Produces: `durationInput({ ms, maxMs, label }) -> HTMLElement` with `el.read() -> number (ms)` and `el.set(ms)`. If the loaded `ms` isn't exact and the boxes aren't edited, `read()` returns the original `ms`; shows muted note `Saved as 1500 ms; editing will round to whole seconds`.
- Replace: delay length (`delayMillis`, max 600000), watch longest wait (`maxMillis`, max 3600000), cooldown (`#f-cooldown` → `cooldownSeconds` = `read()/1000`). HTTP timeout keeps its number input, relabelled "Give up after (seconds)".
- `readAction` reads `delayMillis` via the box's duration input instead of `data-k`.
- Replace `fmtDuration` and the `${a.delayMillis} ms` summary in `actionSummary`/`watchSummary` with `fmtSpan`. Delay shows as "Wait" in `ACTION_LABELS` and summaries (`wait 5 m`).
- Server error text in `internal/rules/watch.go` limit message becomes `watch: the longest wait must be at least the delay (%s) and at most 1 hour` using a Go `fmtSpan` equivalent; update the matching assertion in `internal/rules/watch_test.go`.

- [ ] **Step 1:** Implement; run `go test ./internal/rules/` and `sh internal/web/jstest/run.sh` — PASS.
- [ ] **Step 2: Manual check** (`task run`, open http://localhost:8080): create a rule with a 5 m wait → JSON view shows `"delayMillis": 300000`. Paste JSON with `"delayMillis": 1500`, switch to visual, save without touching → JSON still `1500`. Cooldown 1 h → `"cooldownSeconds": 3600`.
- [ ] **Step 3: Commit** `"Enter waits and cooldowns in hours, minutes and seconds"`.

---

### Task 7: Condition rows with suggestions

**Files:** Modify `internal/web/static/app.js`, `index.html` (`tpl-filter`, remove `#field-list` datalist), `app.css`; `internal/web/api.go` (drop `fields` from `/api/catalog`); `logic.js` + test.

**Interfaces:**
- Consumes: `/api/catalog/fields` (Task 5), `OP_LABELS`, `opNeedsValue` (Task 1).
- Produces:
  - `logic.js`: `fieldOptions(stats: {fields, known}, typed: string) -> [{group, value, label}]` — groups "Seen in your events" (`label` = `"deviceName · 412"`), "Other known fields", and, when `typed` is non-empty and not an exact match, `{group: null, value: typed, label: 'Use “typed”'}` first. Case-insensitive substring filter on `typed`.
  - `app.js`: `comboBox({ value, placeholder, options: (typed) => [...] }) -> HTMLElement` with `.value` get/set and a `change` event; keyboard: ↑/↓/Enter/Escape. One shared implementation used for property and value.
  - `state.fieldStats` loaded by `loadFieldStats(topics, codes)` (debounced 300 ms on topic/code picker change; once on editor open).
  - `addFilterRow(f, container, { watch = false })`: property combo, operator `<select>` from `OP_LABELS`, value combo (hidden when `!opNeedsValue(op)`), remove button. With `watch`, value options start with `{value: "{{ .data.<prop> }}", label: "Same as the event that started this rule"}`.
  - `readFilters` unchanged output shape.
- [ ] **Step 1: Write JS tests** for `fieldOptions`: grouping/order; typed filter; "Use …" entry present for `data.custom`, absent for exact `deviceName`; empty stats gives only "Other known fields".
- [ ] **Step 2: Run** `sh internal/web/jstest/run.sh` — FAIL; implement `fieldOptions`; PASS.
- [ ] **Step 3: Implement** `comboBox`, new `addFilterRow`, loading, empty-state note `Suggestions appear once events start arriving.`, and remove the datalist and `fields` from `/api/catalog` (update any test asserting it).
- [ ] **Step 4: Manual check:** with sample events present, property list shows seen fields with counts; choosing `deviceName` lists camera names; a JSON-loaded filter on `data.custom` shows and saves unchanged; `exists` hides the value box; watch filter offers "Same as the event that started this rule".
- [ ] **Step 5:** `go test -race ./...` and JS tests PASS. **Commit** `"Pick filter properties and values from recent events"`.

---

### Task 8: Rule editor layout A

**Files:** Modify `internal/web/static/index.html` (`#editor` markup), `app.js` (`fillVisual`, `addActionBox`, `wireEditor`, error handling), `app.css`; `logic.js` + test.

**Interfaces:**
- Consumes: `fmtSpan`, `durationInput`, condition rows (Tasks 1, 6, 7); existing `actionSummary`.
- Produces:
  - `logic.js`: `limitsSummary({ scheduleName, cooldownSeconds, cooldownFrom, skipIfRunning }) -> string` — e.g. `"Any time · no limit"`, `"Weeknights · at most once every 5 m · skips while running"`. `stepErrorIndex(message) -> {list: "actions"|"onFailure", index} | null` parsing server errors like `action 3: …` and `on-failure action 2: …` (1-based in text, 0-based out).
  - Step cards: `addActionBox` gains a header row with number, `actionSummary` text, ↑ ↓, duplicate, ✕, and a collapse toggle; `box.dataset.open`. Existing steps start collapsed; new ones open. Summary refreshes on `input` within the box.
  - "+ Add step" `<select>`-style menu (HTTP request, Wait, Discord message, Saved action) replaces the four buttons; same for the on-failure list (no Wait).
- Markup order (`index.html`): header with name input + On/Off switch (`#f-enabled`) + "⋯" menu containing "Edit as JSON" (`#editor-json-mode` kept); `<section>` "When this happens" (topics, codes, "Only if…" conditions, "+ Add condition"; empty pickers read "Any event"); "Do these steps, in order"; `<details>` "If something goes wrong" (summary shows step count); `<details>` "Limits and schedule" (summary from `limitsSummary`). Template help becomes a `<details>` link inside the steps section. Topic/code pills show `code · description` where `yoursix.Catalog` descriptions exist — skip if no description data is available in the catalog response (do not add a new data source).
- On save error, `stepErrorIndex` expands that card, opens its `<details>` if folded, and scrolls it into view.
- [ ] **Step 1: JS tests** for `limitsSummary` (4 cases incl. "Any time · no limit") and `stepErrorIndex` (`"action 3: url is required"` → `{list:"actions", index:2}`, `"on-failure action 1: x"` → `{list:"onFailure", index:0}`, `"name is required"` → `null`). Run — FAIL; implement; PASS.
- [ ] **Step 2: Implement** markup, CSS (cards, folded sections, phone width), and JS.
- [ ] **Step 3: Manual check** in light and dark and at 375 px: open an existing rule — steps collapsed with readable summaries; add a step — opens expanded; reorder/duplicate/remove work; save with a blank URL in step 3 → step 3 expands and scrolls into view; unsaved-changes guard still fires; JSON mode round-trips; template preview still renders inside expanded steps.
- [ ] **Step 4:** `go test -race ./...` (static_test/editor_test) and JS tests PASS. **Commit** `"Lay out the rule editor as plain-English sections"`.

---

### Task 9: Saved-action editor watch panel and styling

**Files:** Modify `internal/web/static/app.js` (`addActionBox` delay branch, `watchSection`, `readRequestForm`), `app.css`.

**Interfaces:**
- Consumes: `watchSection(a, box)` (existing), `durationInput`, card styles (Task 8).
- Produces: `watchSection(a, box, { saved = false })` — with `saved`, the same-trigger checkbox reads "Same events as the rule that uses this action", and the filter hint mentions `{{ .params.name }}`. The delay branch of `addActionBox` renders `watchSection` for the saved-action editor (`container.id === "r-request"`) instead of the current error paragraph; nested watch-action boxes keep the existing "only top-level delays can watch" JSON-only behavior.
- [ ] **Step 1: Implement.**
- [ ] **Step 2: Manual check:** create saved delay "Hold on shop" (wait 5 m, watch same events as the rule, restart up to 30 m); use it in a rule; fire two matching events from Setup → live progress shows the countdown restart; Test button on Saved actions tab shows the "being tested without a rule" note.
- [ ] **Step 3:** All tests PASS. **Commit** `"Let saved delays watch for events in the editor"`.

---

### Task 10: Docs, demo, screenshots

**Files:** `README.md`, `demo/mock-api.js`, `docs/screenshots/*`

- [ ] **Step 1:** Update README sections on the rule editor, delays ("Wait"), watches on saved actions, and filters/suggestions. Remove text saying saved delays can't watch.
- [ ] **Step 2:** Ensure `demo/mock-api.js` accepts watching saved delays and mirrors the nested-watch rejection message.
- [ ] **Step 3:** Run `sh demo/screenshots.sh`; check rules, editor and actions screenshots in both themes look right.
- [ ] **Step 4:** `task test` and `task lint` — PASS. **Commit** `"Update docs and screenshots for the new editor"`.
