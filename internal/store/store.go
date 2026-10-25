// Package store persists rules, received events, and rule runs in SQLite.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, keeps the binary CGO-free

	"yoursix-ifttt/internal/actions"
	"yoursix-ifttt/internal/rules"
	"yoursix-ifttt/internal/yoursix"
)

var ErrNotFound = errors.New("not found")

const schema = `
CREATE TABLE IF NOT EXISTS rules (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	name             TEXT    NOT NULL,
	enabled          INTEGER NOT NULL DEFAULT 1,
	trigger_json     TEXT    NOT NULL,
	actions_json     TEXT    NOT NULL,
	cooldown_seconds INTEGER NOT NULL DEFAULT 0,
	last_fired_at    TEXT,
	created_at       TEXT    NOT NULL,
	updated_at       TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS events (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	event_id    TEXT UNIQUE,
	topic       TEXT NOT NULL,
	code        TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	occurred    TEXT NOT NULL DEFAULT '',
	received_at TEXT NOT NULL,
	source_ip   TEXT NOT NULL DEFAULT '',
	test        INTEGER NOT NULL DEFAULT 0,
	payload     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_received_at ON events(received_at);

CREATE TABLE IF NOT EXISTS runs (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	event_row   INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
	rule_id     INTEGER NOT NULL,
	rule_name   TEXT    NOT NULL,
	status      TEXT    NOT NULL,
	started_at  TEXT    NOT NULL,
	finished_at TEXT,
	steps_json  TEXT    NOT NULL DEFAULT '[]'
);
CREATE INDEX IF NOT EXISTS runs_event_row ON runs(event_row);

CREATE TABLE IF NOT EXISTS requests (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	name            TEXT NOT NULL,
	description     TEXT NOT NULL DEFAULT '',
	params_json     TEXT NOT NULL DEFAULT '[]',
	request_json    TEXT NOT NULL,
	updated_by      TEXT NOT NULL DEFAULT '',
	updated_by_name TEXT NOT NULL DEFAULT '',
	created_at      TEXT NOT NULL,
	updated_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS revisions (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	kind          TEXT NOT NULL,
	item_id       INTEGER NOT NULL,
	name          TEXT NOT NULL,
	change        TEXT NOT NULL,
	snapshot_json TEXT NOT NULL,
	by_id         TEXT NOT NULL DEFAULT '',
	by_name       TEXT NOT NULL DEFAULT '',
	at            TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS revisions_item ON revisions(kind, item_id, id);

CREATE TABLE IF NOT EXISTS secrets (
	owner_id    TEXT NOT NULL,
	owner_name  TEXT NOT NULL DEFAULT '',
	name        TEXT NOT NULL,
	ciphertext  BLOB NOT NULL,
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL,
	PRIMARY KEY (owner_id, name)
);
`

// Run statuses.
const (
	RunRunning  = "running"
	RunOK       = "ok"
	RunFailed   = "failed"
	RunCooldown = "cooldown"
	// RunBusy: skipped because a previous run of the rule was still going.
	RunBusy = "busy"
	// RunOffHours: skipped because the event came outside the rule's
	// active hours.
	RunOffHours = "offhours"
)

type Store struct {
	db *sql.DB
	// fts reports whether the events_fts search index is available.
	fts bool
}

func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer; a single connection avoids SQLITE_BUSY churn.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db, fts: setupFTS(db) == nil}, nil
}

// Columns added after the first release; ALTERed in when missing.
var addedColumns = []struct{ table, column, def string }{
	{"rules", "cooldown_from", "TEXT NOT NULL DEFAULT 'start'"},
	{"rules", "skip_if_running", "INTEGER NOT NULL DEFAULT 0"},
	{"rules", "last_finished_at", "TEXT"},
	{"rules", "updated_by", "TEXT NOT NULL DEFAULT ''"},
	{"rules", "updated_by_name", "TEXT NOT NULL DEFAULT ''"},
	{"runs", "total_steps", "INTEGER NOT NULL DEFAULT 0"},
	{"runs", "current_json", "TEXT NOT NULL DEFAULT ''"},
	{"rules", "schedule_id", "INTEGER NOT NULL DEFAULT 0"},
	{"rules", "on_failure_json", "TEXT NOT NULL DEFAULT ''"},
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(schema + schedulesSchema); err != nil {
		return err
	}
	for _, c := range addedColumns {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, c.table, c.column).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, c.table, c.column, c.def)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func parseTime(v sql.NullString) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, v.String)
	return t
}

