package quota

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMiniMaxFetch(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"base_resp":{"status_code":0,"status_msg":"success"},"model_remains":[
			{"model_name":"MiniMax-M2","current_interval_total_count":1500,"current_interval_usage_count":1200,
			 "current_weekly_total_count":6000,"current_weekly_usage_count":3000,
			 "end_time":1794333000000,"weekly_end_time":1794675600000}
		]}`)
	}))
	defer server.Close()

	status := NewMiniMax("secret", server.URL, server.Client()).Fetch(context.Background())
	if status.Error != "" {
		t.Fatalf("unexpected error: %s", status.Error)
	}
	if status.Provider != "minimax-coding-plan" || status.Source != "minimax-api" {
		t.Errorf("identity = %q, %q", status.Provider, status.Source)
	}
	if requests != 1 {
		t.Errorf("requests = %d, want 1", requests)
	}
	if len(status.Windows) != 2 {
		t.Fatalf("got %d windows, want 2: %#v", len(status.Windows), status.Windows)
	}

	window := status.Windows[0]
	if window.Name != "5h · MiniMax-M2" {
		t.Errorf("window name = %q", window.Name)
	}
	if window.Used != 300 {
		t.Errorf("Used = %d, want 300", window.Used)
	}
	if window.Limit != 1500 {
		t.Errorf("Limit = %d, want 1500", window.Limit)
	}
	if window.UsedPercent != 20 {
		t.Errorf("UsedPercent = %v, want 20", window.UsedPercent)
	}
	if window.ResetsAt == nil || window.ResetsAt.UnixMilli() != 1794333000000 {
		t.Errorf("ResetsAt = %#v", window.ResetsAt)
	}

	weekly := status.Windows[1]
	if weekly.Name != "semana · MiniMax-M2" {
		t.Errorf("weekly name = %q", weekly.Name)
	}
	if weekly.Used != 3000 || weekly.Limit != 6000 || weekly.UsedPercent != 50 {
		t.Errorf("weekly window = %#v", weekly)
	}
	if weekly.ResetsAt == nil || weekly.ResetsAt.UnixMilli() != 1794675600000 {
		t.Errorf("weekly ResetsAt = %#v", weekly.ResetsAt)
	}
}

func TestMiniMaxFetchRemainingPercent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"base_resp":{"status_code":0,"status_msg":"success"},"model_remains":[
			{"start_time":1794322800000,"end_time":1794340800000,"remains_time":15904576,"current_interval_total_count":0,"current_interval_usage_count":0,"model_name":"general","current_weekly_total_count":0,"current_weekly_usage_count":0,"weekly_start_time":1794182400000,"weekly_end_time":1794787200000,"weekly_remains_time":462304576,"current_interval_status":1,"current_interval_remaining_percent":95,"current_weekly_status":1,"current_weekly_remaining_percent":91},
			{"model_name":"video","current_interval_total_count":0,"current_interval_usage_count":0,"current_interval_remaining_percent":100,"current_interval_status":3,"current_weekly_total_count":0,"current_weekly_remaining_percent":100,"current_weekly_status":3,"end_time":1794355200000,"weekly_end_time":1794787200000}
		]}`)
	}))
	defer server.Close()

	status := NewMiniMax("secret", server.URL, server.Client()).Fetch(context.Background())
	if status.Error != "" {
		t.Fatalf("unexpected error: %s", status.Error)
	}
	if len(status.Windows) != 2 {
		t.Fatalf("got %d windows, want 2: %#v", len(status.Windows), status.Windows)
	}

	interval := status.Windows[0]
	if interval.Name != "5h" {
		t.Errorf("window name = %q, want %q", interval.Name, "5h")
	}
	if interval.UsedPercent != 5 {
		t.Errorf("UsedPercent = %v, want 5", interval.UsedPercent)
	}
	if interval.Used != 0 || interval.Limit != 0 {
		t.Errorf("window = %#v, want zeroed counters", interval)
	}
	if interval.ResetsAt == nil || interval.ResetsAt.UnixMilli() != 1794340800000 {
		t.Errorf("ResetsAt = %#v, want end_time", interval.ResetsAt)
	}

	weekly := status.Windows[1]
	if weekly.Name != "semana" {
		t.Errorf("weekly name = %q, want %q", weekly.Name, "semana")
	}
	if weekly.UsedPercent != 9 {
		t.Errorf("weekly UsedPercent = %v, want 9", weekly.UsedPercent)
	}
	if weekly.Used != 0 || weekly.Limit != 0 {
		t.Errorf("weekly window = %#v, want zeroed counters", weekly)
	}
	if weekly.ResetsAt == nil || weekly.ResetsAt.UnixMilli() != 1794787200000 {
		t.Errorf("weekly ResetsAt = %#v, want weekly_end_time", weekly.ResetsAt)
	}
}

