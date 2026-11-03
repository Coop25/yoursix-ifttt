# Friendlier rule and saved-action UI — design

Date: 2026-11-03
Branch: `delay-watch`

## Goal

Make the rule and saved-action editors usable by someone who isn't an
engineer: no milliseconds, no guessing event property names, and a rule
editor that reads top to bottom like a sentence. Also give saved delays the
same "watch for events while waiting" ability rule delays already have, so a
reusable "keep the camera on the shop until 5 minutes after the last
activity" action is possible.

## Constraints

- The stored JSON format does not change (`delayMillis`, `cooldownSeconds`,
  `maxMillis`, `trigger.filters`, …). Existing rules and saved actions load
  and save unchanged. All unit conversion happens in the UI.
- "Edit as JSON" stays available for power users.
- Light and dark themes both keep working; the editor stays usable at phone
  width.
- No new frontend dependencies; keep the existing `h()` / vanilla-JS style
  in `internal/web/static/app.js`.

## 1. Duration inputs

A shared `durationInput({ value, unit, min, max, allowHours })` helper renders
three small number boxes labelled **h**, **m**, **s** and exposes
`read()` / `set()` in the field's own unit (ms or s).

Used for:

| Field | Stored as | Boxes |
| --- | --- | --- |
| Delay length (rule and saved delay) | `delayMillis` | h m s |
| Watch "keep waiting up to" | `watch.maxMillis` | h m s |
| Cooldown ("Run at most once every") | `cooldownSeconds` | h m s |
| HTTP timeout | `timeoutSeconds` | s only (unchanged control, relabelled "Give up after … seconds") |

Rules:

- Values convert exactly: `((h*60 + m)*60 + s) * 1000` for ms fields.
- Sub-second values can't be entered. If a loaded value isn't a whole number
  of seconds (e.g. `delayMillis: 1500` from JSON), the input shows the rounded
  value plus a muted note "Saved as 1500 ms; editing will round to whole
  seconds" and keeps the original value unless the user edits the boxes.
- Server-side limits are unchanged (delay ≤ 10 min, watch ≤ 1 hour); the
  input's `max` mirrors them and the server's error text is reworded to use
  minutes/hours instead of seconds where it mentions limits.
- Summaries everywhere (rule cards, step summaries, saved-action cards) use a
  shared `fmtSpan(ms)` → "5 m", "1 h 30 m", "45 s" instead of "300000 ms".
  The existing `fmtDuration` is folded into it.

## 2. Saved delays can watch for events

### Validation (`internal/rules/request.go`, `internal/rules/watch.go`)

- Remove the `a saved delay can't watch for events` rejection in
  `SavedRequest.Validate`.
- Validate the saved delay's watch with the existing `Watch.validate`.
- New rule: a watch's on-match / if-no-match actions may not be a `request`
  step pointing at a saved delay that itself watches. Saving can only check
  this against the current saved actions; the runner re-checks at run time
  (below).
- Saved-action parameters (`{{ .params.x }}`) are available in watch filter
  values, same as in the rest of the saved action.

### Runner (`internal/actions/runner.go`)

- In `RunWith`, a `request` step whose saved action is a delay with a watch
  goes through `watchedDelay`, exactly like a rule's own watched delay. The
  saved action is resolved once at step start (parameters rendered, secrets
  ownership checked as today) and the resolved delay action is handed to
  `watchedDelay` with the params-augmented data.
- "Same events as this rule's trigger" (`sameAsTrigger`) uses
  `RunOptions.RuleTrigger`, i.e. the trigger of the rule that called the saved
  action. In a direct "Test" run of a saved action (no rule) `sameAsTrigger`
  matches nothing and the test result says so.
- When a watch action is a `request` to a saved delay that watches, the
  runner fails that action with "a saved delay that watches for events can't
  run while another delay is watching" (recorded like other watch-action
  failures: the run fails, the wait continues).
- `delayOf` already reports a saved delay's length for the initial countdown;
  restarts update it through `OnWatch` exactly as for a rule delay, so live
  progress needs no other change.
- Results, live progress (`OnWatch`) and `.watch` data for later steps behave
  identically to a rule delay.

### Saved-action editor UI

- The delay form in the saved-action editor shows the same
  "While waiting, watch for events" panel as rule delays (`watchSection`),
  replacing the current "only the rule's own delays can watch" error.
- The checkbox "Same events as this rule's trigger" reads
  "Same events as the rule that uses this action".
- Watch filter values may use `{{ .params.* }}` and appear in the template
  preview.

### Tests

- `rules`: saved delay with watch validates; nested watching saved delay in
  watch actions is rejected when detectable.
- `actions`: saved delay restart extends the wait; stop skips remaining
  steps; on-match actions run; `sameAsTrigger` uses the calling rule's
  trigger; nested watching saved delay fails at run time.
