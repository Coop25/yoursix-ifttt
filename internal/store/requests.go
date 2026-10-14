package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"yoursix-ifttt/internal/actions"
	"yoursix-ifttt/internal/rules"
)

// RequestView adds the rules that use a saved request, for the UI.
type RequestView struct {
	rules.SavedRequest
	UsedBy []RuleRef `json:"usedBy"`
}

type RuleRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

const requestCols = `id, name, description, params_json, request_json, updated_by, updated_by_name, created_at, updated_at`

func scanRequest(row interface{ Scan(...any) error }) (rules.SavedRequest, error) {
	var (
		sr                   rules.SavedRequest
		params, req          string
		createdAt, updatedAt sql.NullString
	)
	if err := row.Scan(&sr.ID, &sr.Name, &sr.Description, &params, &req, &sr.UpdatedBy, &sr.UpdatedByName, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sr, ErrNotFound
		}
		return sr, err
	}
	if err := json.Unmarshal([]byte(params), &sr.Params); err != nil {
		return sr, fmt.Errorf("request %d params: %w", sr.ID, err)
	}
	if err := json.Unmarshal([]byte(req), &sr.Request); err != nil {
		return sr, fmt.Errorf("request %d: %w", sr.ID, err)
	}
	sr.CreatedAt, sr.UpdatedAt = parseTime(createdAt), parseTime(updatedAt)
	return sr, nil
}

// SavedRequest returns one saved request. It also lets the store serve as
// the action runner's source of saved requests.
func (s *Store) SavedRequest(ctx context.Context, id int64) (rules.SavedRequest, error) {
	return scanRequest(s.db.QueryRowContext(ctx, `SELECT `+requestCols+` FROM requests WHERE id=?`, id))
}

// ListRequests returns every saved request with the rules that use it.
func (s *Store) ListRequests(ctx context.Context) ([]RequestView, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+requestCols+` FROM requests ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	var list []rules.SavedRequest
	for rows.Next() {
		sr, err := scanRequest(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, sr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	users, err := s.requestUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RequestView, 0, len(list))
	for _, sr := range list {
		used := users[sr.ID]
		if used == nil {
			used = []RuleRef{}
		}
		out = append(out, RequestView{SavedRequest: sr, UsedBy: used})
	}
	return out, nil
}

func (s *Store) GetRequest(ctx context.Context, id int64) (RequestView, error) {
	sr, err := s.SavedRequest(ctx, id)
	if err != nil {
		return RequestView{}, err
	}
	users, err := s.requestUsers(ctx)
	if err != nil {
		return RequestView{}, err
	}
	used := users[id]
	if used == nil {
		used = []RuleRef{}
	}
	return RequestView{SavedRequest: sr, UsedBy: used}, nil
}

// requestUsers maps saved-request IDs to the rules whose actions use them.
func (s *Store) requestUsers(ctx context.Context) (map[int64][]RuleRef, error) {
	all, err := s.ListRules(ctx)
	if err != nil {
		return nil, err
	}
	out := map[int64][]RuleRef{}
	for _, r := range all {
		seen := map[int64]bool{}
		for _, a := range r.Actions {
			if a.Type == rules.ActionRequest && !seen[a.RequestID] {
				seen[a.RequestID] = true
				out[a.RequestID] = append(out[a.RequestID], RuleRef{ID: r.ID, Name: r.Name})
			}
		}
	}
	return out, nil
}

func prepareRequest(sr *rules.SavedRequest) (params, req []byte, err error) {
	if err := sr.Validate(); err != nil {
		return nil, nil, ValidationError{err}
	}
	if err := actions.CheckTemplates([]rules.Action{sr.Request}); err != nil {
		return nil, nil, ValidationError{err}
	}
	if params, err = json.Marshal(sr.Params); err != nil {
		return nil, nil, err
	}
	req, err = json.Marshal(sr.Request)
	return params, req, err
}

func (s *Store) CreateRequest(ctx context.Context, sr rules.SavedRequest, by Editor) (RequestView, error) {
	params, req, err := prepareRequest(&sr)
	if err != nil {
		return RequestView{}, err
	}
	ts := now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO requests (name, description, params_json, request_json, updated_by, updated_by_name, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?)`,
		sr.Name, sr.Description, string(params), string(req), by.ID, by.Name, ts, ts)
	if err != nil {
		return RequestView{}, err
	}
	id, _ := res.LastInsertId()
	return s.GetRequest(ctx, id)
}

func (s *Store) UpdateRequest(ctx context.Context, sr rules.SavedRequest, by Editor) (RequestView, error) {
	params, req, err := prepareRequest(&sr)
	if err != nil {
		return RequestView{}, err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE requests SET name=?, description=?, params_json=?, request_json=?, updated_by=?, updated_by_name=?, updated_at=? WHERE id=?`,
		sr.Name, sr.Description, string(params), string(req), by.ID, by.Name, now(), sr.ID)
	if err != nil {
		return RequestView{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return RequestView{}, ErrNotFound
	}
	return s.GetRequest(ctx, sr.ID)
}

// ErrInUse is returned when deleting a saved request that rules still use.
type ErrInUse struct{ Rules []RuleRef }

func (e ErrInUse) Error() string {
	names := make([]string, len(e.Rules))
	for i, r := range e.Rules {
		names[i] = r.Name
	}
	return fmt.Sprintf("still used by %d rule(s): %v; remove it from them first", len(e.Rules), names)
}

func (s *Store) DeleteRequest(ctx context.Context, id int64) error {
	users, err := s.requestUsers(ctx)
	if err != nil {
		return err
	}
	if used := users[id]; len(used) > 0 {
		return ErrInUse{Rules: used}
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM requests WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
