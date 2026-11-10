package source

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"llmwatch/internal/usage"

	_ "modernc.org/sqlite"
)

const (
	opencodeSchema = `CREATE TABLE message (
		id text PRIMARY KEY,
		session_id text NOT NULL,
		time_created integer NOT NULL,
		time_updated integer NOT NULL,
		data text NOT NULL
	)`
	codexSchema = `CREATE TABLE threads (
		id TEXT PRIMARY KEY,
		updated_at INTEGER NOT NULL,
		model_provider TEXT NOT NULL,
		tokens_used INTEGER NOT NULL DEFAULT 0,
		model TEXT,
		updated_at_ms INTEGER
	)`
	opencodeSessionSchema = `CREATE TABLE session_message (
		id text PRIMARY KEY,
		session_id text NOT NULL,
		type text NOT NULL,
		seq integer NOT NULL,
		time_created integer NOT NULL,
		time_updated integer NOT NULL,
		data text NOT NULL
	)`
)

// execDB runs the statements against the database at path, opening it in
// read/write mode. It is used by the tests to build fake databases.
func execDB(t *testing.T, path string, stmts ...string) {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() {
		if cerr := db.Close(); cerr != nil {
			t.Fatalf("close %s: %v", path, cerr)
		}
	}()

	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}

// insertOpencodeMessage inserts one message row in the opencode schema.
func insertOpencodeMessage(t *testing.T, path, id, data string, timeCreated int64) {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() {
		if cerr := db.Close(); cerr != nil {
			t.Fatalf("close %s: %v", path, cerr)
		}
	}()

	stmt := `INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`
	if _, err := db.Exec(stmt, id, "ses_1", timeCreated, timeCreated, data); err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
}

// insertOpencodeSessionMessage inserts one session_message row in the
// opencode v2 schema.
func insertOpencodeSessionMessage(t *testing.T, path, id, msgType, data string, timeCreated int64) {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() {
		if cerr := db.Close(); cerr != nil {
			t.Fatalf("close %s: %v", path, cerr)
		}
	}()

	stmt := `INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data) ` +
		`VALUES (?, ?, ?, ?, ?, ?, ?)`
	if _, err := db.Exec(stmt, id, "ses_1", msgType, 1, timeCreated, timeCreated, data); err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
}

// insertCodexThread inserts one thread row in the codex schema.
func insertCodexThread(t *testing.T, path, id string, updatedAt int64, provider string, tokens int64, model any, updatedAtMs any) {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() {
		if cerr := db.Close(); cerr != nil {
			t.Fatalf("close %s: %v", path, cerr)
		}
	}()

	stmt := `INSERT INTO threads (id, updated_at, model_provider, tokens_used, model, updated_at_ms) VALUES (?, ?, ?, ?, ?, ?)`
	if _, err := db.Exec(stmt, id, updatedAt, provider, tokens, model, updatedAtMs); err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
}

// opencodeData builds the JSON payload of an opencode assistant message.
func opencodeData(provider, model string, cost float64, input, output, reasoning, cacheRead, cacheWrite int64) string {
	return fmt.Sprintf(
		`{"role":"assistant","providerID":%q,"modelID":%q,"cost":%g,`+
			`"tokens":{"input":%d,"output":%d,"reasoning":%d,"cache":{"read":%d,"write":%d}}}`,
		provider, model, cost, input, output, reasoning, cacheRead, cacheWrite)
}

// opencodeSessionData builds the JSON payload of an opencode v2 assistant
// message, where the model identity is nested under the "model" key.
func opencodeSessionData(provider, model string, cost float64, input, output, reasoning, cacheRead, cacheWrite int64) string {
	return fmt.Sprintf(
		`{"model":{"id":%q,"providerID":%q},"cost":%g,`+
			`"tokens":{"input":%d,"output":%d,"reasoning":%d,"cache":{"read":%d,"write":%d}}}`,
		model, provider, cost, input, output, reasoning, cacheRead, cacheWrite)
}

func TestOpencodeName(t *testing.T) {
	o := NewOpencode(filepath.Join(t.TempDir(), "opencode.db"))
	if got := o.Name(); got != "opencode" {
		t.Fatalf("Name() = %q, want %q", got, "opencode")
	}
}

func TestOpencodeMapsFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execDB(t, path, opencodeSchema)

	const created = int64(1_700_000_000_123)
	insertOpencodeMessage(t, path, "msg_1",
		opencodeData("anthropic", "claude-sonnet-4", 0.0125, 100, 50, 20, 30, 40), created)

	o := NewOpencode(path)
	recs, err := o.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(recs), recs)
	}

	got := recs[0]
	if got.Source != "opencode" {
		t.Fatalf("source = %q, want opencode", got.Source)
	}
	if got.Provider != "anthropic" {
		t.Fatalf("provider = %q, want anthropic", got.Provider)
	}
	if got.Model != "claude-sonnet-4" {
		t.Fatalf("model = %q, want claude-sonnet-4", got.Model)
	}
	if got.Input != 100 || got.Output != 50 || got.Reasoning != 20 {
		t.Fatalf("unexpected token counters: %+v", got)
	}
	if got.CacheRead != 30 || got.CacheWrite != 40 {
		t.Fatalf("unexpected cache counters: %+v", got)
	}
	if got.Cost != 0.0125 {
		t.Fatalf("cost = %v, want 0.0125", got.Cost)
	}
	want := time.UnixMilli(created)
	if !got.Time.Equal(want) {
		t.Fatalf("time = %s, want %s", got.Time, want)
	}
}

func TestOpencodeIgnoresNonAssistantMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execDB(t, path, opencodeSchema)

	// A user message with the same payload shape must be ignored.
	user := `{"role":"user","providerID":"anthropic","modelID":"claude-sonnet-4","cost":0.5,` +
		`"tokens":{"input":999,"output":999,"reasoning":999,"cache":{"read":999,"write":999}}}`
	insertOpencodeMessage(t, path, "msg_user", user, 1_700_000_000_000)
	insertOpencodeMessage(t, path, "msg_asst",
		opencodeData("anthropic", "claude-sonnet-4", 0.1, 10, 5, 0, 0, 0), 1_700_000_000_001)

	o := NewOpencode(path)
	recs, err := o.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(recs), recs)
	}
	if recs[0].Input != 10 {
		t.Fatalf("unexpected record: %+v", recs[0])
	}
}

func TestOpencodeIgnoresInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execDB(t, path, opencodeSchema)

	insertOpencodeMessage(t, path, "msg_bad", "{not json at all", 1_700_000_000_000)
	insertOpencodeMessage(t, path, "msg_ok",
		opencodeData("openai", "gpt-5", 0.2, 7, 3, 0, 0, 0), 1_700_000_000_001)

	o := NewOpencode(path)
	recs, err := o.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(recs), recs)
	}
	if recs[0].Model != "gpt-5" {
		t.Fatalf("unexpected record: %+v", recs[0])
	}
}

func TestOpencodeIgnoresZeroTokenMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execDB(t, path, opencodeSchema)

	insertOpencodeMessage(t, path, "msg_zero",
		opencodeData("anthropic", "claude-sonnet-4", 0.1, 0, 0, 0, 0, 0), 1_700_000_000_000)
	// A message carrying only cache tokens is kept: the total is not zero.
	insertOpencodeMessage(t, path, "msg_cache",
		opencodeData("anthropic", "claude-sonnet-4", 0.1, 0, 0, 0, 5, 0), 1_700_000_000_001)

	o := NewOpencode(path)
	recs, err := o.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(recs), recs)
	}
	if recs[0].CacheRead != 5 {
		t.Fatalf("unexpected record: %+v", recs[0])
	}
}

func TestOpencodeMissingFile(t *testing.T) {
	o := NewOpencode(filepath.Join(t.TempDir(), "does-not-exist.db"))

	recs, err := o.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if recs == nil {
		t.Fatal("Load returned nil, want an empty slice")
	}
	if len(recs) != 0 {
		t.Fatalf("got %d records, want 0: %+v", len(recs), recs)
	}

	fp, err := o.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if fp != "" {
		t.Fatalf("fingerprint = %q, want empty", fp)
	}
}

func TestCodexName(t *testing.T) {
	c := NewCodex(filepath.Join(t.TempDir(), "state.sqlite"))
	if got := c.Name(); got != "codex" {
		t.Fatalf("Name() = %q, want %q", got, "codex")
	}
}

func TestCodexMapsFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	execDB(t, path, codexSchema)

	insertCodexThread(t, path, "thr_1", 1_700_000_000, "openai", 1234, "gpt-5-codex", 1_700_000_500_000)

	c := NewCodex(path)
	recs, err := c.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(recs), recs)
	}

	got := recs[0]
	if got.Source != "codex" {
		t.Fatalf("source = %q, want codex", got.Source)
	}
	if got.Provider != "openai" {
		t.Fatalf("provider = %q, want openai", got.Provider)
	}
	if got.Model != "gpt-5-codex" {
		t.Fatalf("model = %q, want gpt-5-codex", got.Model)
	}
	if got.Input != 1234 {
		t.Fatalf("input = %d, want 1234", got.Input)
	}
	if got.Output != 0 || got.CacheRead != 0 || got.CacheWrite != 0 || got.Reasoning != 0 {
		t.Fatalf("unexpected token counters: %+v", got)
	}
	want := time.UnixMilli(1_700_000_500_000)
	if !got.Time.Equal(want) {
		t.Fatalf("time = %s, want %s", got.Time, want)
	}
}

