package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"yoursix-ifttt/internal/rules"
)

// querier is what both *sql.DB and *sql.Tx offer, so helpers can run
// inside or outside a transaction.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// inTx runs fn in a transaction, committing if it returns nil. Saves use it
// so an item's change, its freshness check and its history entry happen
// together or not at all. Inside fn, use only q: the store has a single
// connection, so touching s.db there would wait on the transaction forever.
func (s *Store) inTx(ctx context.Context, fn func(q querier) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// stamp is a row's stored updated_at, exactly as written, so an UPDATE can
// require it to be unchanged ("AND updated_at = ?").
func stamp(ctx context.Context, q querier, table string, id int64) (string, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT updated_at FROM `+table+` WHERE id=?`, id).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// checkRefs confirms, inside a rule's save, that the schedule and saved
// actions it points at still exist. Their deletes check for users in their
// own transactions, so together a rule can never point at nothing.
func checkRefs(ctx context.Context, q querier, r rules.Rule) error {
	if r.ScheduleID > 0 {
		if _, err := getSchedule(ctx, q, r.ScheduleID); errors.Is(err, ErrNotFound) {
			return ValidationError{fmt.Errorf("schedule #%d doesn't exist", r.ScheduleID)}
		} else if err != nil {
			return err
		}
	}
	for _, a := range r.AllActions() {
		if a.Type != rules.ActionRequest {
			continue
		}
		if _, err := savedRequest(ctx, q, a.RequestID); errors.Is(err, ErrNotFound) {
			return ValidationError{fmt.Errorf("saved action #%d doesn't exist", a.RequestID)}
		} else if err != nil {
			return err
		}
	}
	return nil
}
