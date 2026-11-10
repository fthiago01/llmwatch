package usage

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// fixedNow is the reference instant used by every test.
var fixedNow = time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)

func rec(offset time.Duration, provider, model string, input, output int64) Record {
	return Record{
		Source:   "test",
		Provider: provider,
		Model:    model,
		Time:     fixedNow.Add(offset),
		Input:    input,
		Output:   output,
	}
}

func window(t *testing.T, snap Snapshot, name string) Window {
	t.Helper()
	for _, w := range snap.Windows {
		if w.Name == name {
			return w
		}
	}
	t.Fatalf("window %q not found", name)
	return Window{}
}

func TestTotalsAdd(t *testing.T) {
	var tot Totals
	tot.Add(Record{
		Provider:   "anthropic",
		Model:      "sonnet",
		Time:       fixedNow,
		Input:      100,
		Output:     50,
		CacheRead:  10,
		CacheWrite: 5,
		Reasoning:  2,
		Cost:       1.25,
	})
	tot.Add(Record{
		Provider: "anthropic",
		Model:    "sonnet",
		Time:     fixedNow.Add(time.Minute),
		Input:    1,
		Output:   2,
		Cost:     0.5,
	})

	if tot.Input != 101 {
		t.Errorf("Input = %d, want 101", tot.Input)
	}
	if tot.Output != 52 {
		t.Errorf("Output = %d, want 52", tot.Output)
	}
	if tot.CacheRead != 10 {
		t.Errorf("CacheRead = %d, want 10", tot.CacheRead)
	}
	if tot.CacheWrite != 5 {
		t.Errorf("CacheWrite = %d, want 5", tot.CacheWrite)
	}
	if tot.Reasoning != 2 {
		t.Errorf("Reasoning = %d, want 2", tot.Reasoning)
	}
	if want := int64(101 + 52 + 10 + 5 + 2); tot.Total != want {
		t.Errorf("Total = %d, want %d", tot.Total, want)
	}
	if tot.Requests != 2 {
		t.Errorf("Requests = %d, want 2", tot.Requests)
	}
	if math.Abs(tot.Cost-1.75) > 1e-9 {
		t.Errorf("Cost = %v, want 1.75", tot.Cost)
	}
}

func TestAggregateWindows(t *testing.T) {
	// r1: 6h ago -> outside "5h", inside "hoje", "7d" and "30d".
	r1 := rec(-6*time.Hour, "anthropic", "sonnet", 100, 50)
	r1.CacheRead, r1.CacheWrite, r1.Reasoning = 10, 5, 2 // 167 tokens
	// r2: 1h ago -> inside every window.
	r2 := rec(-1*time.Hour, "openai", "gpt", 200, 100) // 300 tokens
	// r3: 20h ago -> outside "5h" and "hoje", inside "7d" and "30d".
	r3 := rec(-20*time.Hour, "anthropic", "opus", 10, 5) // 15 tokens
	// r4: 40d ago -> outside every window.
	r4 := rec(-40*24*time.Hour, "openai", "gpt", 1, 0)

	snap := Aggregate([]Record{r1, r2, r3, r4}, fixedNow, nil)

	wantNames := []string{"5h", "hoje", "7d", "30d"}
	if len(snap.Windows) != len(wantNames) {
		t.Fatalf("len(Windows) = %d, want %d", len(snap.Windows), len(wantNames))
	}
	for i, name := range wantNames {
		if snap.Windows[i].Name != name {
			t.Errorf("Windows[%d].Name = %q, want %q", i, snap.Windows[i].Name, name)
		}
	}

	cases := []struct {
		name      string
		total     int64
		requests  int64
		providers int
	}{
		{"5h", 300, 1, 1},
		{"hoje", 467, 2, 2},
		{"7d", 482, 3, 3},
		{"30d", 482, 3, 3},
	}
	for _, tc := range cases {
		w := window(t, snap, tc.name)
		if w.Sum.Total != tc.total {
			t.Errorf("window %q Sum.Total = %d, want %d", tc.name, w.Sum.Total, tc.total)
		}
		if w.Sum.Requests != tc.requests {
			t.Errorf("window %q Sum.Requests = %d, want %d", tc.name, w.Sum.Requests, tc.requests)
		}
		if len(w.ByProvider) != tc.providers {
			t.Errorf("window %q len(ByProvider) = %d, want %d", tc.name, len(w.ByProvider), tc.providers)
		}
	}
}

