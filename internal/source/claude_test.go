package source

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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

// claudeUsageOf builds a claudeUsage with the given counters.
func claudeUsageOf(in, out, cacheWrite, cacheRead int64) *claudeUsage {
	return &claudeUsage{
		InputTokens:              in,
		OutputTokens:             out,
		CacheCreationInputTokens: cacheWrite,
		CacheReadInputTokens:     cacheRead,
	}
}

// claudeSplitUsage builds a claudeUsage whose cache creation tokens are
// split by TTL.
func claudeSplitUsage(in, out, write5m, write1h, cacheRead int64) *claudeUsage {
	u := claudeUsageOf(in, out, write5m+write1h, cacheRead)
	u.CacheCreation = &struct {
		Ephemeral5mInputTokens *int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1hInputTokens *int64 `json:"ephemeral_1h_input_tokens"`
	}{}
	five, one := write5m, write1h
	u.CacheCreation.Ephemeral5mInputTokens = &five
	u.CacheCreation.Ephemeral1hInputTokens = &one
	return u
}

// wantCost fails the test when got and want differ by more than epsilon.
func wantCost(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("cost = %v, want %v", got, want)
	}
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

func TestClaudeSessionAndProject(t *testing.T) {
	root := t.TempDir()
	content := `{"timestamp":"2026-01-02T03:04:05.000Z","sessionId":"sess_1","cwd":"/home/user/proj","message":{"id":"a","model":"claude-opus-4","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n" +
		`{"timestamp":"2026-01-02T03:04:06.000Z","message":{"id":"b","model":"claude-opus-4","usage":{"input_tokens":20,"output_tokens":8}}}` + "\n"
	writeFile(t, root, "proj/session.jsonl", content)

	c := NewClaude(root)
	recs, err := c.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2: %+v", len(recs), recs)
	}
	if recs[0].Session != "sess_1" {
		t.Fatalf("session = %q, want sess_1", recs[0].Session)
	}
	if recs[0].Project != "/home/user/proj" {
		t.Fatalf("project = %q, want /home/user/proj", recs[0].Project)
	}
	if recs[1].Session != "" {
		t.Fatalf("session = %q, want empty", recs[1].Session)
	}
	if recs[1].Project != "" {
		t.Fatalf("project = %q, want empty", recs[1].Project)
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

func TestClaudeCostOpus55(t *testing.T) {
	// claude-opus-5-5: [input, cw5m, cw1h, read, output] =
	// [4, 5, 8, 0.2, 20] US$/M tokens.
	//
	// (1000*4 + 3000*5 + 200*0.2 + 500*20) / 1e6 = 0.02904
	got := claudeCost("claude-opus-5-5", claudeUsageOf(1000, 500, 3000, 200))
	wantCost(t, got, 0.02904)
}

func TestClaudeCostCacheWriteWithoutSplitCountsAs5m(t *testing.T) {
	// Without the TTL breakdown the whole cache creation count is billed
	// as a 5 minute write.
	//
	// (1000*4 + 3000*5 + 200*0.2 + 500*20) / 1e6 = 0.02904
	got := claudeCost("claude-opus-5-5", claudeUsageOf(1000, 500, 3000, 200))
	wantCost(t, got, 0.02904)
}

func TestClaudeCostCacheWriteSplit(t *testing.T) {
	// The split is used as is: 1000 tokens at 5m (5) and 2000 at 1h (8).
	//
	// (1000*4 + 1000*5 + 2000*8 + 200*0.2 + 500*20) / 1e6 = 0.03504
	got := claudeCost("claude-opus-5-5", claudeSplitUsage(1000, 500, 1000, 2000, 200))
	wantCost(t, got, 0.03504)
}

func TestClaudeCostNormalizesModelSuffixes(t *testing.T) {
	want := 0.02904
	for _, model := range []string{
		"claude-opus-5-5",
		"claude-opus-5-5[1m]",
		"claude-opus-5-5-20260101",
		"claude-opus-5-5[1m]-20260101",
	} {
		got := claudeCost(model, claudeUsageOf(1000, 500, 3000, 200))
		if math.Abs(got-want) > 1e-9 {
			t.Fatalf("claudeCost(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestClaudeCostUnknownModelIsZero(t *testing.T) {
	for _, model := range []string{"", "claude-opus-9", "gpt-5", "claude-opus-5-5-turbo"} {
		if got := claudeCost(model, claudeUsageOf(1000, 500, 3000, 200)); got != 0 {
			t.Fatalf("claudeCost(%q) = %v, want 0", model, got)
		}
	}
}

func TestClaudeCostFastSpeedIsZero(t *testing.T) {
	u := claudeUsageOf(1000, 500, 3000, 200)
	u.Speed = "fast"
	if got := claudeCost("claude-opus-5-5", u); got != 0 {
		t.Fatalf("cost with fast speed = %v, want 0", got)
	}
}

func TestClaudeCostHaikuLongContext(t *testing.T) {
	// claude-haiku-5-5 below 100k tokens of context:
	// [0.1, 0.125, 0.2, 0.01, 0.5].
	//
	// (1000*0.1 + 3000*0.125 + 200*0.01 + 500*0.5) / 1e6 = 0.000727
	short := claudeCost("claude-haiku-5-5", claudeUsageOf(1000, 500, 3000, 200))
	wantCost(t, short, 0.000727)

	// Above 100k tokens of context the long context pricing applies:
	// [0.5, 0.625, 1, 0.05, 2.5].
	//
	// (60000*0.5 + 30000*0.625 + 20000*0.05 + 500*2.5) / 1e6 = 0.051
	long := claudeCost("claude-haiku-5-5", claudeUsageOf(60000, 500, 30000, 20000))
	wantCost(t, long, 0.051)
}

func TestClaudeCostWebSearchIsAdded(t *testing.T) {
	u := claudeUsageOf(1000, 500, 3000, 200)
	u.ServerToolUse = &struct {
		WebSearchRequests int64 `json:"web_search_requests"`
	}{WebSearchRequests: 3}

	// 0.02904 + 3 * 0.01
	got := claudeCost("claude-opus-5-5", u)
	wantCost(t, got, 0.05904)
}

func TestClaudeCostNilUsageIsZero(t *testing.T) {
	if got := claudeCost("claude-opus-5-5", nil); got != 0 {
		t.Fatalf("cost = %v, want 0", got)
	}
}

func TestClaudeRecordCostIsEstimated(t *testing.T) {
	root := t.TempDir()
	line := `{"timestamp":"2026-01-02T03:04:05.000Z","message":{"id":"a","model":"claude-opus-5-5",` +
		`"usage":{"input_tokens":1000,"output_tokens":500,"cache_creation_input_tokens":3000,` +
		`"cache_read_input_tokens":200,"speed":"standard",` +
		`"cache_creation":{"ephemeral_5m_input_tokens":1000,"ephemeral_1h_input_tokens":2000},` +
		`"server_tool_use":{"web_search_requests":2}}}}` + "\n"
	writeFile(t, root, "proj/session.jsonl", line)

	c := NewClaude(root)
	recs, err := c.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(recs), recs)
	}
	// (1000*4 + 1000*5 + 2000*8 + 200*0.2 + 500*20) / 1e6 + 2*0.01
	wantCost(t, recs[0].Cost, 0.05504)
}

func TestClaudeRecordCostIsZeroForFastSpeed(t *testing.T) {
	root := t.TempDir()
	line := `{"timestamp":"2026-01-02T03:04:05.000Z","message":{"id":"a","model":"claude-opus-5-5",` +
		`"usage":{"input_tokens":1000,"output_tokens":500,"speed":"fast"}}}` + "\n"
	writeFile(t, root, "proj/session.jsonl", line)

	c := NewClaude(root)
	recs, err := c.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(recs), recs)
	}
	if recs[0].Cost != 0 {
		t.Fatalf("cost = %v, want 0", recs[0].Cost)
	}
}
