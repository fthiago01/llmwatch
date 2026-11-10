package quota

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClaudeFetch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-statusline.json")
	now := time.Now()
	fiveHourReset := now.Add(time.Hour).Unix()
	sevenDayReset := now.Add(72 * time.Hour).Unix()
	data := fmt.Sprintf(
		`{"model":{"id":"claude-opus-5-5","display_name":"Opus"}, "rate_limits":{"five_hour":{"used_percentage":16,"resets_at":%d},"seven_day":{"used_percentage":2,"resets_at":%d},"seven_day_opus":{"used_percentage":7.5}}}`,
		fiveHourReset,
		sevenDayReset,
	)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	status := NewClaude(path).Fetch(context.Background())
	if status.Error != "" {
		t.Fatalf("unexpected error: %s", status.Error)
	}
	if len(status.Windows) != 3 {
		t.Fatalf("got %d windows, want 3: %#v", len(status.Windows), status.Windows)
	}
	if status.Windows[0].Name != "5h" || status.Windows[0].UsedPercent != 16 {
		t.Errorf("5h window = %#v", status.Windows[0])
	}
	if status.Windows[0].ResetsAt == nil || status.Windows[0].ResetsAt.Unix() != fiveHourReset {
		t.Errorf("5h reset = %#v, want %d", status.Windows[0].ResetsAt, fiveHourReset)
	}
	if status.Windows[1].Name != "7d" || status.Windows[1].UsedPercent != 2 {
		t.Errorf("7d window = %#v", status.Windows[1])
	}
	if status.Windows[2].Name != "7d opus" {
		t.Errorf("extra window = %#v", status.Windows[2])
	}
	if status.Provider != "anthropic" || status.Source != "claude-statusline" {
		t.Errorf("status identity = %q, %q", status.Provider, status.Source)
	}
}

func TestClaudeFetchMissingFile(t *testing.T) {
	status := NewClaude(filepath.Join(t.TempDir(), "missing.json")).Fetch(context.Background())
	if status.Error == "" {
		t.Fatal("expected error")
	}
	if status.Windows == nil {
		t.Fatal("Windows must not be nil")
	}
}

func TestClaudeFetchInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	status := NewClaude(path).Fetch(context.Background())
	if status.Error == "" {
		t.Fatal("expected error")
	}
	if status.Windows == nil {
		t.Fatal("Windows must not be nil")
	}
}

func TestClaudeFetchExpiredWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	expiredReset := time.Now().Add(-time.Hour).Unix()
	data := fmt.Sprintf(
		`{"rate_limits":{"five_hour":{"used_percentage":50,"resets_at":%d}}}`,
		expiredReset,
	)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	status := NewClaude(path).Fetch(context.Background())
	if status.Error != "" {
		t.Fatalf("unexpected error: %s", status.Error)
	}
	if len(status.Windows) != 1 || status.Windows[0].UsedPercent != 0 {
		t.Fatalf("expired window = %#v", status.Windows)
	}
	if status.Windows[0].ResetsAt == nil || status.Windows[0].ResetsAt.Unix() != expiredReset {
		t.Errorf("reset should be retained: %#v, want %d", status.Windows[0].ResetsAt, expiredReset)
	}
}

func TestWindowName(t *testing.T) {
	tests := map[string]string{
		"five_hour":        "5h",
		"seven_day":        "7d",
		"seven_day_opus":   "7d opus",
		"seven_day_sonnet": "7d sonnet",
		"custom_window":    "custom window",
	}
	for key, want := range tests {
		if got := windowName(key); got != want {
			t.Errorf("windowName(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestClaudeFetchUpdatedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	if err := os.WriteFile(path, []byte(`{"rate_limits":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-time.Second)
	status := NewClaude(path).Fetch(context.Background())
	if status.UpdatedAt.Before(before) {
		t.Errorf("UpdatedAt = %v", status.UpdatedAt)
	}
}
