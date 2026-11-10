// Package store keeps the llmwatch configuration, the authenticated sessions
// and the provider list in a SQLite database owned by the application.
//
// The database is separate from the databases read by the sources: llmwatch
// owns its own file and never writes to the databases of the monitored tools.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a record does not exist.
var ErrNotFound = errors.New("not found")

const (
	// schemaVersion is the version of the schema created by the migrations.
	schemaVersion = 1
	// dirPerm is the permission of the directories created for the
	// database.
	dirPerm = 0o700
	// filePerm is the permission of the database file.
	filePerm = 0o600
)

// dsnOptions are the pragmas applied to every connection of the database.
//
// The busy timeout makes the writers wait instead of failing right away,
// the write-ahead log keeps the readers from blocking the writers and the
// foreign keys enforcement is required by the schema.
const dsnOptions = "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"

// migrations holds the statements of every schema version, in order.
//
// The index of a migration is its version number minus one: the version 1
// is the initial schema.
var migrations = [][]string{
	{
		`CREATE TABLE settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE sessions (
			token_hash TEXT PRIMARY KEY,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL
		)`,
		`CREATE TABLE providers (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			type TEXT NOT NULL,
			name TEXT NOT NULL UNIQUE,
			enabled INTEGER NOT NULL DEFAULT 1,
			config TEXT NOT NULL DEFAULT '{}',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
	},
}

// DB is the llmwatch database.
//
// The underlying connection pool is limited to a single connection: SQLite
// serializes the writes anyway and a single connection avoids the lock
// contention between the concurrent statements of the application.
type DB struct {
	priv *sql.DB
}

// Open opens the database at path, creating it when it does not exist.
//
// The parent directory is created with the 0700 permission and the database
// file with the 0600 permission, since it holds the session tokens and the
// provider credentials.
//
// The call is idempotent: opening an existing database runs the pending
// migrations and keeps the stored data.
func Open(path string) (*DB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("create directory %s: %w", dir, err)
	}

	db, err := sql.Open("sqlite", "file:"+path+dsnOptions)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		if cerr := db.Close(); cerr != nil {
			return nil, errors.Join(
				fmt.Errorf("ping %s: %w", path, err),
				fmt.Errorf("close %s: %w", path, cerr),
			)
		}
		return nil, fmt.Errorf("ping %s: %w", path, err)
	}

	if err := chmod(path); err != nil {
		if cerr := db.Close(); cerr != nil {
			return nil, errors.Join(
				fmt.Errorf("chmod %s: %w", path, err),
				fmt.Errorf("close %s: %w", path, cerr),
			)
		}
		return nil, fmt.Errorf("chmod %s: %w", path, err)
	}

	if err := migrate(db); err != nil {
		if cerr := db.Close(); cerr != nil {
			return nil, errors.Join(
				fmt.Errorf("migrate %s: %w", path, err),
				fmt.Errorf("close %s: %w", path, cerr),
			)
		}
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}

	return &DB{priv: db}, nil
}

// chmod restricts the database file to the owner.
//
// The permission is ignored on Windows, which has no POSIX permission
// model for the file modes.
func chmod(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if err := os.Chmod(path, filePerm); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

// migrate applies the pending migrations to the database.
//
// The current schema version is kept in the user_version pragma, which is
// updated only after the statements of a version succeed.
func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("unsupported schema version %d, the maximum supported is %d", version, schemaVersion)
	}

	for v := version; v < schemaVersion; v++ {
		for _, stmt := range migrations[v] {
			if _, err := db.Exec(stmt); err != nil {
				return fmt.Errorf("apply migration %d: %w", v+1, err)
			}
		}
		if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v+1)); err != nil {
			return fmt.Errorf("set user_version to %d: %w", v+1, err)
		}
	}

	return nil
}

// Close releases the database connection.
func (d *DB) Close() error {
	if err := d.priv.Close(); err != nil {
		return fmt.Errorf("close database: %w", err)
	}
	return nil
}

// GetSetting returns the value stored under key.
//
// The second result reports whether the key exists.
func (d *DB) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := d.priv.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	switch {
	case err == nil:
		return value, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	default:
		return "", false, fmt.Errorf("query setting %s: %w", key, err)
	}
}

// SetSetting stores value under key, replacing any previous value.
func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := d.priv.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	if err != nil {
		return fmt.Errorf("set setting %s: %w", key, err)
	}
	return nil
}

// CreateSession stores an authenticated session.
//
// The token is stored hashed: the plain token never reaches the database.
func (d *DB) CreateSession(ctx context.Context, tokenHash string, expiresAt time.Time) error {
	now := time.Now()
	_, err := d.priv.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, created_at, expires_at) VALUES (?, ?, ?)`,
		tokenHash, now.Unix(), expiresAt.Unix())
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// SessionValid reports whether the session exists and is not expired yet.
func (d *DB) SessionValid(ctx context.Context, tokenHash string, now time.Time) (bool, error) {
	var expires int64
	err := d.priv.QueryRowContext(ctx,
		`SELECT expires_at FROM sessions WHERE token_hash = ?`, tokenHash).Scan(&expires)
	switch {
	case err == nil:
		return expires > now.Unix(), nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("query session: %w", err)
	}
}

// DeleteSession removes a session.
//
// Deleting a session that does not exist is not an error.
func (d *DB) DeleteSession(ctx context.Context, tokenHash string) error {
	if _, err := d.priv.ExecContext(ctx,
		`DELETE FROM sessions WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteAllSessions removes every session.
func (d *DB) DeleteAllSessions(ctx context.Context) error {
	if _, err := d.priv.ExecContext(ctx, `DELETE FROM sessions`); err != nil {
		return fmt.Errorf("delete sessions: %w", err)
	}
	return nil
}

// PurgeExpiredSessions removes the sessions expired at now.
func (d *DB) PurgeExpiredSessions(ctx context.Context, now time.Time) error {
	if _, err := d.priv.ExecContext(ctx,
		`DELETE FROM sessions WHERE expires_at <= ?`, now.Unix()); err != nil {
		return fmt.Errorf("purge expired sessions: %w", err)
	}
	return nil
}
