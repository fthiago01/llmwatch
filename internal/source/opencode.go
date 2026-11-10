package source

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"llmwatch/internal/usage"

	_ "modernc.org/sqlite"
)

const (
	// opencodeName is the source identifier.
	opencodeName = "opencode"
	// opencodeQuery selects the assistant messages of the legacy opencode
	// database.
	//
	// The json_valid guard is required: json_extract raises an error on
	// malformed JSON, which would abort the whole query instead of skipping
	// the broken rows.
	opencodeQuery = `SELECT time_created, data FROM message WHERE json_valid(data) AND json_extract(data,'$.role')='assistant'`
	// opencodeSessionQuery selects the assistant messages of the opencode v2
	// database, which stores them in the session_message table.
	opencodeSessionQuery = `SELECT time_created, data FROM session_message WHERE type='assistant' AND json_valid(data)`
	// opencodeTableExistsQuery checks whether a table is present in the
	// database schema.
	opencodeTableExistsQuery = `SELECT name FROM sqlite_master WHERE type='table' AND name=?`
)

// opencodeMessage is the subset of the legacy message payload that carries
// usage data.
type opencodeMessage struct {
	ProviderID string  `json:"providerID"`
	ModelID    string  `json:"modelID"`
	Cost       float64 `json:"cost"`
	Tokens     struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
}

// opencodeSessionMessage is the subset of the session_message payload that
// carries usage data.
//
// The v2 schema nests the model identity under "model" and keeps the same
// token counters as the legacy payload.
type opencodeSessionMessage struct {
	Model struct {
		ID         string `json:"id"`
		ProviderID string `json:"providerID"`
	} `json:"model"`
	Cost   float64 `json:"cost"`
	Tokens struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
}

// Opencode is a Source that reads the opencode SQLite database, which keeps
// one row per message with the model, provider and token usage of the
// assistant turns.
//
// The database is opened in read-only mode and the connection is closed as
// soon as the load is done, so no lock is held on the file between loads.
type Opencode struct {
	dbPath string
}

// NewOpencode returns an Opencode source reading the database at dbPath.
//
// The typical path is ~/.local/share/opencode/opencode.db.
func NewOpencode(dbPath string) *Opencode {
	return &Opencode{dbPath: dbPath}
}

// Name returns the source identifier.
func (o *Opencode) Name() string { return opencodeName }

// Fingerprint returns a token that changes whenever the database or its
// write-ahead log changes. It only stats the files, so it is cheap.
func (o *Opencode) Fingerprint() (string, error) {
	return sqliteFingerprint(o.dbPath)
}

// Load returns all records known by the source.
//
// Both the legacy "message" table and the v2 "session_message" table are
// read and their records concatenated, so a database migrated from v1 to v2
// does not lose the older usage. A missing table is not an error: it simply
// contributes no record.
func (o *Opencode) Load(ctx context.Context) (recs []usage.Record, err error) {
	if _, err := os.Stat(o.dbPath); err != nil {
		if os.IsNotExist(err) {
			return []usage.Record{}, nil
		}
		return nil, fmt.Errorf("stat %s: %w", o.dbPath, err)
	}

	db, err := sql.Open("sqlite", "file:"+o.dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", o.dbPath, err)
	}
	defer func() {
		if cerr := db.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close %s: %w", o.dbPath, cerr))
			recs = nil
		}
	}()

	out := []usage.Record{}

	legacy, err := o.tableExists(ctx, db, "message")
	if err != nil {
		return nil, err
	}
	if legacy {
		recs, err := loadOpencodeRows(ctx, db, o.dbPath, opencodeQuery, parseOpencodeMessage)
		if err != nil {
			return nil, err
		}
		out = append(out, recs...)
	}

	session, err := o.tableExists(ctx, db, "session_message")
	if err != nil {
		return nil, err
	}
	if session {
		recs, err := loadOpencodeRows(ctx, db, o.dbPath, opencodeSessionQuery, parseOpencodeSessionMessage)
		if err != nil {
			return nil, err
		}
		out = append(out, recs...)
	}

	return out, nil
}