- `engine`: an event arriving during a saved delay's wait is delivered to its
  watch.
- `demo/mock-api.js` mirrors the new validation.

## 3. Condition picker

### Endpoint

`GET /api/catalog/fields?topics=a,b&codes=x,y` (viewer access, read-only).

- Scans the most recent 500 stored events matching the given topics/codes
  (all events if none given). Codes may be bare or `topic:code`, as in
  triggers.
- Returns:

```json
{
  "fields": [
    { "name": "deviceName", "seen": 412,
      "values": [ { "value": "CAM-01", "count": 210 }, { "value": "Shop cam", "count": 95 } ] }
  ],
  "known": ["eventCode", "eventTopic", "barrierName", "…"]
}
```

- `fields`: every top-level `eventData` key plus the event-level fields
  (`eventCode`, `eventTopic`, `eventDescription`), sorted by `seen`
  descending; up to 20 most common values each (scalar values only, stringified;
  values longer than 100 characters are skipped).
- `known`: the documented field list currently returned by `/api/catalog`
  that wasn't seen, so the picker can offer them as a fallback.
- Implemented in `internal/store` as a query over recent event rows' stored
  JSON, aggregated in Go. No new table or index.

### Picker UI

Each condition row becomes: **[property ▾] [operator ▾] [value ▾] [✕]**

- **Property**: a searchable combo box. Groups: "Seen in your events" (with
  how often, e.g. "deviceName · 412"), then "Other known fields". Typing
  something not in the list offers "Use “xyz”". Existing free-text values
  load as-is.
- **Operator**: plain words, same stored values —
  is (`equals`), is not (`not_equals`), contains, starts with,
  exists, doesn't exist (`not_exists`), matches pattern (advanced) (`regex`).
- **Value**: the same combo box filled with that property's seen values;
  free text always allowed. Hidden for exists / doesn't exist.
- Suggestions refresh when the trigger's topics/codes change (debounced) and
  are fetched once per editor open otherwise.
- Watch filters use the same row. Their value list also offers
  "Same as the event that started this rule", which inserts
  `{{ .data.<property> }}`; this replaces the current "+ Filter" default and
  hint text.
- With no events stored yet, the picker shows only "Other known fields" and a
  muted note "Suggestions appear once events start arriving."

The `<datalist id="field-list">` and the `fields` array in `/api/catalog`
are removed once nothing uses them.

## 4. Rule editor layout (option A)

One scrolling page, top to bottom:

1. **Header**: rule name (large input) and an On/Off switch; "Edit as JSON"
   moves into a small "⋯" menu next to it.
2. **When this happens**: topic and event-code chips (existing pill
   pickers, with friendly labels where we have them, e.g. `vmd` shown as
   "vmd · motion"), then "Only if…" condition rows (section 3) and
   "+ Add condition". Empty state reads "Any event" rather than "(empty = any)".
3. **Do these steps, in order**: each step is a card that shows a one-line
   summary when collapsed (e.g. "2 · Wait 5 m, keep waiting while motion
   continues") and its full form when expanded. New and invalid steps open
   expanded; on open, existing steps are collapsed. Card header holds move
   up/down, duplicate and remove. A single "+ Add step" menu (HTTP request,
   Wait, Discord message, Saved action) replaces the four buttons. Delay is
   labelled "Wait" in the UI.
4. **If something goes wrong** (folded by default; summary shows the number
   of steps). Same step cards.
5. **Limits and schedule** (folded; summary like
   "Any time · no limit" or "Weeknights · at most once every 5 m"):
   schedule select, cooldown duration + started/finished, ignore-while-running.

Kept as-is: template preview (per-step, inside expanded cards), unsaved-changes
guard, conflict handling, template help (moved into the steps section as a
"Template help" link that opens the existing content).

The saved-action editor gets the same card styling and duration inputs; its
structure (name, description, parameters, one action) is unchanged.

Error messages from the server that name a step ("action 3: …") expand and
scroll to that step.

### Docs and demo

- Regenerate `docs/screenshots/*` (rules, editor, actions) via
  `demo/screenshots.sh`.
- Update README sections describing the editor, delays and saved actions.
- `demo/mock-api.js` serves `/api/catalog/fields` from its sample events.

## Testing

- Go unit tests as listed in section 2, plus store tests for the field
  aggregation (topic/code narrowing, value cap, non-scalar skip).
- `internal/web/jstest/logic.test.js`: duration conversion and formatting,
  operator label mapping, step summary text.
- Manual pass in the demo (light/dark, phone width): create a rule with a
  saved watching delay end to end; load an old rule with `delayMillis: 1500`
  and confirm it round-trips unchanged.

## Out of scope

- Changes to stored JSON or the database schema.
- Redesigning the Events, History, Schedules, Secrets or Setup tabs.
- Guided "wizard" rule creation (option B).