// ---- rules ----

// RuleView adds runtime info to a rule for the UI.
type RuleView struct {
	rules.Rule
	LastFiredAt    *time.Time `json:"lastFiredAt,omitempty"`
	LastFinishedAt *time.Time `json:"lastFinishedAt,omitempty"`
	// Running is filled in by the API from engine state.
	Running int `json:"running"`
	// ActiveRuns are this rule's runs in progress (filled in by the API).
	ActiveRuns []RunView `json:"activeRuns,omitempty"`
}

const ruleCols = `id, name, enabled, trigger_json, actions_json, cooldown_seconds, cooldown_from, skip_if_running, last_fired_at, last_finished_at, created_at, updated_at, updated_by, updated_by_name, schedule_id, on_failure_json`

func scanRule(row interface{ Scan(...any) error }) (RuleView, error) {
	var (
		r                    RuleView
		enabled, skip        int
		trig, acts           string
		onFail               string
		lastFired, lastDone  sql.NullString
		createdAt, updatedAt sql.NullString
	)
	if err := row.Scan(&r.ID, &r.Name, &enabled, &trig, &acts, &r.CooldownSeconds, &r.CooldownFrom, &skip, &lastFired, &lastDone, &createdAt, &updatedAt, &r.UpdatedBy, &r.UpdatedByName, &r.ScheduleID, &onFail); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return r, ErrNotFound
		}
		return r, err
	}
	r.Enabled = enabled == 1
	r.SkipIfRunning = skip == 1
	if err := json.Unmarshal([]byte(trig), &r.Trigger); err != nil {
		return r, fmt.Errorf("rule %d trigger: %w", r.ID, err)
	}
	if err := json.Unmarshal([]byte(acts), &r.Actions); err != nil {
		return r, fmt.Errorf("rule %d actions: %w", r.ID, err)
	}
	if onFail != "" {
		if err := json.Unmarshal([]byte(onFail), &r.OnFailure); err != nil {
			return r, fmt.Errorf("rule %d on-failure actions: %w", r.ID, err)
		}
	}
	r.CreatedAt = parseTime(createdAt)
	r.UpdatedAt = parseTime(updatedAt)
	if t := parseTime(lastFired); !t.IsZero() {
		r.LastFiredAt = &t
	}
	if t := parseTime(lastDone); !t.IsZero() {
		r.LastFinishedAt = &t
	}
	return r, nil
}

func (s *Store) ListRules(ctx context.Context) ([]RuleView, error) { return listRules(ctx, s.db) }

