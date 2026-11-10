package source

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"llmwatch/internal/usage"

	_ "modernc.org/sqlite"
)

const (
	// codexName is the source identifier.
	codexName = "codex"
	// codexQuery selects the threads of the codex database that consumed
	// tokens, using the millisecond timestamp when available.
	codexQuery = `SELECT COALESCE(updated_at_ms, updated_at*1000), model_provider, COALESCE(model,''), tokens_used FROM threads WHERE tokens_used > 0`
)

// Codex is a Source that reads the codex SQLite database, which keeps one row
// per thread with the provider, model and accumulated token usage.
//
// The database is opened in read-only mode and the connection is closed as
// soon as the load is done, so no lock is held on the file between loads.
type Codex struct {
	dbPath string
}

// NewCodex returns a Codex source reading the database at dbPath.
//
// The typical path is ~/.codex/state_5.sqlite.
func NewCodex(dbPath string) *Codex {
	return &Codex{dbPath: dbPath}
}

// Name returns the source identifier.
func (c *Codex) Name() string { return codexName }

// Fingerprint returns a token that changes whenever the database or its
// write-ahead log changes. It only stats the files, so it is cheap.
func (c *Codex) Fingerprint() (string, error) {
	return sqliteFingerprint(c.dbPath)
}

// Load returns all records known by the source.
func (c *Codex) Load(ctx context.Context) (recs []usage.Record, err error) {
	if _, err := os.Stat(c.dbPath); err != nil {
		if os.IsNotExist(err) {
			return []usage.Record{}, nil
		}
		return nil, fmt.Errorf("stat %s: %w", c.dbPath, err)
	}

	db, err := sql.Open("sqlite", "file:"+c.dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", c.dbPath, err)
	}
	defer func() {
		if cerr := db.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close %s: %w", c.dbPath, cerr))
			recs = nil
		}
	}()

	rows, err := db.QueryContext(ctx, codexQuery)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", c.dbPath, err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close rows %s: %w", c.dbPath, cerr))
			recs = nil
		}
	}()

	out := []usage.Record{}
	for rows.Next() {
		var (
			timeMillis int64
			provider   string
			model      string
			tokens     int64
		)
		if err := rows.Scan(&timeMillis, &provider, &model, &tokens); err != nil {
			return nil, fmt.Errorf("scan %s: %w", c.dbPath, err)
		}

		out = append(out, usage.Record{
			Source:   codexName,
			Provider: provider,
			Model:    model,
			Time:     time.UnixMilli(timeMillis),
			Input:    tokens,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows %s: %w", c.dbPath, err)
	}

	return out, nil
}