func TestCodexFallsBackToUpdatedAtSeconds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	execDB(t, path, codexSchema)

	// updated_at_ms is NULL: updated_at (seconds) must be used instead.
	insertCodexThread(t, path, "thr_1", 1_700_000_000, "openai", 42, "gpt-5-codex", nil)

	c := NewCodex(path)
	recs, err := c.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(recs), recs)
	}
	want := time.UnixMilli(1_700_000_000_000)
	if !recs[0].Time.Equal(want) {
		t.Fatalf("time = %s, want %s", recs[0].Time, want)
	}
}

func TestCodexIgnoresZeroTokenThreads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	execDB(t, path, codexSchema)

	insertCodexThread(t, path, "thr_zero", 1_700_000_000, "openai", 0, "gpt-5-codex", nil)
	insertCodexThread(t, path, "thr_ok", 1_700_000_001, "openai", 10, "gpt-5-codex", nil)

	c := NewCodex(path)
	recs, err := c.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(recs), recs)
	}
	if recs[0].Input != 10 {
		t.Fatalf("unexpected record: %+v", recs[0])
	}
}

func TestCodexHandlesNullModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	execDB(t, path, codexSchema)

	insertCodexThread(t, path, "thr_1", 1_700_000_000, "openai", 5, nil, nil)

	c := NewCodex(path)
	recs, err := c.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(recs), recs)
	}
	if recs[0].Model != "" {
		t.Fatalf("model = %q, want empty", recs[0].Model)
	}
}

func TestCodexMissingFile(t *testing.T) {
	c := NewCodex(filepath.Join(t.TempDir(), "does-not-exist.sqlite"))

	recs, err := c.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if recs == nil {
		t.Fatal("Load returned nil, want an empty slice")
	}
	if len(recs) != 0 {
		t.Fatalf("got %d records, want 0: %+v", len(recs), recs)
	}

	fp, err := c.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if fp != "" {
		t.Fatalf("fingerprint = %q, want empty", fp)
	}
}

func TestOpencodeFingerprintChangesAfterInsert(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execDB(t, path, opencodeSchema)
	insertOpencodeMessage(t, path, "msg_1",
		opencodeData("anthropic", "claude-sonnet-4", 0.1, 10, 5, 0, 0, 0), 1_700_000_000_000)

	o := NewOpencode(path)
	fp1, err := o.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if fp1 == "" {
		t.Fatal("expected a non-empty fingerprint")
	}

	time.Sleep(10 * time.Millisecond)
	insertOpencodeMessage(t, path, "msg_2",
		opencodeData("anthropic", "claude-sonnet-4", 0.1, 20, 5, 0, 0, 0), 1_700_000_000_001)

	fp2, err := o.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if fp1 == fp2 {
		t.Fatalf("fingerprint did not change after insert: %q", fp2)
	}
}

func TestCodexFingerprintChangesAfterInsert(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	execDB(t, path, codexSchema)
	insertCodexThread(t, path, "thr_1", 1_700_000_000, "openai", 10, "gpt-5-codex", nil)

	c := NewCodex(path)
	fp1, err := c.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if fp1 == "" {
		t.Fatal("expected a non-empty fingerprint")
	}

	time.Sleep(10 * time.Millisecond)
	insertCodexThread(t, path, "thr_2", 1_700_000_001, "openai", 20, "gpt-5-codex", nil)

	fp2, err := c.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if fp1 == fp2 {
		t.Fatalf("fingerprint did not change after insert: %q", fp2)
	}
}

func TestOpencodeFingerprintSeesWALFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execDB(t, path, opencodeSchema)

	o := NewOpencode(path)
	fp1, err := o.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if fp1 == "" {
		t.Fatal("expected a non-empty fingerprint")
	}

	// A write-ahead log file next to the database changes the state of the
	// source and must be reflected in the fingerprint.
	wal := path + "-wal"
	if err := os.WriteFile(wal, []byte("wal"), 0o644); err != nil {
		t.Fatalf("write wal: %v", err)
	}

	fp2, err := o.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if fp1 == fp2 {
		t.Fatalf("fingerprint did not change when the wal file appeared: %q", fp2)
	}
}

func TestOpencodeImplementsSource(t *testing.T) {
	var _ Source = NewOpencode(filepath.Join(t.TempDir(), "opencode.db"))
}

func TestCodexImplementsSource(t *testing.T) {
	var _ Source = NewCodex(filepath.Join(t.TempDir(), "state.sqlite"))
}

func TestOpencodeLoadsSessionMessageOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	// Only the v2 table exists: the legacy one is absent.
	execDB(t, path, opencodeSessionSchema)

	const created = int64(1_700_000_000_123)
	insertOpencodeSessionMessage(t, path, "smsg_1", "assistant",
		opencodeSessionData("minimax-coding-plan", "MiniMax-M3", 0, 255, 790, 0, 32183, 0), created)

	o := NewOpencode(path)
	recs, err := o.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(recs), recs)
	}

	got := recs[0]
	if got.Source != "opencode" {
		t.Fatalf("source = %q, want opencode", got.Source)
	}
	if got.Provider != "minimax-coding-plan" {
		t.Fatalf("provider = %q, want minimax-coding-plan", got.Provider)
	}
	if got.Model != "MiniMax-M3" {
		t.Fatalf("model = %q, want MiniMax-M3", got.Model)
	}
	if got.Input != 255 || got.Output != 790 || got.Reasoning != 0 {
		t.Fatalf("unexpected token counters: %+v", got)
	}
	if got.CacheRead != 32183 || got.CacheWrite != 0 {
		t.Fatalf("unexpected cache counters: %+v", got)
	}
	want := time.UnixMilli(created)
	if !got.Time.Equal(want) {
		t.Fatalf("time = %s, want %s", got.Time, want)
	}
}

func TestOpencodeLoadsBothTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execDB(t, path, opencodeSchema, opencodeSessionSchema)

	insertOpencodeMessage(t, path, "msg_1",
		opencodeData("anthropic", "claude-sonnet-4", 0.1, 10, 5, 0, 0, 0), 1_700_000_000_000)
	insertOpencodeSessionMessage(t, path, "smsg_1", "assistant",
		opencodeSessionData("minimax-coding-plan", "MiniMax-M3", 0, 255, 790, 0, 32183, 0), 1_700_000_001_000)

	o := NewOpencode(path)
	recs, err := o.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2: %+v", len(recs), recs)
	}

	byModel := map[string]usage.Record{}
	for _, rec := range recs {
		byModel[rec.Model] = rec
	}

	legacy, ok := byModel["claude-sonnet-4"]
	if !ok {
		t.Fatalf("legacy record missing: %+v", recs)
	}
	if legacy.Provider != "anthropic" || legacy.Input != 10 || legacy.Output != 5 {
		t.Fatalf("unexpected legacy record: %+v", legacy)
	}

	session, ok := byModel["MiniMax-M3"]
	if !ok {
		t.Fatalf("session record missing: %+v", recs)
	}
	if session.Provider != "minimax-coding-plan" || session.Input != 255 || session.Output != 790 {
		t.Fatalf("unexpected session record: %+v", session)
	}
	if session.CacheRead != 32183 {
		t.Fatalf("unexpected session cache counters: %+v", session)
	}
}

func TestOpencodeIgnoresNonAssistantSessionMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execDB(t, path, opencodeSessionSchema)

	// A user row with the same payload shape must be ignored.
	insertOpencodeSessionMessage(t, path, "smsg_user", "user",
		opencodeSessionData("minimax-coding-plan", "MiniMax-M3", 0, 999, 999, 999, 999, 999), 1_700_000_000_000)
	insertOpencodeSessionMessage(t, path, "smsg_asst", "assistant",
		opencodeSessionData("minimax-coding-plan", "MiniMax-M3", 0, 10, 5, 0, 0, 0), 1_700_000_000_001)

	o := NewOpencode(path)
	recs, err := o.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(recs), recs)
	}
	if recs[0].Input != 10 {
		t.Fatalf("unexpected record: %+v", recs[0])
	}
}

func TestOpencodeIgnoresInvalidAndZeroTokenSessionMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execDB(t, path, opencodeSessionSchema)

	insertOpencodeSessionMessage(t, path, "smsg_bad", "assistant", "{not json at all", 1_700_000_000_000)
	insertOpencodeSessionMessage(t, path, "smsg_zero", "assistant",
		opencodeSessionData("minimax-coding-plan", "MiniMax-M3", 0, 0, 0, 0, 0, 0), 1_700_000_000_001)
	insertOpencodeSessionMessage(t, path, "smsg_ok", "assistant",
		opencodeSessionData("minimax-coding-plan", "MiniMax-M3", 0, 7, 3, 0, 0, 0), 1_700_000_000_002)

	o := NewOpencode(path)
	recs, err := o.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(recs), recs)
	}
	if recs[0].Input != 7 || recs[0].Output != 3 {
		t.Fatalf("unexpected record: %+v", recs[0])
	}
}

func TestOpencodeMissingTablesReturnsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	execDB(t, path, `CREATE TABLE unrelated (id text)`)

	o := NewOpencode(path)
	recs, err := o.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if recs == nil {
		t.Fatal("Load returned nil, want an empty slice")
	}
	if len(recs) != 0 {
		t.Fatalf("got %d records, want 0: %+v", len(recs), recs)
	}
}