func TestAggregateWindowsUseLocalMidnight(t *testing.T) {
	brt := time.FixedZone("BRT", -3*60*60)
	now := time.Date(2026, 3, 10, 15, 0, 0, 0, brt)

	// 09:00 UTC is 06:00 local -> still today.
	today := Record{Provider: "p", Model: "m", Time: time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC), Input: 10}
	// 19:00 UTC on the previous day is 16:00 local -> yesterday.
	yesterday := Record{Provider: "p", Model: "m", Time: time.Date(2026, 3, 9, 19, 0, 0, 0, time.UTC), Input: 100}

	snap := Aggregate([]Record{today, yesterday}, now, nil)

	hoje := window(t, snap, "hoje")
	if hoje.Sum.Total != 10 {
		t.Errorf("hoje Sum.Total = %d, want 10", hoje.Sum.Total)
	}
	seven := window(t, snap, "7d")
	if seven.Sum.Total != 110 {
		t.Errorf("7d Sum.Total = %d, want 110", seven.Sum.Total)
	}
}

func TestAggregateIgnoresFutureRecords(t *testing.T) {
	future := Record{Provider: "p", Model: "m", Time: fixedNow.Add(time.Minute), Input: 1000}
	snap := Aggregate([]Record{future}, fixedNow, nil)

	for _, w := range snap.Windows {
		if w.Sum.Total != 0 || w.Sum.Requests != 0 {
			t.Errorf("window %q Sum = %+v, want empty", w.Name, w.Sum)
		}
	}
	if len(snap.Blocks) != 0 {
		t.Errorf("len(Blocks) = %d, want 0", len(snap.Blocks))
	}
}

func TestWindowByProviderOrdering(t *testing.T) {
	recs := []Record{
		{Provider: "alpha", Model: "m1", Time: fixedNow.Add(-time.Hour), Input: 100},
		{Provider: "alpha", Model: "m1", Time: fixedNow.Add(-2 * time.Hour), Input: 100},
		{Provider: "alpha", Model: "m2", Time: fixedNow.Add(-3 * time.Hour), Input: 150},
		{Provider: "beta", Model: "m1", Time: fixedNow.Add(-4 * time.Hour), Input: 150},
	}

	snap := Aggregate(recs, fixedNow, nil)
	rows := window(t, snap, "5h").ByProvider

	want := []struct {
		provider string
		model    string
		total    int64
	}{
		{"alpha", "m1", 200},
		{"alpha", "m2", 150},
		{"beta", "m1", 150},
	}
	if len(rows) != len(want) {
		t.Fatalf("len(ByProvider) = %d, want %d", len(rows), len(want))
	}
	for i, w := range want {
		got := rows[i]
		if got.Provider != w.provider || got.Model != w.model {
			t.Errorf("ByProvider[%d] = %s/%s, want %s/%s", i, got.Provider, got.Model, w.provider, w.model)
		}
		if got.Total != w.total {
			t.Errorf("ByProvider[%d].Total = %d, want %d", i, got.Total, w.total)
		}
	}
}