func listRules(ctx context.Context, q querier) ([]RuleView, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+ruleCols+` FROM rules ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RuleView{}
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) EnabledRules(ctx context.Context) ([]rules.Rule, error) {
	all, err := s.ListRules(ctx)
	if err != nil {
		return nil, err
	}
	var out []rules.Rule
	for _, r := range all {
		if r.Enabled {
			out = append(out, r.Rule)
		}
	}
	return out, nil
}

func (s *Store) GetRule(ctx context.Context, id int64) (RuleView, error) {
	return getRule(ctx, s.db, id)
}

func getRule(ctx context.Context, q querier, id int64) (RuleView, error) {
	return scanRule(q.QueryRowContext(ctx, `SELECT `+ruleCols+` FROM rules WHERE id = ?`, id))
}

// encodedRule is a validated rule's JSON columns.
type encodedRule struct{ trig, acts, onFail string }

func prepare(r *rules.Rule) (encodedRule, error) {
	var e encodedRule
	if err := r.Validate(); err != nil {
		return e, err
	}
	if err := actions.CheckTemplates(r.AllActions()); err != nil {
		return e, err
	}
	enc := func(v any) string {
		b, _ := json.Marshal(v) // plain structs; can't fail
		return string(b)
	}
	e.trig, e.acts = enc(r.Trigger), enc(r.Actions)
	if len(r.OnFailure) > 0 {
		e.onFail = enc(r.OnFailure)
	}
	return e, nil
}

// ConflictError is returned when saving an item that someone else changed
// after the editor loaded it (its updatedAt no longer matches). Current is
// the item as it is now.
type ConflictError struct {
	Kind    string
	Current any
	By      string
	At      time.Time
}

func (e ConflictError) Error() string {
	what := "rule"
	if e.Kind == KindAction {
		what = "saved action"
	} else if e.Kind == KindSchedule {
		what = "schedule"
	}
	by := e.By
	if by == "" {
		by = "someone else"
	}
	return fmt.Sprintf("%s changed this %s at %s, after you opened it", by, what, e.At.UTC().Format(time.RFC3339))
}

// checkFresh rejects a save based on a stale copy. A zero expected time
// (API clients that don't send updatedAt) skips the check.
func checkFresh(kind string, expected, actual time.Time, by string, current any) error {
	if expected.IsZero() || expected.Equal(actual) {
		return nil
	}
	return ConflictError{Kind: kind, Current: current, By: by, At: actual}
}

// ValidationError wraps rule problems the caller should show to the user.
type ValidationError struct{ Err error }

func (e ValidationError) Error() string { return e.Err.Error() }
func (e ValidationError) Unwrap() error { return e.Err }

// Editor identifies who is saving a rule; empty for seeded rules.
type Editor struct{ ID, Name string }

func (s *Store) CreateRule(ctx context.Context, r rules.Rule, by Editor) (RuleView, error) {
	enc, err := prepare(&r)
	if err != nil {
		return RuleView{}, ValidationError{err}
	}
	var id int64
	err = s.inTx(ctx, func(q querier) error {
		if err := checkRefs(ctx, q, r); err != nil {
			return err
		}
		ts := now()
		res, err := q.ExecContext(ctx,
			`INSERT INTO rules (name, enabled, trigger_json, actions_json, cooldown_seconds, cooldown_from, skip_if_running, created_at, updated_at, updated_by, updated_by_name, schedule_id, on_failure_json) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.Name, boolInt(r.Enabled), enc.trig, enc.acts, r.CooldownSeconds, r.CooldownFrom, boolInt(r.SkipIfRunning), ts, ts, by.ID, by.Name, r.ScheduleID, enc.onFail)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		created, err := getRule(ctx, q, id)
		if err != nil {
			return err
		}
		return record(ctx, q, KindRule, id, created.Name, ChangeCreated, created.Rule, by, nil)
	})
	if err != nil {
		return RuleView{}, err
	}
	return s.GetRule(ctx, id)
}

// ruleBase is a rule's state before a change, for the edit log.
func ruleBase(r RuleView) *revisionBase {
	return &revisionBase{item: r.Rule, name: r.Name, by: Editor{r.UpdatedBy, r.UpdatedByName}, at: r.UpdatedAt.UTC().Format(time.RFC3339Nano)}
}

func (s *Store) UpdateRule(ctx context.Context, r rules.Rule, by Editor) (RuleView, error) {
	enc, err := prepare(&r)
	if err != nil {
		return RuleView{}, ValidationError{err}
	}
	err = s.inTx(ctx, func(q querier) error {
		if err := checkRefs(ctx, q, r); err != nil {
			return err
		}
		before, err := getRule(ctx, q, r.ID)
		if err != nil {
			return err
		}
		if err := checkFresh(KindRule, r.UpdatedAt, before.UpdatedAt, before.UpdatedByName, before); err != nil {
			return err
		}
		// The read above and this write share the store's one connection,
		// so nothing can change the row in between.
		if _, err := q.ExecContext(ctx,
			`UPDATE rules SET name=?, enabled=?, trigger_json=?, actions_json=?, cooldown_seconds=?, cooldown_from=?, skip_if_running=?, updated_at=?, updated_by=?, updated_by_name=?, schedule_id=?, on_failure_json=? WHERE id=?`,
			r.Name, boolInt(r.Enabled), enc.trig, enc.acts, r.CooldownSeconds, r.CooldownFrom, boolInt(r.SkipIfRunning), now(), by.ID, by.Name, r.ScheduleID, enc.onFail, r.ID); err != nil {
			return err
		}
		updated, err := getRule(ctx, q, r.ID)
		if err != nil {
			return err
		}
		return record(ctx, q, KindRule, r.ID, updated.Name, ChangeUpdated, updated.Rule, by, ruleBase(before))
	})
	if err != nil {
		return RuleView{}, err
	}
	return s.GetRule(ctx, r.ID)
}

func (s *Store) DeleteRule(ctx context.Context, id int64, by Editor) error {
	return s.inTx(ctx, func(q querier) error {
		before, err := getRule(ctx, q, id)
		if err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `DELETE FROM rules WHERE id=?`, id); err != nil {
			return err
		}
		return record(ctx, q, KindRule, id, before.Name, ChangeDeleted, before.Rule, by, nil)
	})
}

