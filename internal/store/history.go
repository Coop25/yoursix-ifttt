package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
)

// Revision kinds.
const (
	KindRule     = "rule"
	KindAction   = "action" // a saved action (stored in the requests table)
	KindSchedule = "schedule"
)

// Revision changes.
const (
	ChangeCreated = "created"
	ChangeUpdated = "updated"
	ChangeDeleted = "deleted"
	// ChangeExisting is the state an item had before history was kept,
	// recorded the first time it changes so there's something to compare.
	ChangeExisting = "existing"
)

// Revision is one entry in the edit log: an item's editable content after a
// change (or, for a deletion, just before it).
type Revision struct {
	ID       int64           `json:"id"`
	Kind     string          `json:"kind"`
	ItemID   int64           `json:"itemId"`
	Name     string          `json:"name"`
	Change   string          `json:"change"`
	Snapshot json.RawMessage `json:"snapshot"`
	ByID     string          `json:"byId,omitempty"`
	ByName   string          `json:"byName,omitempty"`
	At       time.Time       `json:"at"`
}

// volatile fields aren't part of an item's content: identity, bookkeeping,
// and live state. They're left out of snapshots so diffs show real edits.
var volatile = []string{"id", "createdAt", "updatedAt", "updatedBy", "updatedByName", "lastFiredAt", "lastFinishedAt", "running", "activeRuns", "usedBy"}

// snapshot is v's editable content as JSON with sorted keys.
func snapshot(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	for _, k := range volatile {
		delete(m, k)
	}
	redact(m)
	return json.Marshal(m)
}

// Hidden replaces credentials typed straight into an action (not through
// {{ secret }}) in history snapshots. Anyone who can view the log can read
// them, and they'd outlive the edit or delete that removed them.
const Hidden = "‹hidden›"

// credentialHeaders are header names whose literal values are hidden.
var credentialHeaders = []string{"authorization", "proxy-authorization", "x-api-key"}

// redact hides literal passwords and credential headers anywhere in v.
// Templated values ({{ secret "X" }}) only name a secret, so they're kept.
func redact(v any) {
	literal := func(s any) bool {
		str, ok := s.(string)
		return ok && str != "" && !strings.Contains(str, "{{")
	}
	switch v := v.(type) {
	case map[string]any:
		for k, val := range v {
			switch {
			case k == "password" && literal(val):
				v[k] = Hidden
			case k == "headers":
				if hs, ok := val.(map[string]any); ok {
					for name, hv := range hs {
						if slices.Contains(credentialHeaders, strings.ToLower(name)) && literal(hv) {
							hs[name] = Hidden
						}
					}
				}
			default:
				redact(val)
			}
		}
	case []any:
		for _, e := range v {
			redact(e)
		}
	}
}

// record adds a revision for an item, in the caller's transaction. Updates that change nothing (compared
// with the latest revision) are skipped. before is the item's state prior
// to an update or delete: if the item has no history yet, it's logged first
// as ChangeExisting, attributed to whoever saved it last.
func record(ctx context.Context, q querier, kind string, id int64, name, change string, item any, by Editor, before *revisionBase) error {
	snap, err := snapshot(item)
	if err != nil {
		return err
	}
	var last []byte
	err = q.QueryRowContext(ctx, `SELECT snapshot_json FROM revisions WHERE kind=? AND item_id=? ORDER BY id DESC LIMIT 1`, kind, id).Scan(&last)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if before != nil {
			prev, err := snapshot(before.item)
			if err != nil {
				return err
			}
			if err := insertRevision(ctx, q, kind, id, before.name, ChangeExisting, prev, before.by, before.at); err != nil {
				return err
			}
			last = prev
		}
	case err != nil:
		return err
	}
	if change == ChangeUpdated && bytes.Equal(last, snap) {
		return nil
	}
	return insertRevision(ctx, q, kind, id, name, change, snap, by, now())
}

// revisionBase is an item's state before a change.
type revisionBase struct {
	item any
	name string
	by   Editor
	at   string
}

func insertRevision(ctx context.Context, q querier, kind string, id int64, name, change string, snap []byte, by Editor, at string) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO revisions (kind, item_id, name, change, snapshot_json, by_id, by_name, at) VALUES (?,?,?,?,?,?,?,?)`,
		kind, id, name, change, string(snap), by.ID, by.Name, at)
	return err
}

// HistoryFilter narrows ListRevisions. Zero values mean everything.
type HistoryFilter struct {
	Kind   string
	ItemID int64
	Before int64 // revision ID, for paging
	Limit  int
}

// ListRevisions returns revisions newest first.
func (s *Store) ListRevisions(ctx context.Context, f HistoryFilter) ([]Revision, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	q := `SELECT id, kind, item_id, name, change, snapshot_json, by_id, by_name, at FROM revisions WHERE 1=1`
	var args []any
	if f.Kind != "" {
		q += ` AND kind=?`
		args = append(args, f.Kind)
	}
	if f.ItemID > 0 {
		q += ` AND item_id=?`
		args = append(args, f.ItemID)
	}
	if f.Before > 0 {
		q += ` AND id<?`
		args = append(args, f.Before)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Revision{}
	for rows.Next() {
		var (
			r    Revision
			snap string
			at   sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.Kind, &r.ItemID, &r.Name, &r.Change, &snap, &r.ByID, &r.ByName, &at); err != nil {
			return nil, err
		}
		r.Snapshot = json.RawMessage(snap)
		r.At = parseTime(at)
		out = append(out, r)
	}
	return out, rows.Err()
}
