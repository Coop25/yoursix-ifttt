package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"yoursix-ifttt/internal/rules"
)

const schedulesSchema = `
CREATE TABLE IF NOT EXISTS schedules (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	name            TEXT NOT NULL,
	description     TEXT NOT NULL DEFAULT '',
	windows_json    TEXT NOT NULL,
	timezone        TEXT NOT NULL,
	updated_by      TEXT NOT NULL DEFAULT '',
	updated_by_name TEXT NOT NULL DEFAULT '',
	created_at      TEXT NOT NULL,
	updated_at      TEXT NOT NULL
);
`

// ScheduleView adds the rules that use a schedule, for the UI.
type ScheduleView struct {
	rules.Schedule
	UsedBy []RuleRef `json:"usedBy"`
}

const scheduleCols = `id, name, description, windows_json, timezone, updated_by, updated_by_name, created_at, updated_at`

func scanSchedule(row interface{ Scan(...any) error }) (rules.Schedule, error) {
	var (
		s                    rules.Schedule
		windows              string
		createdAt, updatedAt sql.NullString
	)
	if err := row.Scan(&s.ID, &s.Name, &s.Description, &windows, &s.Timezone, &s.UpdatedBy, &s.UpdatedByName, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return s, ErrNotFound
		}
		return s, err
	}
	if err := json.Unmarshal([]byte(windows), &s.Windows); err != nil {
		return s, fmt.Errorf("schedule %d windows: %w", s.ID, err)
	}
	s.CreatedAt, s.UpdatedAt = parseTime(createdAt), parseTime(updatedAt)
	return s, nil
}

// Schedule returns one schedule. The engine uses it to check rules' hours.
func (s *Store) Schedule(ctx context.Context, id int64) (rules.Schedule, error) {
	return getSchedule(ctx, s.db, id)
}

func getSchedule(ctx context.Context, q querier, id int64) (rules.Schedule, error) {
	return scanSchedule(q.QueryRowContext(ctx, `SELECT `+scheduleCols+` FROM schedules WHERE id=?`, id))
}

// ListSchedules returns every schedule with the rules that use it.
func (s *Store) ListSchedules(ctx context.Context) ([]ScheduleView, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+scheduleCols+` FROM schedules ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	var list []rules.Schedule
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, sc)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	users, err := scheduleUsers(ctx, s.db)
	if err != nil {
		return nil, err
	}
	out := make([]ScheduleView, 0, len(list))
	for _, sc := range list {
		out = append(out, ScheduleView{Schedule: sc, UsedBy: orEmpty(users[sc.ID])})
	}
	return out, nil
}

func (s *Store) GetSchedule(ctx context.Context, id int64) (ScheduleView, error) {
	sc, err := s.Schedule(ctx, id)
	if err != nil {
		return ScheduleView{}, err
	}
	users, err := scheduleUsers(ctx, s.db)
	if err != nil {
		return ScheduleView{}, err
	}
	return ScheduleView{Schedule: sc, UsedBy: orEmpty(users[id])}, nil
}

func orEmpty(refs []RuleRef) []RuleRef {
	if refs == nil {
		return []RuleRef{}
	}
	return refs
}

// scheduleUsers maps schedule IDs to the rules that use them.
func scheduleUsers(ctx context.Context, q querier) (map[int64][]RuleRef, error) {
	rows, err := q.QueryContext(ctx, `SELECT schedule_id, id, name FROM rules WHERE schedule_id > 0 ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]RuleRef{}
	for rows.Next() {
		var (
			sid int64
			ref RuleRef
		)
		if err := rows.Scan(&sid, &ref.ID, &ref.Name); err != nil {
			return nil, err
		}
		out[sid] = append(out[sid], ref)
	}
	return out, rows.Err()
}

func scheduleBase(sc rules.Schedule) *revisionBase {
	return &revisionBase{item: sc, name: sc.Name, by: Editor{sc.UpdatedBy, sc.UpdatedByName}, at: sc.UpdatedAt.UTC().Format(time.RFC3339Nano)}
}

func (s *Store) CreateSchedule(ctx context.Context, sc rules.Schedule, by Editor) (ScheduleView, error) {
	if err := sc.Validate(); err != nil {
		return ScheduleView{}, ValidationError{err}
	}
	windows, _ := json.Marshal(sc.Windows)
	var id int64
	err := s.inTx(ctx, func(q querier) error {
		ts := now()
		res, err := q.ExecContext(ctx,
			`INSERT INTO schedules (name, description, windows_json, timezone, updated_by, updated_by_name, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?)`,
			sc.Name, sc.Description, string(windows), sc.Timezone, by.ID, by.Name, ts, ts)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		created, err := getSchedule(ctx, q, id)
		if err != nil {
			return err
		}
		return record(ctx, q, KindSchedule, id, created.Name, ChangeCreated, created, by, nil)
	})
	if err != nil {
		return ScheduleView{}, err
	}
	return s.GetSchedule(ctx, id)
}

func (s *Store) UpdateSchedule(ctx context.Context, sc rules.Schedule, by Editor) (ScheduleView, error) {
	if err := sc.Validate(); err != nil {
		return ScheduleView{}, ValidationError{err}
	}
	windows, _ := json.Marshal(sc.Windows)
	err := s.inTx(ctx, func(q querier) error {
		before, err := getSchedule(ctx, q, sc.ID)
		if err != nil {
			return err
		}
		if err := checkFresh(KindSchedule, sc.UpdatedAt, before.UpdatedAt, before.UpdatedByName, before); err != nil {
			return err
		}
		was, err := stamp(ctx, q, "schedules", sc.ID)
		if err != nil {
			return err
		}
		res, err := q.ExecContext(ctx,
			`UPDATE schedules SET name=?, description=?, windows_json=?, timezone=?, updated_by=?, updated_by_name=?, updated_at=? WHERE id=? AND updated_at=?`,
			sc.Name, sc.Description, string(windows), sc.Timezone, by.ID, by.Name, now(), sc.ID, was)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ConflictError{Kind: KindSchedule, Current: before, By: before.UpdatedByName, At: before.UpdatedAt}
		}
		updated, err := getSchedule(ctx, q, sc.ID)
		if err != nil {
			return err
		}
		return record(ctx, q, KindSchedule, sc.ID, updated.Name, ChangeUpdated, updated, by, scheduleBase(before))
	})
	if err != nil {
		return ScheduleView{}, err
	}
	return s.GetSchedule(ctx, sc.ID)
}

// DeleteSchedule refuses (ErrInUse) while rules still use the schedule. The
// check and the delete share a transaction.
func (s *Store) DeleteSchedule(ctx context.Context, id int64, by Editor) error {
	return s.inTx(ctx, func(q querier) error {
		users, err := scheduleUsers(ctx, q)
		if err != nil {
			return err
		}
		if used := users[id]; len(used) > 0 {
			return ErrInUse{Rules: used}
		}
		before, err := getSchedule(ctx, q, id)
		if err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `DELETE FROM schedules WHERE id=?`, id); err != nil {
			return err
		}
		return record(ctx, q, KindSchedule, id, before.Name, ChangeDeleted, before, by, nil)
	})
}