// SeedRules imports every *.json rule in dir, but only when no rules exist
// yet, so edits made in the UI are never overwritten on restart.
func (s *Store) SeedRules(ctx context.Context, dir string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rules`).Scan(&n); err != nil {
		return 0, err
	}
	if n > 0 {
		return 0, nil
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return 0, err
	}
	sort.Strings(files)
	imported := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return imported, err
		}
		var r rules.Rule
		if err := json.Unmarshal(b, &r); err != nil {
			return imported, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		if _, err := s.CreateRule(ctx, r, Editor{}); err != nil {
			return imported, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		imported++
	}
	return imported, nil
}

func (s *Store) MarkRuleFinished(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE rules SET last_finished_at=? WHERE id=?`, at.UTC().Format(time.RFC3339Nano), id)
	return err
}

func (s *Store) MarkRuleFired(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE rules SET last_fired_at=? WHERE id=?`, at.UTC().Format(time.RFC3339Nano), id)
	return err
}

// ---- events ----

// InsertEvent stores a received event. duplicate is true when an event with
// the same eventId was already stored (YourSix redelivery).
func (s *Store) InsertEvent(ctx context.Context, ev yoursix.Event, raw []byte, sourceIP string, test bool) (rowID int64, duplicate bool, err error) {
	var eventID any
	if ev.EventID != "" {
		eventID = ev.EventID
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO events (event_id, topic, code, description, occurred, received_at, source_ip, test, payload)
		 VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING`,
		eventID, ev.EventTopic, ev.EventCode, ev.EventDescription, ev.EventOccurred, now(), sourceIP, boolInt(test), string(raw))
	if err != nil {
		return 0, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, true, nil
	}
	rowID, err = res.LastInsertId()
	return rowID, false, err
}

type RunView struct {
	ID         int64                `json:"id"`
	EventRow   int64                `json:"eventRow"`
	RuleID     int64                `json:"ruleId"`
	RuleName   string               `json:"ruleName"`
	Status     string               `json:"status"`
	StartedAt  time.Time            `json:"startedAt"`
	FinishedAt *time.Time           `json:"finishedAt,omitempty"`
	Steps      []actions.StepResult `json:"steps"`
	// TotalSteps is the rule's action count when the run started.
	TotalSteps int `json:"totalSteps"`
	// Current is the step in progress, while the run is running.
	Current *StepInProgress `json:"current,omitempty"`
}

// StepInProgress describes the action a running rule is on. ElapsedMs and
// RemainingMs are computed when the view is built, from the server's clock,
// so a browser with a skewed clock can still count down correctly.
type StepInProgress struct {
	Index       int        `json:"index"`
	Type        string     `json:"type"`
	Name        string     `json:"name"`
	StartedAt   time.Time  `json:"startedAt"`
	Until       *time.Time `json:"until,omitempty"` // delays only
	ElapsedMs   int64      `json:"elapsedMs"`
	RemainingMs *int64     `json:"remainingMs,omitempty"` // delays only
	// Matches and Restarts count what a watched delay has seen so far.
	Matches  int `json:"matches,omitempty"`
	Restarts int `json:"restarts,omitempty"`
}

type EventView struct {
	ID          int64           `json:"id"`
	EventID     string          `json:"eventId"`
	Topic       string          `json:"topic"`
	Code        string          `json:"code"`
	Description string          `json:"description"`
	Occurred    string          `json:"occurred"`
	ReceivedAt  time.Time       `json:"receivedAt"`
	SourceIP    string          `json:"sourceIp"`
	Test        bool            `json:"test"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Runs        []RunView       `json:"runs"`

	// From eventData, for linking to the event's video. MatrixURL is filled
	// in by the web layer, which knows the platform URL.
	DeviceID      string `json:"deviceId,omitempty"`
	VideoSourceID string `json:"videoSourceId,omitempty"`
	GroupID       string `json:"groupId,omitempty"`
	MatrixURL     string `json:"matrixUrl,omitempty"`
}

// setRefs reads the device, video source and alarm group from a stored
// payload. videoSourceId may be a string or a number.
func (e *EventView) setRefs(payload []byte) {
	var p struct {
		EventData struct {
			DeviceID      string `json:"deviceId"`
			VideoSourceID any    `json:"videoSourceId"`
			GroupID       string `json:"groupId"`
		} `json:"eventData"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return
	}
	e.DeviceID, e.GroupID = p.EventData.DeviceID, p.EventData.GroupID
	switch v := p.EventData.VideoSourceID.(type) {
	case string:
		e.VideoSourceID = v
	case float64:
		e.VideoSourceID = strconv.FormatFloat(v, 'f', -1, 64)
	}
}

