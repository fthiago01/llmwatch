package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"llmwatch/internal/usage"
)

func TestNewer(t *testing.T) {
	tests := []struct {
		name    string
		latest  string
		current string
		want    bool
	}{
		{name: "newer patch", latest: "v0.2.3", current: "v0.2.2", want: true},
		{name: "newer minor numeric", latest: "0.10.0", current: "0.9.9", want: true},
		{name: "newer major", latest: "v1.0.0", current: "v0.9.9", want: true},
		{name: "final beats prerelease", latest: "v1.0.0", current: "v1.0.0-rc1", want: true},
		{name: "equal", latest: "v1.0.0", current: "v1.0.0", want: false},
		{name: "equal with prefix", latest: "v1.2.3", current: "1.2.3", want: false},
		{name: "older", latest: "v0.2.1", current: "v0.2.2", want: false},
		{name: "older prerelease", latest: "v1.0.0-rc1", current: "v1.0.0", want: false},
		{name: "dev current", latest: "v9.9.9", current: "dev", want: false},
		{name: "empty current", latest: "v9.9.9", current: "", want: false},
		{name: "invalid current", latest: "v9.9.9", current: "not-a-version", want: false},
		{name: "invalid latest", latest: "banana", current: "v0.2.2", want: false},
		{name: "invalid latest short", latest: "v1.2", current: "v1.2.3", want: false},
		{name: "invalid latest empty", latest: "", current: "v1.2.3", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Newer(test.latest, test.current); got != test.want {
				t.Fatalf("Newer(%q, %q) = %v, want %v", test.latest, test.current, got, test.want)
			}
		})
	}
}

// newServer starts a test server answering with the given status and body.
func newServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestCheckNewerRelease(t *testing.T) {
	server := newServer(t, http.StatusOK, `{"tag_name":"v0.2.3","html_url":"https://github.com/fthiago01/llmwatch/releases/tag/v0.2.3"}`)

	checker := New("v0.2.2", server.URL, server.Client())
	info := checker.Check(context.Background())

	if !info.Available {
		t.Fatalf("expected an update to be available, got %+v", info)
	}
	if info.Current != "v0.2.2" {
		t.Fatalf("Current = %q, want %q", info.Current, "v0.2.2")
	}
	if info.Latest != "v0.2.3" {
		t.Fatalf("Latest = %q, want %q", info.Latest, "v0.2.3")
	}
	if info.URL != "https://github.com/fthiago01/llmwatch/releases/tag/v0.2.3" {
		t.Fatalf("URL = %q, want the release html_url", info.URL)
	}
	if info.Error != "" {
		t.Fatalf("Error = %q, want empty", info.Error)
	}
	if info.CheckedAt.IsZero() {
		t.Fatal("CheckedAt must be set")
	}
}

func TestCheckSameVersion(t *testing.T) {
	server := newServer(t, http.StatusOK, `{"tag_name":"v0.2.2","html_url":"https://example.test/v0.2.2"}`)

	checker := New("v0.2.2", server.URL, server.Client())
	info := checker.Check(context.Background())

	if info.Available {
		t.Fatalf("expected no update, got %+v", info)
	}
	if info.Latest != "v0.2.2" {
		t.Fatalf("Latest = %q, want %q", info.Latest, "v0.2.2")
	}
	if info.Error != "" {
		t.Fatalf("Error = %q, want empty", info.Error)
	}
}

func TestCheckPrerelease(t *testing.T) {
	server := newServer(t, http.StatusOK, `{"tag_name":"v0.2.3","html_url":"https://example.test/v0.2.3","prerelease":true}`)

	checker := New("v0.2.2", server.URL, server.Client())
	info := checker.Check(context.Background())

	if info.Available {
		t.Fatalf("expected no update for a prerelease, got %+v", info)
	}
	if info.Latest != "" {
		t.Fatalf("Latest = %q, want empty for a prerelease", info.Latest)
	}
}

func TestCheckDraft(t *testing.T) {
	server := newServer(t, http.StatusOK, `{"tag_name":"v0.2.3","html_url":"https://example.test/v0.2.3","draft":true}`)

	checker := New("v0.2.2", server.URL, server.Client())
	info := checker.Check(context.Background())

	if info.Available {
		t.Fatalf("expected no update for a draft, got %+v", info)
	}
}

func TestCheckRateLimited(t *testing.T) {
	server := newServer(t, http.StatusForbidden, `{"message":"rate limit exceeded"}`)

	checker := New("v0.2.2", server.URL, server.Client())
	info := checker.Check(context.Background())

	if info.Available {
		t.Fatalf("expected no update on a rate limit, got %+v", info)
	}
	if !strings.Contains(info.Error, "limite de requisições do GitHub") {
		t.Fatalf("Error = %q, want a rate limit message", info.Error)
	}
	if info.Current != "v0.2.2" {
		t.Fatalf("Current = %q, want %q", info.Current, "v0.2.2")
	}
}

