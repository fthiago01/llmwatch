package source

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llmwatch/internal/usage"
)

// writeFile creates a file with the given content under dir.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// entry builds a transcript line with the given fields.
func entry(ts, id, model string, in, out, cacheWrite, cacheRead int64) string {
	return fmt.Sprintf(
		`{"timestamp":%q,"message":{"id":%q,"model":%q,"usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d}}}`,
		ts, id, model, in, out, cacheWrite, cacheRead)
}

func TestClaudeName(t *testing.T) {
	c := NewClaude(t.TempDir())
	if got := c.Name(); got != "claude" {
		t.Fatalf("Name() = %q, want %q", got, "claude")
	}
}

func TestClaudeDedupesByMessageID(t *testing.T) {
	root := t.TempDir()
	content := entry("2026-01-02T03:04:05.000Z", "msg_1", "claude-opus-4", 10, 5, 1, 2) + "\n" +
		entry("2026-01-02T03:04:06.000Z", "msg_1", "claude-opus-4", 100, 50, 11, 22) + "\n"
	writeFile(t, root, "proj/session.jsonl", content)

	c := NewClaude(root)
	recs, err := c.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(recs), recs)
	}
	got := recs[0]
	if got.Input != 100 || got.Output != 50 || got.CacheWrite != 11 || got.CacheRead != 22 {
		t.Fatalf("expected the last values of the id, got %+v", got)
	}
	if got.Source != "claude" || got.Provider != "anthropic" {
		t.Fatalf("unexpected source/provider: %+v", got)
	}
	if got.Model != "claude-opus-4" {
		t.Fatalf("model = %q, want claude-opus-4", got.Model)
	}
	want, _ := time.Parse(time.RFC3339Nano, "2026-01-02T03:04:06.000Z")
	if !got.Time.Equal(want) {
		t.Fatalf("time = %s, want %s", got.Time, want)
	}
	if got.Cost != 0 {
		t.Fatalf("cost = %v, want 0", got.Cost)
	}
}

func TestClaudeIgnoresSyntheticAndInvalidLines(t *testing.T) {
	root := t.TempDir()
	content := entry("2026-01-02T03:04:05.000Z", "synth", "<synthetic>", 10, 5, 0, 0) + "\n" +
		"{not json at all\n" +
		entry("2026-01-02T03:04:07.000Z", "ok", "claude-sonnet-4", 7, 3, 0, 0) + "\n"
	writeFile(t, root, "proj/session.jsonl", content)

	c := NewClaude(root)
	recs, err := c.Load(context.Background())
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

func TestClaudeIgnoresLinesWithoutUsage(t *testing.T) {
	root := t.TempDir()
	content := `{"timestamp":"2026-01-02T03:04:05.000Z","message":{"id":"x","model":"claude-opus-4"}}` + "\n" +
		`{"timestamp":"2026-01-02T03:04:05.000Z","type":"summary"}` + "\n"
	writeFile(t, root, "proj/session.jsonl", content)

	c := NewClaude(root)
	recs, err := c.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("got %d records, want 0: %+v", len(recs), recs)
	}
}

func TestClaudeIncrementalAppend(t *testing.T) {
	root := t.TempDir()
	first := entry("2026-01-02T03:04:05.000Z", "a", "claude-opus-4", 10, 5, 0, 0) + "\n"
	writeFile(t, root, "proj/session.jsonl", first)

	c := NewClaude(root)
	recs, err := c.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}

	second := entry("2026-01-02T03:05:05.000Z", "b", "claude-opus-4", 20, 8, 0, 0) + "\n"
	path := filepath.Join(root, "proj", "session.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString(second); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Close()

	recs, err = c.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2: %+v", len(recs), recs)
	}
	if recs[0].Input != 10 || recs[1].Input != 20 {
		t.Fatalf("unexpected records: %+v", recs)
	}
}

func TestClaudePartialLineIsNotConsumed(t *testing.T) {
	root := t.TempDir()
	first := entry("2026-01-02T03:04:05.000Z", "a", "claude-opus-4", 10, 5, 0, 0)
	second := entry("2026-01-02T03:04:06.000Z", "b", "claude-opus-4", 20, 5, 0, 0)
	writeFile(t, root, "proj/session.jsonl", first+"\n"+second[:20])

	c := NewClaude(root)
	recs, err := c.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1 (partial line ignored): %+v", len(recs), recs)
	}

	// Complete the partial line; it must be picked up.
	path := filepath.Join(root, "proj", "session.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString(second[20:] + "\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Close()

	recs, err = c.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2: %+v", len(recs), recs)
	}
	if recs[1].Input != 20 {
		t.Fatalf("unexpected record: %+v", recs[1])
	}
}