func TestMiniMaxFetchWindowSelection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"base_resp":{"status_code":0,"status_msg":"success"},"model_remains":[
			{"model_name":"general","current_interval_status":1,"current_interval_remaining_percent":0,"current_interval_total_count":100,"current_interval_usage_count":40,
			 "current_weekly_status":1,"current_weekly_remaining_percent":"50","current_weekly_total_count":"200","current_weekly_usage_count":"80"},
			{"model_name":"video","current_interval_status":3,"current_interval_total_count":100,"current_interval_usage_count":10,
			 "current_weekly_status":3,"current_weekly_total_count":100,"current_weekly_usage_count":10}
		]}`)
	}))
	defer server.Close()

	status := NewMiniMax("secret", server.URL, server.Client()).Fetch(context.Background())
	if status.Error != "" {
		t.Fatalf("unexpected error: %s", status.Error)
	}
	// The video entry is disabled (status 3) in both windows.
	if len(status.Windows) != 2 {
		t.Fatalf("got %d windows, want 2: %#v", len(status.Windows), status.Windows)
	}

	interval := status.Windows[0]
	if interval.Name != "5h" || interval.UsedPercent != 100 {
		t.Errorf("interval window = %#v", interval)
	}
	// The counters are reported alongside the percentage, so they are kept.
	if interval.Used != 60 || interval.Limit != 100 {
		t.Errorf("interval counters = %#v", interval)
	}

	weekly := status.Windows[1]
	if weekly.Name != "semana" || weekly.UsedPercent != 50 {
		t.Errorf("weekly window = %#v", weekly)
	}
	if weekly.Used != 120 || weekly.Limit != 200 {
		t.Errorf("weekly counters = %#v", weekly)
	}
}

func TestMiniMaxFetchMissingKey(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer server.Close()

	status := NewMiniMax("", server.URL, server.Client()).Fetch(context.Background())
	if status.Error == "" {
		t.Fatal("expected error")
	}
	if !strings.Contains(status.Error, "LLMWATCH_MINIMAX_KEY") {
		t.Errorf("error = %q", status.Error)
	}
	if requests != 0 {
		t.Errorf("requests = %d, want 0", requests)
	}
	if status.Windows == nil {
		t.Fatal("Windows must not be nil")
	}
}

func TestMiniMaxFetchStringNumbers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"base_resp":{"status_code":0,"status_msg":"success"},"model_remains":[
			{"model_name":"MiniMax-M2","current_interval_total_count":"1500","current_interval_usage_count":"1200",
			 "current_weekly_total_count":"6000","current_weekly_usage_count":"3000",
			 "end_time":"1794333000000"}
		]}`)
	}))
	defer server.Close()

	status := NewMiniMax("secret", server.URL, server.Client()).Fetch(context.Background())
	if status.Error != "" {
		t.Fatalf("unexpected error: %s", status.Error)
	}
	if len(status.Windows) != 2 {
		t.Fatalf("got %d windows, want 2: %#v", len(status.Windows), status.Windows)
	}
	window := status.Windows[0]
	if window.Used != 300 || window.Limit != 1500 || window.UsedPercent != 20 {
		t.Errorf("window = %#v", window)
	}
	if window.ResetsAt == nil || window.ResetsAt.UnixMilli() != 1794333000000 {
		t.Errorf("ResetsAt = %#v", window.ResetsAt)
	}
}

func TestMiniMaxFetchRemainsTime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"base_resp":{"status_code":0,"status_msg":"success"},"model_remains":[
			{"model_name":"MiniMax-M2","current_interval_total_count":100,"current_interval_usage_count":40,
			 "remains_time":60000}
		]}`)
	}))
	defer server.Close()

	status := NewMiniMax("secret", server.URL, server.Client()).Fetch(context.Background())
	if status.Error != "" {
		t.Fatalf("unexpected error: %s", status.Error)
	}
	if len(status.Windows) != 1 {
		t.Fatalf("got %d windows, want 1: %#v", len(status.Windows), status.Windows)
	}
	window := status.Windows[0]
	if window.Used != 60 || window.Limit != 100 || window.UsedPercent != 60 {
		t.Errorf("window = %#v", window)
	}
	if window.ResetsAt == nil {
		t.Fatal("ResetsAt should be derived from remains_time")
	}
	if delta := window.ResetsAt.Sub(time.Now()); delta < 50*time.Second || delta > 70*time.Second {
		t.Errorf("ResetsAt = %v, want about 60s ahead", window.ResetsAt)
	}
}

func TestMiniMaxFetchStatusCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"base_resp":{"status_code":2049,"status_msg":"invalid api key"}}`)
	}))
	defer server.Close()

	status := NewMiniMax("secret", server.URL, server.Client()).Fetch(context.Background())
	if status.Error == "" {
		t.Fatal("expected error")
	}
	if !strings.Contains(status.Error, "M Plan") {
		t.Errorf("error = %q, want a hint about the M Plan key", status.Error)
	}
	if !strings.Contains(status.Error, "2049") {
		t.Errorf("error = %q, want the status code", status.Error)
	}
}

func TestMiniMaxFetchHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "boom")
	}))
	defer server.Close()

	status := NewMiniMax("secret", server.URL, server.Client()).Fetch(context.Background())
	if status.Error == "" {
		t.Fatal("expected error")
	}
	if !strings.Contains(status.Error, "500") {
		t.Errorf("error = %q, want the HTTP status", status.Error)
	}
	if status.Windows == nil {
		t.Fatal("Windows must not be nil")
	}
}

func TestMiniMaxFetchDefaults(t *testing.T) {
	provider := NewMiniMax("secret", "", nil)
	if provider.Name() != "minimax" {
		t.Errorf("Name = %q", provider.Name())
	}
	if provider.Interval() != 60*time.Second {
		t.Errorf("Interval = %v", provider.Interval())
	}
	if provider.url != DefaultMiniMaxURL {
		t.Errorf("url = %q", provider.url)
	}
	if provider.client == nil || provider.client.Timeout != 15*time.Second {
		t.Errorf("client = %#v", provider.client)
	}
}

func TestCodexFetch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("codex test requires a POSIX shell")
	}

	bin := writeFakeCodex(t, `
printf '%s\n' '{"jsonrpc":"2.0","method":"session/updated","params":{}}'
printf '%s\n' '{"id":1,"result":{"ok":true}}'
printf '%s\n' '{"id":2,"result":{"rateLimits":{"planType":"plus","primary":{"usedPercent":12,"windowDurationMins":300,"resetsAt":1794333000},"secondary":{"usedPercent":3,"windowDurationMins":10080,"resetsAt":1794675600}}}}'
`)

	status := NewCodex(bin).Fetch(context.Background())
	if status.Error != "" {
		t.Fatalf("unexpected error: %s", status.Error)
	}
	if status.Provider != "openai" || status.Source != "codex-app-server" {
		t.Errorf("identity = %q, %q", status.Provider, status.Source)
	}
	if status.Plan != "plus" {
		t.Errorf("Plan = %q", status.Plan)
	}
	if len(status.Windows) != 2 {
		t.Fatalf("got %d windows, want 2: %#v", len(status.Windows), status.Windows)
	}
	if status.Windows[0].Name != "5h" || status.Windows[0].UsedPercent != 12 {
		t.Errorf("primary window = %#v", status.Windows[0])
	}
	if status.Windows[0].ResetsAt == nil || status.Windows[0].ResetsAt.Unix() != 1794333000 {
		t.Errorf("primary ResetsAt = %#v", status.Windows[0].ResetsAt)
	}
	if status.Windows[1].Name != "7d" || status.Windows[1].UsedPercent != 3 {
		t.Errorf("secondary window = %#v", status.Windows[1])
	}
	if status.Windows[1].ResetsAt == nil || status.Windows[1].ResetsAt.Unix() != 1794675600 {
		t.Errorf("secondary ResetsAt = %#v", status.Windows[1].ResetsAt)
	}
	if status.UpdatedAt.IsZero() {
		t.Error("UpdatedAt must be set")
	}
}

func TestCodexFetchAuthenticationError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("codex test requires a POSIX shell")
	}

	bin := writeFakeCodex(t, `
printf '%s\n' '{"jsonrpc":"2.0","method":"session/updated","params":{}}'
printf '%s\n' '{"id":1,"result":{"ok":true}}'
printf '%s\n' '{"id":2,"error":{"code":-32600,"message":"codex account authentication required to read rate limits"}}'
`)

	status := NewCodex(bin).Fetch(context.Background())
	if status.Error == "" {
		t.Fatal("expected error")
	}
	if !strings.Contains(status.Error, "codex login") {
		t.Errorf("error = %q, want a login hint", status.Error)
	}
	if !strings.Contains(status.Error, "authentication required") {
		t.Errorf("error = %q, want the original message", status.Error)
	}
	if status.Windows == nil {
		t.Fatal("Windows must not be nil")
	}
}

func TestCodexFetchMissingBinary(t *testing.T) {
	status := NewCodex(filepath.Join(t.TempDir(), "missing-codex")).Fetch(context.Background())
	if status.Error == "" {
		t.Fatal("expected error")
	}
	if !strings.Contains(status.Error, "LLMWATCH_CODEX_BIN") {
		t.Errorf("error = %q", status.Error)
	}
	if status.Windows == nil {
		t.Fatal("Windows must not be nil")
	}
}

func TestCodexDefaults(t *testing.T) {
	provider := NewCodex("")
	if provider.Name() != "codex" {
		t.Errorf("Name = %q", provider.Name())
	}
	if provider.Interval() != 60*time.Second {
		t.Errorf("Interval = %v", provider.Interval())
	}
	if provider.bin != "codex" {
		t.Errorf("bin = %q", provider.bin)
	}
}

// writeFakeCodex creates a shell script that mimics the codex app-server:
// it reads the three request lines and then writes a notification plus the
// answer for request id 2.
func writeFakeCodex(t *testing.T, script string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	content := "#!/bin/sh\n" +
		"read line1\n" +
		"read line2\n" +
		"read line3\n" +
		script
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