func TestHourlyBuckets(t *testing.T) {
	nowTrunc := fixedNow.Truncate(time.Hour)
	recs := []Record{
		{Provider: "openai", Model: "gpt", Time: nowTrunc.Add(-time.Hour), Input: 300},
		{Provider: "anthropic", Model: "sonnet", Time: nowTrunc, Output: 100},
		{Provider: "openai", Model: "gpt", Time: nowTrunc.Add(-25 * time.Hour), Input: 9999},
		{Provider: "openai", Model: "gpt", Time: fixedNow.Add(time.Hour), Input: 9999},
	}

	snap := Aggregate(recs, fixedNow, nil)
	hourly := snap.Hourly

	if len(hourly) != 24 {
		t.Fatalf("len(Hourly) = %d, want 24", len(hourly))
	}
	if want := nowTrunc.Add(-23 * time.Hour); !hourly[0].Hour.Equal(want) {
		t.Errorf("Hourly[0].Hour = %v, want %v", hourly[0].Hour, want)
	}
	if want := nowTrunc; !hourly[23].Hour.Equal(want) {
		t.Errorf("Hourly[23].Hour = %v, want %v", hourly[23].Hour, want)
	}
	for i := 1; i < len(hourly); i++ {
		if d := hourly[i].Hour.Sub(hourly[i-1].Hour); d != time.Hour {
			t.Errorf("Hourly[%d].Hour - Hourly[%d].Hour = %v, want 1h", i, i-1, d)
		}
		if hourly[i].ByProvider == nil {
			t.Errorf("Hourly[%d].ByProvider is nil", i)
		}
	}

	prev := hourly[22]
	if prev.ByProvider["openai"] != 300 {
		t.Errorf("Hourly[22].ByProvider[openai] = %d, want 300", prev.ByProvider["openai"])
	}
	cur := hourly[23]
	if cur.ByProvider["anthropic"] != 100 {
		t.Errorf("Hourly[23].ByProvider[anthropic] = %d, want 100", cur.ByProvider["anthropic"])
	}
	for i, b := range hourly {
		if i == 22 || i == 23 {
			continue
		}
		if len(b.ByProvider) != 0 {
			t.Errorf("Hourly[%d].ByProvider = %v, want empty", i, b.ByProvider)
		}
	}
}

func TestBlockStartTruncatedToHour(t *testing.T) {
	r := Record{
		Provider: "anthropic",
		Model:    "sonnet",
		Time:     time.Date(2026, 3, 10, 10, 37, 42, 0, time.UTC),
		Input:    600,
		Output:   400,
	}
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

	snap := Aggregate([]Record{r}, now, nil)
	if len(snap.Blocks) != 1 {
		t.Fatalf("len(Blocks) = %d, want 1", len(snap.Blocks))
	}
	b := snap.Blocks[0]
	if want := time.Date(2026, 3, 10, 10, 0, 0, 0, time.UTC); !b.Start.Equal(want) {
		t.Errorf("Block.Start = %v, want %v", b.Start, want)
	}
	if want := time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC); !b.ResetAt.Equal(want) {
		t.Errorf("Block.ResetAt = %v, want %v", b.ResetAt, want)
	}
	if b.Total != 1000 {
		t.Errorf("Block.Total = %d, want 1000", b.Total)
	}
	if b.Requests != 1 {
		t.Errorf("Block.Requests = %d, want 1", b.Requests)
	}
	if want := 1000.0 / 120.0; math.Abs(b.TokensPerMin-want) > 1e-9 {
		t.Errorf("Block.TokensPerMin = %v, want %v", b.TokensPerMin, want)
	}
}

func TestBlockExpiredIsNotReported(t *testing.T) {
	r := Record{
		Provider: "anthropic",
		Model:    "sonnet",
		Time:     time.Date(2026, 3, 10, 8, 30, 0, 0, time.UTC),
		Input:    100,
	}
	now := time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)

	snap := Aggregate([]Record{r}, now, nil)
	if len(snap.Blocks) != 0 {
		t.Fatalf("len(Blocks) = %d, want 0 (block expired)", len(snap.Blocks))
	}
}