func TestClaudeFingerprintChangesOnAppend(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "proj/session.jsonl", entry("2026-01-02T03:04:05.000Z", "a", "m", 1, 1, 0, 0)+"\n")

	c := NewClaude(root)
	fp1, err := c.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if fp1 == "" {
		t.Fatal("expected a non-empty fingerprint")
	}

	time.Sleep(10 * time.Millisecond)
	f, err := os.OpenFile(filepath.Join(root, "proj", "session.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	f.WriteString(entry("2026-01-02T03:05:05.000Z", "b", "m", 1, 1, 0, 0) + "\n")
	f.Close()

	fp2, err := c.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if fp1 == fp2 {
		t.Fatalf("fingerprint did not change after append: %q", fp2)
	}
}

func TestClaudeFingerprintMissingRootIsEmpty(t *testing.T) {
	c := NewClaude(filepath.Join(t.TempDir(), "nope"))
	fp, err := c.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if fp != "" {
		t.Fatalf("fingerprint = %q, want empty", fp)
	}
}

func TestClaudeFingerprintCountsSubagentFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "proj/session.jsonl", entry("2026-01-02T03:04:05.000Z", "a", "m", 1, 1, 0, 0)+"\n")

	c := NewClaude(root)
	fp1, err := c.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	writeFile(t, root, "proj/sub/subagents/agent.jsonl", entry("2026-01-02T03:04:06.000Z", "b", "m", 1, 1, 0, 0)+"\n")

	fp2, err := c.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if fp1 == fp2 {
		t.Fatalf("fingerprint did not change when a subagent file appeared: %q", fp2)
	}
}

func TestClaudeMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does-not-exist")
	c := NewClaude(root)

	recs, err := c.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("got %d records, want 0", len(recs))
	}

	fp, err := c.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if fp != "" {
		t.Fatalf("fingerprint = %q, want empty", fp)
	}
}

func TestClaudeReadsSubagentFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "proj/session.jsonl", entry("2026-01-02T03:04:05.000Z", "a", "m", 1, 1, 0, 0)+"\n")
	writeFile(t, root, "proj/sub/subagents/agent.jsonl", entry("2026-01-02T03:04:07.000Z", "c", "m", 3, 3, 0, 0)+"\n")

	c := NewClaude(root)
	recs, err := c.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2: %+v", len(recs), recs)
	}
}

func TestClaudeTruncatedFileIsReread(t *testing.T) {
	root := t.TempDir()
	path := writeFile(t, root, "proj/session.jsonl",
		entry("2026-01-02T03:04:05.000Z", "a", "m", 10, 5, 0, 0)+"\n"+
			entry("2026-01-02T03:04:06.000Z", "b", "m", 20, 5, 0, 0)+"\n")

	c := NewClaude(root)
	recs, err := c.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}

	// Truncate the file: the previous state must be discarded.
	if err := os.WriteFile(path, []byte(entry("2026-01-02T03:04:07.000Z", "c", "m", 30, 5, 0, 0)+"\n"), 0o644); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	recs, err = c.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1 after truncation: %+v", len(recs), recs)
	}
	if recs[0].Input != 30 {
		t.Fatalf("unexpected record: %+v", recs[0])
	}
}

func TestClaudeRecordsAreSortedByTime(t *testing.T) {
	root := t.TempDir()
	content := entry("2026-01-02T03:04:09.000Z", "c", "m", 1, 1, 0, 0) + "\n" +
		entry("2026-01-02T03:04:05.000Z", "a", "m", 1, 1, 0, 0) + "\n" +
		entry("2026-01-02T03:04:07.000Z", "b", "m", 1, 1, 0, 0) + "\n"
	writeFile(t, root, "proj/session.jsonl", content)

	c := NewClaude(root)
	recs, err := c.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3", len(recs))
	}
	for i := 1; i < len(recs); i++ {
		if recs[i].Time.Before(recs[i-1].Time) {
			t.Fatalf("records are not sorted by time: %+v", recs)
		}
	}
}

func TestClaudeRecordsWithoutIDAreKept(t *testing.T) {
	root := t.TempDir()
	line := `{"timestamp":"2026-01-02T03:04:05.000Z","message":{"model":"m","usage":{"input_tokens":4,"output_tokens":2}}}`
	writeFile(t, root, "proj/session.jsonl", line+"\n"+line+"\n")

	c := NewClaude(root)
	recs, err := c.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2 (no id means no dedupe): %+v", len(recs), recs)
	}
}

func TestClaudeLargeLine(t *testing.T) {
	root := t.TempDir()
	// A line well above the default 64KB scanner limit.
	big := entry("2026-01-02T03:04:05.000Z", "a", "m", 1, 1, 0, 0)
	big = big[:len(big)-1] + `,"padding":"` + strings.Repeat("x", 2<<20) + `"}`
	writeFile(t, root, "proj/session.jsonl", big+"\n")

	c := NewClaude(root)
	recs, err := c.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if recs[0].Input != 1 {
		t.Fatalf("unexpected record: %+v", recs[0])
	}
}

func TestClaudeLoadRespectsContextCancellation(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "proj/session.jsonl", entry("2026-01-02T03:04:05.000Z", "a", "m", 1, 1, 0, 0)+"\n")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := NewClaude(root)
	if _, err := c.Load(ctx); err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
}

func TestClaudeImplementsSource(t *testing.T) {
	var _ Source = NewClaude(t.TempDir())
}

func TestClaudeUsageJSONShape(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "proj/session.jsonl", entry("2026-01-02T03:04:05.000Z", "a", "m", 1, 2, 3, 4)+"\n")

	c := NewClaude(root)
	recs, err := c.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var rec usage.Record = recs[0]
	if rec.CacheWrite != 3 || rec.CacheRead != 4 {
		t.Fatalf("unexpected cache counters: %+v", rec)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("empty json")
	}
}
