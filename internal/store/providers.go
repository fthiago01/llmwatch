package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrDuplicateName is returned when a provider name is already taken.
var ErrDuplicateName = errors.New("provider name already exists")

// uniqueConstraintMsg is the marker of a UNIQUE violation in the error
// message of the SQLite driver.
const uniqueConstraintMsg = "UNIQUE constraint failed"

// Provider is a monitored LLM provider.
//
// The configuration holds the provider specific settings, such as the API
// key or the base URL.
type Provider struct {
	ID        int64             `json:"id"`
	Type      string            `json:"type"`
	Name      string            `json:"name"`
	Enabled   bool              `json:"enabled"`
	Config    map[string]string `json:"config"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// ListProviders returns every provider, ordered by name.
//
// The returned slice is never nil.
func (d *DB) ListProviders(ctx context.Context) ([]Provider, error) {
	rows, err := d.priv.QueryContext(ctx,
		`SELECT id, type, name, enabled, config, created_at, updated_at
		 FROM providers ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("query providers: %w", err)
	}
	defer func() {
		// The rows are closed by the deferred call, so the error of the
		// close is reported only when the scan did not fail.
		_ = rows.Close()
	}()

	out := []Provider{}
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows providers: %w", err)
	}

	return out, nil
}

// GetProvider returns the provider with the given id.
//
// It returns ErrNotFound when the provider does not exist.
func (d *DB) GetProvider(ctx context.Context, id int64) (Provider, error) {
	row := d.priv.QueryRowContext(ctx,
		`SELECT id, type, name, enabled, config, created_at, updated_at
		 FROM providers WHERE id = ?`, id)

	p, err := scanProvider(row)
	switch {
	case err == nil:
		return p, nil
	case errors.Is(err, sql.ErrNoRows):
		return Provider{}, fmt.Errorf("provider %d: %w", id, ErrNotFound)
	default:
		return Provider{}, err
	}
}

// CreateProvider stores a new provider.
//
// The id and the timestamps of the argument are ignored: the store assigns
// them.
//
// It returns ErrDuplicateName when the name is already taken.
func (d *DB) CreateProvider(ctx context.Context, p Provider) (Provider, error) {
	config, err := marshalConfig(p.Config)
	if err != nil {
		return Provider{}, err
	}

	now := time.Now()
	res, err := d.priv.ExecContext(ctx,
		`INSERT INTO providers (type, name, enabled, config, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		p.Type, p.Name, boolToInt(p.Enabled), config, now.Unix(), now.Unix())
	if err != nil {
		if isUniqueViolation(err) {
			return Provider{}, fmt.Errorf("provider %s: %w", p.Name, ErrDuplicateName)
		}
		return Provider{}, fmt.Errorf("create provider %s: %w", p.Name, err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return Provider{}, fmt.Errorf("last insert id of provider %s: %w", p.Name, err)
	}

	return Provider{
		ID:        id,
		Type:      p.Type,
		Name:      p.Name,
		Enabled:   p.Enabled,
		Config:    nonNilConfig(p.Config),
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// UpdateProvider replaces the fields of the provider with the given id.
//
// It returns ErrNotFound when the provider does not exist and
// ErrDuplicateName when the name is already taken by another provider.
func (d *DB) UpdateProvider(ctx context.Context, p Provider) (Provider, error) {
	config, err := marshalConfig(p.Config)
	if err != nil {
		return Provider{}, err
	}

	now := time.Now()
	res, err := d.priv.ExecContext(ctx,
		`UPDATE providers
		 SET type = ?, name = ?, enabled = ?, config = ?, updated_at = ?
		 WHERE id = ?`,
		p.Type, p.Name, boolToInt(p.Enabled), config, now.Unix(), p.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return Provider{}, fmt.Errorf("provider %s: %w", p.Name, ErrDuplicateName)
		}
		return Provider{}, fmt.Errorf("update provider %d: %w", p.ID, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return Provider{}, fmt.Errorf("rows affected of provider %d: %w", p.ID, err)
	}
	if affected == 0 {
		return Provider{}, fmt.Errorf("provider %d: %w", p.ID, ErrNotFound)
	}

	return Provider{
		ID:        p.ID,
		Type:      p.Type,
		Name:      p.Name,
		Enabled:   p.Enabled,
		Config:    nonNilConfig(p.Config),
		CreatedAt: p.CreatedAt,
		UpdatedAt: now,
	}, nil
}

// DeleteProvider removes the provider with the given id.
//
// It returns ErrNotFound when the provider does not exist.
func (d *DB) DeleteProvider(ctx context.Context, id int64) error {
	res, err := d.priv.ExecContext(ctx, `DELETE FROM providers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete provider %d: %w", id, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected of provider %d: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("provider %d: %w", id, ErrNotFound)
	}

	return nil
}

// CountProviders returns the number of stored providers.
func (d *DB) CountProviders(ctx context.Context) (int, error) {
	var count int
	if err := d.priv.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM providers`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count providers: %w", err)
	}
	return count, nil
}

// scanner reads the columns of a provider row.
//
// It is implemented by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// scanProvider converts one row into a provider.
func scanProvider(row scanner) (Provider, error) {
	var (
		p         Provider
		enabled   int
		config    string
		createdAt int64
		updatedAt int64
	)
	if err := row.Scan(&p.ID, &p.Type, &p.Name, &enabled, &config, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Provider{}, err
		}
		return Provider{}, fmt.Errorf("scan provider: %w", err)
	}

	p.Enabled = enabled != 0
	p.CreatedAt = time.Unix(createdAt, 0)
	p.UpdatedAt = time.Unix(updatedAt, 0)

	cfg, err := unmarshalConfig(config)
	if err != nil {
		return Provider{}, err
	}
	p.Config = cfg

	return p, nil
}

// marshalConfig serializes the provider configuration as JSON.
//
// A nil configuration is stored as an empty JSON object.
func marshalConfig(config map[string]string) (string, error) {
	if config == nil {
		return "{}", nil
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("marshal provider config: %w", err)
	}
	return string(raw), nil
}

// unmarshalConfig deserializes the provider configuration.
//
// An empty column yields an empty configuration. The result is never nil.
func unmarshalConfig(raw string) (map[string]string, error) {
	out := map[string]string{}
	if raw == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("unmarshal provider config: %w", err)
	}
	if out == nil {
		out = map[string]string{}
	}
	return out, nil
}

// nonNilConfig returns the configuration with a nil map replaced by an empty
// one, so the JSON encoding never carries a null.
func nonNilConfig(config map[string]string) map[string]string {
	if config == nil {
		return map[string]string{}
	}
	return config
}

// boolToInt converts a boolean into the integer stored in the database.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// isUniqueViolation reports whether the error is a UNIQUE constraint
// violation.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), uniqueConstraintMsg)
}