// ListEvents returns newest-first events with their runs. beforeID pages.
func (s *Store) ListEvents(ctx context.Context, limit int, beforeID int64) ([]EventView, error) {
	return s.SearchEvents(ctx, EventQuery{Limit: limit, Before: beforeID})
}

// Event outcomes EventQuery can filter on.
const (
	OutcomeMatched = "matched" // ran at least one rule (any status)
	OutcomeNoMatch = "nomatch"
	OutcomeOK      = "ok"
	OutcomeFailed  = "failed"
	OutcomeSkipped = "skipped" // a rule matched but was in cooldown, busy or off hours
	OutcomeTest    = "test"    // simulated, replayed or test runs
)

// EventQuery selects events. Text is a search (see parseSearch): every free
// word or "phrase" must appear (case-insensitively) in the event's
// topic:code, description, source IP, payload or the name of a rule it ran;
// key:value terms match one field, e.g. device:CAM-01, site:"Head office",
// rule:gate, code:vmd, ip:10.0., status:failed, or any eventData field.
// Since and Until bound when it was received.
type EventQuery struct {
	Limit        int
	Before       int64
	Text         string
	Outcome      string
	Since, Until time.Time
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func (s *Store) SearchEvents(ctx context.Context, f EventQuery) ([]EventView, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT id, COALESCE(event_id,''), topic, code, description, occurred, received_at, source_ip, test, payload FROM events WHERE 1=1`
	args := []any{}
	if f.Before > 0 {
		q += ` AND id < ?`
		args = append(args, f.Before)
	}
	cond, condArgs := s.searchSQL(parseSearch(f.Text))
	q += cond
	args = append(args, condArgs...)
	// julianday() compares instants, whatever the stored fraction digits.
	if !f.Since.IsZero() {
		q += ` AND julianday(received_at) >= julianday(?)`
		args = append(args, f.Since.UTC().Format(time.RFC3339Nano))
	}
	if !f.Until.IsZero() {
		q += ` AND julianday(received_at) < julianday(?)`
		args = append(args, f.Until.UTC().Format(time.RFC3339Nano))
	}
	const hasRun = ` AND EXISTS (SELECT 1 FROM runs WHERE runs.event_row = events.id`
	switch f.Outcome {
	case "":
	case OutcomeMatched:
		q += hasRun + `)`
	case OutcomeNoMatch:
		q += ` AND NOT EXISTS (SELECT 1 FROM runs WHERE runs.event_row = events.id)`
	case OutcomeOK, OutcomeFailed:
		q += hasRun + ` AND runs.status = ?)`
		args = append(args, f.Outcome)
	case OutcomeSkipped:
		q += hasRun + ` AND runs.status IN (?, ?, ?))`
		args = append(args, RunCooldown, RunBusy, RunOffHours)
	case OutcomeTest:
		q += ` AND test = 1`
	default:
		return nil, ValidationError{fmt.Errorf("unknown outcome %q", f.Outcome)}
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out := []EventView{}
	idx := map[int64]int{}
	for rows.Next() {
		var (
			e       EventView
			recv    sql.NullString
			test    int
			payload []byte
		)
		if err := rows.Scan(&e.ID, &e.EventID, &e.Topic, &e.Code, &e.Description, &e.Occurred, &recv, &e.SourceIP, &test, &payload); err != nil {
			rows.Close()
			return nil, err
		}
		e.ReceivedAt = parseTime(recv)
		e.Test = test == 1
		e.setRefs(payload)
		e.Runs = []RunView{}
		idx[e.ID] = len(out)
		out = append(out, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	runs, err := s.runsBetween(ctx, out[len(out)-1].ID, out[0].ID)
	if err != nil {
		return nil, err
	}
	for _, r := range runs {
		if i, ok := idx[r.EventRow]; ok {
			out[i].Runs = append(out[i].Runs, r)
		}
	}
	return out, nil
}

func (s *Store) GetEvent(ctx context.Context, id int64) (EventView, error) {
	var (
		e       EventView
		recv    sql.NullString
		test    int
		payload string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, COALESCE(event_id,''), topic, code, description, occurred, received_at, source_ip, test, payload FROM events WHERE id=?`, id).
		Scan(&e.ID, &e.EventID, &e.Topic, &e.Code, &e.Description, &e.Occurred, &recv, &e.SourceIP, &test, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	if err != nil {
		return e, err
	}
	e.ReceivedAt = parseTime(recv)
	e.Test = test == 1
	e.Payload = json.RawMessage(payload)
	e.setRefs(e.Payload)
	if e.Runs, err = s.runsBetween(ctx, id, id); err != nil {
		return e, err
	}
	return e, nil
}