func TestBlockGapStartsNewBlock(t *testing.T) {
	recs := []Record{
		{
			Provider: "anthropic",
			Model:    "sonnet",
			Time:     time.Date(2026, 3, 10, 6, 0, 0, 0, time.UTC),
			Input:    500,
		},
		{
			Provider: "anthropic",
			Model:    "sonnet",
			Time:     time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC),
			Input:    200,
		},
	}
	now := time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)

	snap := Aggregate(recs, now, nil)
	if len(snap.Blocks) != 1 {
		t.Fatalf("len(Blocks) = %d, want 1", len(snap.Blocks))
	}
	b := snap.Blocks[0]
	if want := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC); !b.Start.Equal(want) {
		t.Errorf("Block.Start = %v, want %v", b.Start, want)
	}
	if b.Total != 200 {
		t.Errorf("Block.Total = %d, want 200 (only the new block counts)", b.Total)
	}
	if b.Requests != 1 {
		t.Errorf("Block.Requests = %d, want 1", b.Requests)
	}
}

func TestBlockLimitAndPercentUsed(t *testing.T) {
	recs := []Record{
		{
			Provider: "anthropic",
			Model:    "sonnet",
			Time:     time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC),
			Input:    150,
			Output:   50,
		},
		{
			Provider: "openai",
			Model:    "gpt",
			Time:     time.Date(2026, 3, 10, 13, 0, 0, 0, time.UTC),
			Input:    10,
		},
	}
	now := time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)

	snap := Aggregate(recs, now, map[string]int64{"anthropic": 1000})
	if len(snap.Blocks) != 2 {
		t.Fatalf("len(Blocks) = %d, want 2", len(snap.Blocks))
	}
	if snap.Blocks[0].Provider != "anthropic" || snap.Blocks[1].Provider != "openai" {
		t.Errorf("Blocks providers = %q, %q, want anthropic, openai",
			snap.Blocks[0].Provider, snap.Blocks[1].Provider)
	}

	first := snap.Blocks[0]
	if first.Limit != 1000 {
		t.Errorf("Block.Limit = %d, want 1000", first.Limit)
	}
	if want := 20.0; math.Abs(first.PercentUsed-want) > 1e-9 {
		t.Errorf("Block.PercentUsed = %v, want %v", first.PercentUsed, want)
	}

	second := snap.Blocks[1]
	if second.Limit != 0 {
		t.Errorf("Block.Limit = %d, want 0", second.Limit)
	}
	if second.PercentUsed != 0 {
		t.Errorf("Block.PercentUsed = %v, want 0", second.PercentUsed)
	}
}

func TestAggregateEmptyInputHasNoNilSlices(t *testing.T) {
	snap := Aggregate(nil, fixedNow, nil)

	if snap.Windows == nil {
		t.Error("Windows is nil")
	}
	if len(snap.Windows) != 4 {
		t.Errorf("len(Windows) = %d, want 4", len(snap.Windows))
	}
	for _, w := range snap.Windows {
		if w.ByProvider == nil {
			t.Errorf("window %q ByProvider is nil", w.Name)
		}
	}
	if snap.Blocks == nil {
		t.Error("Blocks is nil")
	}
	if snap.Hourly == nil {
		t.Error("Hourly is nil")
	}
	if len(snap.Hourly) != 24 {
		t.Errorf("len(Hourly) = %d, want 24", len(snap.Hourly))
	}
	for i, b := range snap.Hourly {
		if b.ByProvider == nil {
			t.Errorf("Hourly[%d].ByProvider is nil", i)
		}
	}
	if snap.Errors == nil {
		t.Error("Errors is nil")
	}
	if !snap.GeneratedAt.Equal(fixedNow) {
		t.Errorf("GeneratedAt = %v, want %v", snap.GeneratedAt, fixedNow)
	}
}