func TestCheckHTTPError(t *testing.T) {
	server := newServer(t, http.StatusInternalServerError, `{"message":"boom"}`)

	checker := New("v0.2.2", server.URL, server.Client())
	info := checker.Check(context.Background())

	if info.Available {
		t.Fatalf("expected no update on a server error, got %+v", info)
	}
	if !strings.Contains(info.Error, "github: HTTP 500") {
		t.Fatalf("Error = %q, want %q", info.Error, "github: HTTP 500")
	}
}

func TestCheckSendsUserAgent(t *testing.T) {
	var (
		mu      sync.Mutex
		accept  string
		agent   string
		headers bool
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		accept = r.Header.Get("Accept")
		agent = r.Header.Get("User-Agent")
		headers = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v0.2.3"}`))
	}))
	defer server.Close()

	checker := New("v0.2.2", server.URL, server.Client())
	checker.Check(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if !headers {
		t.Fatal("the request did not reach the server")
	}
	if agent != "llmwatch/v0.2.2" {
		t.Fatalf("User-Agent = %q, want %q", agent, "llmwatch/v0.2.2")
	}
	if accept != "application/vnd.github+json" {
		t.Fatalf("Accept = %q, want %q", accept, "application/vnd.github+json")
	}
}

func TestCheckInvalidJSON(t *testing.T) {
	server := newServer(t, http.StatusOK, `not json`)

	checker := New("v0.2.2", server.URL, server.Client())
	info := checker.Check(context.Background())

	if info.Available {
		t.Fatalf("expected no update for an invalid body, got %+v", info)
	}
	if info.Error == "" {
		t.Fatal("Error must be set for an invalid body")
	}
}

func TestCheckDefaultURLAndClient(t *testing.T) {
	checker := New("v0.2.2", "", nil)
	if checker.apiURL != DefaultAPIURL {
		t.Fatalf("apiURL = %q, want %q", checker.apiURL, DefaultAPIURL)
	}
	if checker.client == nil || checker.client.Timeout != 15*time.Second {
		t.Fatalf("client = %+v, want a client with a 15s timeout", checker.client)
	}
}

func TestRunDevDoesNotRequest(t *testing.T) {
	var requests int
	var (
		mu    sync.Mutex
		calls int
		seen  usage.UpdateInfo
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9"}`))
	}))
	defer server.Close()

	checker := New("dev", server.URL, server.Client())
	checker.Run(context.Background(), 10*time.Millisecond, 0, func(info usage.UpdateInfo) {
		mu.Lock()
		calls++
		seen = info
		mu.Unlock()
	})

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("notify called %d times, want 1", calls)
	}
	if requests != 0 {
		t.Fatalf("%d requests were made, want none", requests)
	}
	if seen.Current != "dev" {
		t.Fatalf("Current = %q, want %q", seen.Current, "dev")
	}
	if seen.Available {
		t.Fatalf("expected no update for a dev build, got %+v", seen)
	}
}

func TestRunChecksPeriodically(t *testing.T) {
	server := newServer(t, http.StatusOK, `{"tag_name":"v0.2.3","html_url":"https://example.test/v0.2.3"}`)

	var (
		mu    sync.Mutex
		calls int
	)
	checker := New("v0.2.2", server.URL, server.Client())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		checker.Run(ctx, 20*time.Millisecond, 0, func(info usage.UpdateInfo) {
			mu.Lock()
			calls++
			count := calls
			mu.Unlock()
			if count == 2 {
				cancel()
			}
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("Run did not stop after the context was cancelled")
	}

	mu.Lock()
	defer mu.Unlock()
	if calls < 2 {
		t.Fatalf("notify called %d times, want at least 2", calls)
	}
}

func TestRunRespectsInitialDelay(t *testing.T) {
	server := newServer(t, http.StatusOK, `{"tag_name":"v0.2.3"}`)

	var (
		mu    sync.Mutex
		calls int
	)
	checker := New("v0.2.2", server.URL, server.Client())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	go checker.Run(ctx, time.Hour, 60*time.Millisecond, func(info usage.UpdateInfo) {
		mu.Lock()
		calls++
		mu.Unlock()
	})

	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	early := calls
	mu.Unlock()
	if early != 0 {
		t.Fatalf("notify called %d times before the initial delay, want 0", early)
	}

	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("notify called %d times, want 1", calls)
	}
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Fatalf("the first check happened after %v, want at least 60ms", elapsed)
	}
}

func TestRunStopsOnCancelledContext(t *testing.T) {
	server := newServer(t, http.StatusOK, `{"tag_name":"v0.2.3"}`)

	checker := New("v0.2.2", server.URL, server.Client())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		checker.Run(ctx, time.Hour, time.Hour, func(info usage.UpdateInfo) {})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}