func (s *Store) runsBetween(ctx context.Context, lo, hi int64) ([]RunView, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runCols+` FROM runs WHERE event_row BETWEEN ? AND ? ORDER BY id`, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRuns(rows)
}

const runCols = `id, event_row, rule_id, rule_name, status, started_at, finished_at, steps_json, total_steps, current_json`

func scanRuns(rows *sql.Rows) ([]RunView, error) {
	out := []RunView{}
	nowT := time.Now()
	for rows.Next() {
		var (
			r                 RunView
			started, finished sql.NullString
			steps, current    string
		)
		if err := rows.Scan(&r.ID, &r.EventRow, &r.RuleID, &r.RuleName, &r.Status, &started, &finished, &steps, &r.TotalSteps, &current); err != nil {
			return nil, err
		}
		r.StartedAt = parseTime(started)
		if t := parseTime(finished); !t.IsZero() {
			r.FinishedAt = &t
		}
		_ = json.Unmarshal([]byte(steps), &r.Steps)
		if r.Steps == nil {
			r.Steps = []actions.StepResult{}
		}
		if current != "" && r.Status == RunRunning {
			var cur StepInProgress
			if json.Unmarshal([]byte(current), &cur) == nil {
				cur.ElapsedMs = max(nowT.Sub(cur.StartedAt).Milliseconds(), 0)
				if cur.Until != nil {
					rem := max(cur.Until.Sub(nowT).Milliseconds(), 0)
					cur.RemainingMs = &rem
				}
				r.Current = &cur
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ActiveRuns returns every run still in progress, for the Rules tab.
func (s *Store) ActiveRuns(ctx context.Context) ([]RunView, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runCols+` FROM runs WHERE status=? ORDER BY id`, RunRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRuns(rows)
}

// UpdateRunCurrent records which step a running run is on.
func (s *Store) UpdateRunCurrent(ctx context.Context, runID int64, cur StepInProgress) error {
	cur.ElapsedMs, cur.RemainingMs = 0, nil // computed on read
	b, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE runs SET current_json=? WHERE id=? AND status=?`, string(b), runID, RunRunning)
	return err
}

// ---- runs ----

func (s *Store) StartRun(ctx context.Context, eventRow int64, rule rules.Rule, status string) (int64, error) {
	var finished any
	if status != RunRunning {
		finished = now()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (event_row, rule_id, rule_name, status, started_at, finished_at, total_steps) VALUES (?,?,?,?,?,?,?)`,
		eventRow, rule.ID, rule.Name, status, now(), finished, len(rule.Actions))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FinishRun(ctx context.Context, runID int64, status string, steps []actions.StepResult) error {
	b, err := json.Marshal(steps)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE runs SET status=?, finished_at=?, steps_json=?, current_json='' WHERE id=?`, status, now(), string(b), runID)
	return err
}

// UpdateRunSteps records progress on a run that is still going.
func (s *Store) UpdateRunSteps(ctx context.Context, runID int64, steps []actions.StepResult) error {
	b, err := json.Marshal(steps)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE runs SET steps_json=? WHERE id=? AND status=?`, string(b), runID, RunRunning)
	return err
}

// GetRun returns one run, or ErrNotFound.
func (s *Store) GetRun(ctx context.Context, runID int64) (RunView, error) {
	var eventRow int64
	err := s.db.QueryRowContext(ctx, `SELECT event_row FROM runs WHERE id=?`, runID).Scan(&eventRow)
	if errors.Is(err, sql.ErrNoRows) {
		return RunView{}, ErrNotFound
	}
	if err != nil {
		return RunView{}, err
	}
	runs, err := s.runsBetween(ctx, eventRow, eventRow)
	if err != nil {
		return RunView{}, err
	}
	for _, r := range runs {
		if r.ID == runID {
			return r, nil
		}
	}
	return RunView{}, ErrNotFound
}

// FailInterruptedRuns marks runs left "running" by a previous process.
func (s *Store) FailInterruptedRuns(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET status=?, finished_at=?, current_json='' WHERE status=?`, RunFailed, now(), RunRunning)
	return err
}

// Prune deletes events (and their runs) received before cutoff.
func (s *Store) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM events WHERE received_at < ?`, cutoff.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