func TestJSONTagsAreSnakeCase(t *testing.T) {
	raw, err := json.Marshal(Record{Input: 1})
	if err != nil {
		t.Fatalf("marshal Record: %v", err)
	}
	for _, key := range []string{`"source"`, `"provider"`, `"model"`, `"time"`, `"input"`, `"output"`, `"cache_read"`, `"cache_write"`, `"reasoning"`, `"cost"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("Record JSON %s missing %s", raw, key)
		}
	}

	snap := Aggregate(nil, fixedNow, nil)
	raw, err = json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal Snapshot: %v", err)
	}
	for _, key := range []string{`"generated_at"`, `"windows"`, `"by_provider"`, `"blocks"`, `"hourly"`, `"errors"`, `"requests"`, `"total"`, `"name"`, `"sum"`, `"hour"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("Snapshot JSON missing %s", key)
		}
	}

	raw, err = json.Marshal(Block{Provider: "p", Limit: 10})
	if err != nil {
		t.Fatalf("marshal Block: %v", err)
	}
	for _, key := range []string{`"provider"`, `"start"`, `"reset_at"`, `"tokens_per_min"`, `"percent_used"`, `"limit"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("Block JSON %s missing %s", raw, key)
		}
	}
}

func TestAggregateQuotasAreEmptyAndNotNil(t *testing.T) {
	snap := Aggregate(nil, fixedNow, nil)

	if snap.Quotas == nil {
		t.Fatal("Quotas is nil")
	}
	if len(snap.Quotas) != 0 {
		t.Errorf("len(Quotas) = %d, want 0", len(snap.Quotas))
	}
}

func TestQuotaWindowRemaining(t *testing.T) {
	cases := []struct {
		used   float64
		want   float64
		reason string
	}{
		{used: 30, want: 70, reason: "30% used leaves 70%"},
		{used: 120, want: 0, reason: "over 100% is clamped to 0"},
		{used: -5, want: 100, reason: "negative usage is clamped to 100"},
		{used: 0, want: 100, reason: "no usage leaves everything"},
		{used: 100, want: 0, reason: "fully used leaves nothing"},
	}

	for _, tc := range cases {
		got := QuotaWindow{UsedPercent: tc.used}.Remaining()
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("QuotaWindow{UsedPercent: %v}.Remaining() = %v, want %v (%s)",
				tc.used, got, tc.want, tc.reason)
		}
	}
}

func TestQuotaJSONTagsAreSnakeCase(t *testing.T) {
	resets := fixedNow.Add(time.Hour)

	raw, err := json.Marshal(QuotaWindow{Name: "5h", UsedPercent: 30, ResetsAt: &resets, Used: 30, Limit: 100})
	if err != nil {
		t.Fatalf("marshal QuotaWindow: %v", err)
	}
	for _, key := range []string{"name", "used_percent", "resets_at", "used", "limit"} {
		if !strings.Contains(string(raw), `"`+key+`"`) {
			t.Errorf("QuotaWindow JSON %s missing %s", raw, key)
		}
	}

	raw, err = json.Marshal(QuotaStatus{
		Provider:  "anthropic",
		Source:    "claude-statusline",
		Plan:      "max",
		Windows:   []QuotaWindow{{Name: "5h", UsedPercent: 12.5}},
		UpdatedAt: fixedNow,
	})
	if err != nil {
		t.Fatalf("marshal QuotaStatus: %v", err)
	}
	for _, key := range []string{"provider", "source", "plan", "windows", "updated_at"} {
		if !strings.Contains(string(raw), `"`+key+`"`) {
			t.Errorf("QuotaStatus JSON %s missing %s", raw, key)
		}
	}

	raw, err = json.Marshal(Aggregate(nil, fixedNow, nil))
	if err != nil {
		t.Fatalf("marshal Snapshot: %v", err)
	}
	if !strings.Contains(string(raw), `"quotas"`) {
		t.Errorf("Snapshot JSON %s missing quotas key", raw)
	}
}
