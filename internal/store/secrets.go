package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SecretMeta is everything about a secret except its value. Values never
// leave the store except as ciphertext to the secrets manager.
type SecretMeta struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// UsedBy lists rules owned by the same admin that reference this secret.
	UsedBy []string `json:"usedBy"`
}

func (s *Store) ListSecrets(ctx context.Context, ownerID string) ([]SecretMeta, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, created_at, updated_at FROM secrets WHERE owner_id=? ORDER BY name`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SecretMeta{}
	for rows.Next() {
		var (
			m    SecretMeta
			c, u sql.NullString
		)
		if err := rows.Scan(&m.Name, &c, &u); err != nil {
			return nil, err
		}
		m.CreatedAt, m.UpdatedAt = parseTime(c), parseTime(u)
		m.UsedBy = []string{}
		out = append(out, m)
	}
	return out, rows.Err()
}

// PutSecret creates or replaces ownerID's secret.
func (s *Store) PutSecret(ctx context.Context, ownerID, ownerName, name string, ciphertext []byte) error {
	ts := now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO secrets (owner_id, owner_name, name, ciphertext, created_at, updated_at) VALUES (?,?,?,?,?,?)
		 ON CONFLICT(owner_id, name) DO UPDATE SET ciphertext=excluded.ciphertext, owner_name=excluded.owner_name, updated_at=excluded.updated_at`,
		ownerID, ownerName, name, ciphertext, ts, ts)
	return err
}

func (s *Store) DeleteSecret(ctx context.Context, ownerID, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE owner_id=? AND name=?`, ownerID, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SecretCiphertext returns the sealed value, or ErrNotFound.
func (s *Store) SecretCiphertext(ctx context.Context, ownerID, name string) ([]byte, error) {
	var ct []byte
	err := s.db.QueryRowContext(ctx, `SELECT ciphertext FROM secrets WHERE owner_id=? AND name=?`, ownerID, name).Scan(&ct)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return ct, err
}

func (s *Store) HasSecret(ctx context.Context, ownerID, name string) (bool, error) {
	_, err := s.SecretCiphertext(ctx, ownerID, name)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}
