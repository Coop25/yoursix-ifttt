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
)

type Store struct{ db *sql.DB }

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
	return &Store{db: db}, nil
}

// Columns added after the first release; ALTERed in when missing.
var addedColumns = []struct{ table, column, def string }{
	{"rules", "cooldown_from", "TEXT NOT NULL DEFAULT 'start'"},
	{"rules", "skip_if_running", "INTEGER NOT NULL DEFAULT 0"},
	{"rules", "last_finished_at", "TEXT"},
	{"rules", "updated_by", "TEXT NOT NULL DEFAULT ''"},
	{"rules", "updated_by_name", "TEXT NOT NULL DEFAULT ''"},
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
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
}

const ruleCols = `id, name, enabled, trigger_json, actions_json, cooldown_seconds, cooldown_from, skip_if_running, last_fired_at, last_finished_at, created_at, updated_at, updated_by, updated_by_name`

func scanRule(row interface{ Scan(...any) error }) (RuleView, error) {
	var (
		r                    RuleView
		enabled, skip        int
		trig, acts           string
		lastFired, lastDone  sql.NullString
		createdAt, updatedAt sql.NullString
	)
	if err := row.Scan(&r.ID, &r.Name, &enabled, &trig, &acts, &r.CooldownSeconds, &r.CooldownFrom, &skip, &lastFired, &lastDone, &createdAt, &updatedAt, &r.UpdatedBy, &r.UpdatedByName); err != nil {
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

func (s *Store) ListRules(ctx context.Context) ([]RuleView, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+ruleCols+` FROM rules ORDER BY name COLLATE NOCASE`)
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
	return scanRule(s.db.QueryRowContext(ctx, `SELECT `+ruleCols+` FROM rules WHERE id = ?`, id))
}

func prepare(r *rules.Rule) (trig, acts []byte, err error) {
	if err := r.Validate(); err != nil {
		return nil, nil, err
	}
	if err := actions.CheckTemplates(r.Actions); err != nil {
		return nil, nil, err
	}
	if trig, err = json.Marshal(r.Trigger); err != nil {
		return nil, nil, err
	}
	acts, err = json.Marshal(r.Actions)
	return trig, acts, err
}

// ValidationError wraps rule problems the caller should show to the user.
type ValidationError struct{ Err error }

func (e ValidationError) Error() string { return e.Err.Error() }
func (e ValidationError) Unwrap() error { return e.Err }

// Editor identifies who is saving a rule; empty for seeded rules.
type Editor struct{ ID, Name string }

func (s *Store) CreateRule(ctx context.Context, r rules.Rule, by Editor) (RuleView, error) {
	trig, acts, err := prepare(&r)
	if err != nil {
		return RuleView{}, ValidationError{err}
	}
	ts := now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO rules (name, enabled, trigger_json, actions_json, cooldown_seconds, cooldown_from, skip_if_running, created_at, updated_at, updated_by, updated_by_name) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		r.Name, boolInt(r.Enabled), string(trig), string(acts), r.CooldownSeconds, r.CooldownFrom, boolInt(r.SkipIfRunning), ts, ts, by.ID, by.Name)
	if err != nil {
		return RuleView{}, err
	}
	id, _ := res.LastInsertId()
	return s.GetRule(ctx, id)
}

func (s *Store) UpdateRule(ctx context.Context, r rules.Rule, by Editor) (RuleView, error) {
	trig, acts, err := prepare(&r)
	if err != nil {
		return RuleView{}, ValidationError{err}
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE rules SET name=?, enabled=?, trigger_json=?, actions_json=?, cooldown_seconds=?, cooldown_from=?, skip_if_running=?, updated_at=?, updated_by=?, updated_by_name=? WHERE id=?`,
		r.Name, boolInt(r.Enabled), string(trig), string(acts), r.CooldownSeconds, r.CooldownFrom, boolInt(r.SkipIfRunning), now(), by.ID, by.Name, r.ID)
	if err != nil {
		return RuleView{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return RuleView{}, ErrNotFound
	}
	return s.GetRule(ctx, r.ID)
}

func (s *Store) DeleteRule(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM rules WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
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
}

// ListEvents returns newest-first events with their runs. beforeID pages.
func (s *Store) ListEvents(ctx context.Context, limit int, beforeID int64) ([]EventView, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT id, COALESCE(event_id,''), topic, code, description, occurred, received_at, source_ip, test FROM events`
	args := []any{}
	if beforeID > 0 {
		q += ` WHERE id < ?`
		args = append(args, beforeID)
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
			e    EventView
			recv sql.NullString
			test int
		)
		if err := rows.Scan(&e.ID, &e.EventID, &e.Topic, &e.Code, &e.Description, &e.Occurred, &recv, &e.SourceIP, &test); err != nil {
			rows.Close()
			return nil, err
		}
		e.ReceivedAt = parseTime(recv)
		e.Test = test == 1
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
	if e.Runs, err = s.runsBetween(ctx, id, id); err != nil {
		return e, err
	}
	return e, nil
}

func (s *Store) runsBetween(ctx context.Context, lo, hi int64) ([]RunView, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, event_row, rule_id, rule_name, status, started_at, finished_at, steps_json
		 FROM runs WHERE event_row BETWEEN ? AND ? ORDER BY id`, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunView{}
	for rows.Next() {
		var (
			r                 RunView
			started, finished sql.NullString
			steps             string
		)
		if err := rows.Scan(&r.ID, &r.EventRow, &r.RuleID, &r.RuleName, &r.Status, &started, &finished, &steps); err != nil {
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
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- runs ----

func (s *Store) StartRun(ctx context.Context, eventRow int64, rule rules.Rule, status string) (int64, error) {
	var finished any
	if status != RunRunning {
		finished = now()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (event_row, rule_id, rule_name, status, started_at, finished_at) VALUES (?,?,?,?,?,?)`,
		eventRow, rule.ID, rule.Name, status, now(), finished)
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
	_, err = s.db.ExecContext(ctx, `UPDATE runs SET status=?, finished_at=?, steps_json=? WHERE id=?`, status, now(), string(b), runID)
	return err
}

// FailInterruptedRuns marks runs left "running" by a previous process.
func (s *Store) FailInterruptedRuns(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET status=?, finished_at=? WHERE status=?`, RunFailed, now(), RunRunning)
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
