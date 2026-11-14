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
	//
	// The session id identifies the conversation and the directory of the
	// joined session is the project it belongs to.
	opencodeQuery = `SELECT m.time_created, m.data, m.session_id, COALESCE(s.directory,'') ` +
		`FROM message m LEFT JOIN session s ON s.id = m.session_id ` +
		`WHERE json_valid(m.data) AND json_extract(m.data,'$.role')='assistant'`
	// opencodeQueryNoSession selects the assistant messages of the legacy
	// opencode database when the session table is absent.
	//
	// It is used for the databases written by an older opencode version,
	// which have no session table to join: the project stays unknown.
	opencodeQueryNoSession = `SELECT time_created, data, session_id, '' ` +
		`FROM message WHERE json_valid(data) AND json_extract(data,'$.role')='assistant'`
	// opencodeSessionQuery selects the assistant messages of the opencode v2
	// database, which stores them in the session_message table.
	//
	// The session id of a v2 message references the session_v2 table, which
	// is a superset of the legacy session table: the newest sessions only
	// exist there. The directory of the joined session is the project the
	// message belongs to.
	opencodeSessionQuery = `SELECT sm.time_created, sm.data, sm.session_id, COALESCE(s.directory,'') ` +
		`FROM session_message sm LEFT JOIN session_v2 s ON s.id = sm.session_id ` +
		`WHERE sm.type='assistant' AND json_valid(sm.data)`
	// opencodeSessionQueryLegacy selects the assistant messages of the
	// opencode v2 database when the session_v2 table is absent.
	//
	// It falls back to the legacy session table, which is the only one
	// available in the databases written before the v2 sessions were split.
	opencodeSessionQueryLegacy = `SELECT sm.time_created, sm.data, sm.session_id, COALESCE(s.directory,'') ` +
		`FROM session_message sm LEFT JOIN session s ON s.id = sm.session_id ` +
		`WHERE sm.type='assistant' AND json_valid(sm.data)`
	// opencodeSessionQueryNoSession selects the assistant messages of the
	// opencode v2 database when no session table is available.
	//
	// It is used for the databases written by an older opencode version,
	// which have no session table to join: the project stays unknown.
	opencodeSessionQueryNoSession = `SELECT time_created, data, session_id, '' ` +
		`FROM session_message WHERE type='assistant' AND json_valid(data)`
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

	// The session tables map a session id to its working directory. They are
	// absent in the databases written by an older opencode version: the
	// records are still loaded, but their project stays unknown.
	//
	// The legacy messages reference the session table and the v2 messages
	// reference the session_v2 one, which holds the newest sessions too.
	hasSession, err := o.tableExists(ctx, db, "session")
	if err != nil {
		return nil, err
	}
	hasSessionV2, err := o.tableExists(ctx, db, "session_v2")
	if err != nil {
		return nil, err
	}

	legacy, err := o.tableExists(ctx, db, "message")
	if err != nil {
		return nil, err
	}
	if legacy {
		query := opencodeQuery
		if !hasSession {
			query = opencodeQueryNoSession
		}
		recs, err := loadOpencodeRows(ctx, db, o.dbPath, query, parseOpencodeMessage)
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
		query := opencodeSessionQueryNoSession
		switch {
		case hasSessionV2:
			query = opencodeSessionQuery
		case hasSession:
			query = opencodeSessionQueryLegacy
		}
		recs, err := loadOpencodeRows(ctx, db, o.dbPath, query, parseOpencodeSessionMessage)
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
	parse func(timeCreated int64, data, session, project string) (usage.Record, bool),
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
			session     string
			project     string
		)
		if err := rows.Scan(&timeCreated, &data, &session, &project); err != nil {
			return nil, fmt.Errorf("scan %s: %w", dbPath, err)
		}

		rec, ok := parse(timeCreated, data, session, project)
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
func parseOpencodeMessage(timeCreated int64, data, session, project string) (usage.Record, bool) {
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
		Session:    session,
		Project:    project,
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
func parseOpencodeSessionMessage(timeCreated int64, data, session, project string) (usage.Record, bool) {
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
		Session:    session,
		Project:    project,
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