// tableExists reports whether the named table is present in the schema.
//
// A missing table is not an error: it means the database was written by a
// different opencode version.
func (o *Opencode) tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var found string
	err := db.QueryRowContext(ctx, opencodeTableExistsQuery, name).Scan(&found)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("query table %s in %s: %w", name, o.dbPath, err)
	}
}

// loadOpencodeRows runs the query and converts every row with parse.
//
// The returned slice is never nil.
func loadOpencodeRows(
	ctx context.Context,
	db *sql.DB,
	dbPath, query string,
	parse func(timeCreated int64, data string) (usage.Record, bool),
) ([]usage.Record, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", dbPath, err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close rows %s: %w", dbPath, cerr))
		}
	}()

	out := []usage.Record{}
	for rows.Next() {
		var (
			timeCreated int64
			data        string
		)
		if err := rows.Scan(&timeCreated, &data); err != nil {
			return nil, fmt.Errorf("scan %s: %w", dbPath, err)
		}

		rec, ok := parse(timeCreated, data)
		if !ok {
			continue
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows %s: %w", dbPath, err)
	}

	return out, nil
}

// parseOpencodeMessage converts one message row into a record.
//
// It returns false when the row must be ignored: the payload is not valid
// JSON or it carries no token at all.
func parseOpencodeMessage(timeCreated int64, data string) (usage.Record, bool) {
	var msg opencodeMessage
	if err := json.Unmarshal([]byte(data), &msg); err != nil {
		return usage.Record{}, false
	}

	rec := usage.Record{
		Source:     opencodeName,
		Provider:   msg.ProviderID,
		Model:      msg.ModelID,
		Time:       time.UnixMilli(timeCreated),
		Input:      msg.Tokens.Input,
		Output:     msg.Tokens.Output,
		Reasoning:  msg.Tokens.Reasoning,
		CacheRead:  msg.Tokens.Cache.Read,
		CacheWrite: msg.Tokens.Cache.Write,
		Cost:       msg.Cost,
	}

	if rec.Input == 0 && rec.Output == 0 && rec.Reasoning == 0 &&
		rec.CacheRead == 0 && rec.CacheWrite == 0 {
		return usage.Record{}, false
	}

	return rec, true
}

// parseOpencodeSessionMessage converts one session_message row into a record.
//
// It returns false when the row must be ignored: the payload is not valid
// JSON or it carries no token at all.
func parseOpencodeSessionMessage(timeCreated int64, data string) (usage.Record, bool) {
	var msg opencodeSessionMessage
	if err := json.Unmarshal([]byte(data), &msg); err != nil {
		return usage.Record{}, false
	}

	rec := usage.Record{
		Source:     opencodeName,
		Provider:   msg.Model.ProviderID,
		Model:      msg.Model.ID,
		Time:       time.UnixMilli(timeCreated),
		Input:      msg.Tokens.Input,
		Output:     msg.Tokens.Output,
		Reasoning:  msg.Tokens.Reasoning,
		CacheRead:  msg.Tokens.Cache.Read,
		CacheWrite: msg.Tokens.Cache.Write,
		Cost:       msg.Cost,
	}

	if rec.Input == 0 && rec.Output == 0 && rec.Reasoning == 0 &&
		rec.CacheRead == 0 && rec.CacheWrite == 0 {
		return usage.Record{}, false
	}

	return rec, true
}

// sqliteFingerprint returns a token that changes whenever the SQLite database
// at path or its write-ahead log changes.
//
// The token combines the size and the modification time of both files. It is
// empty when the database does not exist.
func sqliteFingerprint(path string) (string, error) {
	fp, exists, err := sqliteFileFingerprint(path)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", nil
	}

	wal, _, err := sqliteFileFingerprint(path + "-wal")
	if err != nil {
		return "", err
	}

	return fp + ":" + wal, nil
}

// sqliteFileFingerprint returns the "size:mtime" token of one file.
//
// It returns false when the file does not exist.
func sqliteFileFingerprint(path string) (string, bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("stat %s: %w", path, err)
	}
	if fi.IsDir() {
		return "", false, nil
	}

	token := strconv.FormatInt(fi.Size(), 10) + ":" + strconv.FormatInt(fi.ModTime().UnixNano(), 10)
	return token, true, nil
}
