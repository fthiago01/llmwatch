package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"

	"llmwatch/internal/quota"
	"llmwatch/internal/source"
	"llmwatch/internal/usage"
)

// fakeSource is a source.Source whose records and fingerprint are
// controlled by the test.
type fakeSource struct {
	name string

	mu          sync.Mutex
	fingerprint string
	records     []usage.Record
	loadErr     error
	fpErr       error
	loadCount   int
}

func newFakeSource(name string) *fakeSource {
	return &fakeSource{name: name, fingerprint: "fp-" + name}
}

// Name returns the source identifier.
func (f *fakeSource) Name() string { return f.name }

// Load returns the configured records.
func (f *fakeSource) Load(ctx context.Context) ([]usage.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadCount++
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return append([]usage.Record(nil), f.records...), nil
}

// Fingerprint returns the configured fingerprint.
func (f *fakeSource) Fingerprint() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fpErr != nil {
		return "", f.fpErr
	}
	return f.fingerprint, nil
}

// set replaces the fingerprint and the records of the source.
func (f *fakeSource) set(fingerprint string, records []usage.Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fingerprint = fingerprint
	f.records = records
}

// setError makes the source fail on Load.
func (f *fakeSource) setError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadErr = err
}

// setFingerprintError makes the source fail on Fingerprint.
func (f *fakeSource) setFingerprintError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fpErr = err
}

// loads returns how many times Load was called.
func (f *fakeSource) loads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loadCount
}

// record builds a usage record with the given token counts.
func record(source, provider, model string, at time.Time, in, out int64) usage.Record {
	return usage.Record{
		Source:   source,
		Provider: provider,
		Model:    model,
		Time:     at,
		Input:    in,
		Output:   out,
	}
}

// testIndex returns the filesystem serving the single page application.
func testIndex() fstest.MapFS {
	return fstest.MapFS{
		"index.html": &fstest.MapFile{
			Data: []byte("<html><body>llmwatch</body></html>"),
			Mode: 0o644,
		},
	}
}

// fakeQuota is a quota.Provider whose status is controlled by the test.
type fakeQuota struct {
	name string

	mu       sync.Mutex
	status   usage.QuotaStatus
	fetchCnt int
}

// newFakeQuota returns a provider reporting the given status.
func newFakeQuota(name string, status usage.QuotaStatus) *fakeQuota {
	return &fakeQuota{name: name, status: status}
}

// Name returns the provider identifier.
func (f *fakeQuota) Name() string { return f.name }

// Interval returns the polling interval of the provider.
func (f *fakeQuota) Interval() time.Duration { return 50 * time.Millisecond }

// Fetch returns the configured status.
func (f *fakeQuota) Fetch(ctx context.Context) usage.QuotaStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetchCnt++
	return f.status
}

// set replaces the status reported by the provider.
func (f *fakeQuota) set(status usage.QuotaStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

// fetches returns how many times the provider was queried.
func (f *fakeQuota) fetches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetchCnt
}

// quotaStatus builds a quota status with a single window.
func quotaStatus(provider, sourceName string, used float64) usage.QuotaStatus {
	return usage.QuotaStatus{
		Provider:  provider,
		Source:    sourceName,
		Plan:      "max",
		Windows:   []usage.QuotaWindow{{Name: "5h", UsedPercent: used, Used: int64(used), Limit: 100}},
		UpdatedAt: time.Now(),
	}
}

// snapshotResponse is the body served by /api/snapshot.
type snapshotResponse struct {
	GeneratedAt time.Time           `json:"generated_at"`
	Windows     []usage.Window      `json:"windows"`
	Blocks      []usage.Block       `json:"blocks"`
	Hourly      []usage.Bucket      `json:"hourly"`
	Quotas      []usage.QuotaStatus `json:"quotas"`
	Errors      []string            `json:"errors"`
	Update      *usage.UpdateInfo   `json:"update,omitempty"`
}

// wsMessage is the envelope sent over the WebSocket.
type wsMessage struct {
	Type string           `json:"type"`
	Data snapshotResponse `json:"data"`
}

// totalTokens returns the total of the first window of the snapshot.
func totalTokens(s snapshotResponse) int64 {
	if len(s.Windows) == 0 {
		return 0
	}
	return s.Windows[0].Sum.Total
}

// TestSnapshotNotReadyBeforeRun checks that /api/snapshot answers 503 before
// the first refresh happened.
func TestSnapshotNotReadyBeforeRun(t *testing.T) {
	src := newFakeSource("claude")
	s := New([]source.Source{src}, nil, 50*time.Millisecond, nil, testIndex())

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/snapshot", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"not ready"}` {
		t.Errorf("body = %q, want not ready error", got)
	}
}

// TestSnapshotAfterRun checks that /api/snapshot serves the aggregated
// totals once the server ran.
func TestSnapshotAfterRun(t *testing.T) {
	now := time.Now()
	src := newFakeSource("claude")
	src.set("fp-1", []usage.Record{
		record("claude", "anthropic", "sonnet", now.Add(-time.Hour), 100, 50),
		record("claude", "anthropic", "sonnet", now.Add(-2*time.Hour), 10, 5),
	})

	s := New([]source.Source{src}, nil, 50*time.Millisecond, nil, testIndex())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// Wait for the first refresh to be published.
	waitFor(t, func() bool {
		return snapshotTotal(t, s) == 165
	}, 2*time.Second)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/snapshot", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json", ct)
	}

	var body snapshotResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Windows) == 0 {
		t.Fatal("no windows in snapshot")
	}
	if got := body.Windows[0].Sum.Total; got != 165 {
		t.Errorf("total = %d, want 165", got)
	}
	if got := body.Windows[0].Sum.Requests; got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
	if len(body.Errors) != 0 {
		t.Errorf("errors = %v, want none", body.Errors)
	}
}

// snapshotTotal returns the total of the last snapshot of the server.
func snapshotTotal(t *testing.T, s *Server) int64 {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasSnapshot {
		return -1
	}
	if len(s.snapshot.Windows) == 0 {
		return 0
	}
	return s.snapshot.Windows[0].Sum.Total
}

// TestWebSocketReceivesSnapshot checks that a client connected to /ws
// receives the current snapshot right away.
func TestWebSocketReceivesSnapshot(t *testing.T) {
	now := time.Now()
	src := newFakeSource("claude")
	src.set("fp-1", []usage.Record{
		record("claude", "anthropic", "sonnet", now.Add(-time.Hour), 7, 3),
	})

	s := New([]source.Source{src}, nil, 50*time.Millisecond, nil, testIndex())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, func() bool {
		return snapshotTotal(t, s) == 10
	}, 2*time.Second)

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	conn, _, err := websocket.Dial(ctx, wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()

	msg := readMessage(t, readCtx, conn)
	if msg.Type != "snapshot" {
		t.Errorf("type = %q, want snapshot", msg.Type)
	}
	if got := totalTokens(msg.Data); got != 10 {
		t.Errorf("total = %d, want 10", got)
	}
}

// TestWebSocketReceivesUpdates checks that a client is notified when the
// source changes.
func TestWebSocketReceivesUpdates(t *testing.T) {
	now := time.Now()
	src := newFakeSource("claude")
	src.set("fp-1", []usage.Record{
		record("claude", "anthropic", "sonnet", now.Add(-time.Hour), 7, 3),
	})

	s := New([]source.Source{src}, nil, 50*time.Millisecond, nil, testIndex())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, func() bool {
		return snapshotTotal(t, s) == 10
	}, 2*time.Second)

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	conn, _, err := websocket.Dial(ctx, wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()

	first := readUntil(t, readCtx, conn, func(msg wsMessage) bool {
		return totalTokens(msg.Data) == 10
	})
	if got := totalTokens(first.Data); got != 10 {
		t.Fatalf("total = %d, want 10", got)
	}

	// The source changes: the client must be notified.
	src.set("fp-2", []usage.Record{
		record("claude", "anthropic", "sonnet", now.Add(-time.Hour), 100, 50),
	})

	second := readUntil(t, readCtx, conn, func(msg wsMessage) bool {
		return totalTokens(msg.Data) == 150
	})
	if got := totalTokens(second.Data); got != 150 {
		t.Errorf("total = %d, want 150", got)
	}
}

// TestWebSocketNoMessageWhenFingerprintStable checks that an unchanged
// source does not produce any new message.
func TestWebSocketNoMessageWhenFingerprintStable(t *testing.T) {
	now := time.Now()
	src := newFakeSource("claude")
	src.set("fp-1", []usage.Record{
		record("claude", "anthropic", "sonnet", now.Add(-time.Hour), 7, 3),
	})

	s := New([]source.Source{src}, nil, 50*time.Millisecond, nil, testIndex())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// Wait for the first snapshot, so the client receives it on connect
	// and the quiet window below starts from a known state.
	waitFor(t, func() bool {
		return snapshotTotal(t, s) == 10
	}, 2*time.Second)

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	conn, _, err := websocket.Dial(ctx, wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()

	first := readMessage(t, readCtx, conn)
	if got := totalTokens(first.Data); got != 10 {
		t.Fatalf("total = %d, want 10", got)
	}

	// Nothing changed: no new message must arrive.
	quietCtx, quietCancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer quietCancel()

	for {
		_, _, err := conn.Read(quietCtx)
		if err != nil {
			break
		}
	}
	if quietCtx.Err() == nil {
		t.Error("received an unexpected message with an unchanged source")
	}
}

// TestSourceErrorIsReported checks that a failing source is reported in the
// snapshot errors while the other sources keep working.
func TestSourceErrorIsReported(t *testing.T) {
	now := time.Now()

	broken := newFakeSource("broken")
	broken.setError(errors.New("boom"))

	ok := newFakeSource("claude")
	ok.set("fp-1", []usage.Record{
		record("claude", "anthropic", "sonnet", now.Add(-time.Hour), 20, 30),
	})

	s := New([]source.Source{broken, ok}, nil, 50*time.Millisecond, nil, testIndex())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, func() bool {
		return snapshotTotal(t, s) == 50
	}, 2*time.Second)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/snapshot", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body snapshotResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Errors) != 1 {
		t.Fatalf("errors = %v, want one error", body.Errors)
	}
	if !strings.Contains(body.Errors[0], "broken") {
		t.Errorf("error = %q, want it to mention the source name", body.Errors[0])
	}
	if !strings.Contains(body.Errors[0], "boom") {
		t.Errorf("error = %q, want it to mention the cause", body.Errors[0])
	}
	// The healthy source still contributed its records.
	if got := body.Windows[0].Sum.Total; got != 50 {
		t.Errorf("total = %d, want 50", got)
	}
}

// TestFingerprintErrorForcesRefresh checks that a fingerprint error does
// not freeze the data.
func TestFingerprintErrorForcesRefresh(t *testing.T) {
	now := time.Now()
	src := newFakeSource("claude")
	src.set("fp-1", []usage.Record{
		record("claude", "anthropic", "sonnet", now.Add(-time.Hour), 5, 5),
	})

	s := New([]source.Source{src}, nil, 50*time.Millisecond, nil, testIndex())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, func() bool {
		return snapshotTotal(t, s) == 10
	}, 2*time.Second)

	src.setFingerprintError(errors.New("stat failed"))

	// The data is refreshed even though the fingerprint cannot be read.
	waitFor(t, func() bool {
		return src.loads() > 1
	}, 2*time.Second)
}

// TestIndexIsServed checks that the index page is served at the root path.
func TestIndexIsServed(t *testing.T) {
	src := newFakeSource("claude")
	s := New([]source.Source{src}, nil, 50*time.Millisecond, nil, testIndex())

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "llmwatch") {
		t.Errorf("body = %q, want it to contain the index page", body)
	}
}

// TestClientRemovedOnDisconnect checks that a disconnected client does not
// leak in the client registry.
func TestClientRemovedOnDisconnect(t *testing.T) {
	src := newFakeSource("claude")
	s := New([]source.Source{src}, nil, 50*time.Millisecond, nil, testIndex())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	conn, _, err := websocket.Dial(ctx, wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	waitFor(t, func() bool {
		return s.clientCount() == 1
	}, 2*time.Second)

	if err := conn.CloseNow(); err != nil {
		t.Fatalf("close: %v", err)
	}

	waitFor(t, func() bool {
		return s.clientCount() == 0
	}, 2*time.Second)
}

// wsURL returns the WebSocket URL of the test server.
func wsURL(ts *httptest.Server) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
}

// readMessage reads one WebSocket message and decodes it.
func readMessage(t *testing.T, ctx context.Context, conn *websocket.Conn) wsMessage {
	t.Helper()

	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var msg wsMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	return msg
}

// readUntil reads WebSocket messages until one satisfies the condition or
// the context expires.
func readUntil(t *testing.T, ctx context.Context, conn *websocket.Conn, cond func(wsMessage) bool) wsMessage {
	t.Helper()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}

		var msg wsMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("decode message: %v", err)
		}
		if cond(msg) {
			return msg
		}
	}
}

// waitFor waits until the condition holds or the timeout expires.
func waitFor(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

// TestQuotaBroadcastOverWebSocket checks that the quota status reaches the
// WebSocket clients and that a change is pushed right away.
func TestQuotaBroadcastOverWebSocket(t *testing.T) {
	now := time.Now()
	src := newFakeSource("claude")
	src.set("fp-1", []usage.Record{
		record("claude", "anthropic", "sonnet", now.Add(-time.Hour), 7, 3),
	})

	qp := newFakeQuota("anthropic", quotaStatus("anthropic", "claude-statusline", 10))
	s := New([]source.Source{src}, []quota.Provider{qp}, 50*time.Millisecond, nil, testIndex())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, func() bool {
		return snapshotTotal(t, s) == 10
	}, 2*time.Second)

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	conn, _, err := websocket.Dial(ctx, wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()

	// The client receives the quota along with the first snapshot.
	msg := readUntil(t, readCtx, conn, func(msg wsMessage) bool {
		return len(msg.Data.Quotas) > 0 && msg.Data.Quotas[0].Provider == "anthropic"
	})
	if got := msg.Data.Quotas[0].Provider; got != "anthropic" {
		t.Errorf("provider = %q, want anthropic", got)
	}
	if got := msg.Data.Quotas[0].Windows[0].UsedPercent; got != 10 {
		t.Errorf("used percent = %v, want 10", got)
	}

	// The quota changes: the client must be notified. Intermediate snapshots
	// may still contain the previous value, so wait for the updated quota.
	qp.set(quotaStatus("anthropic", "claude-statusline", 42))

	second := readUntil(t, readCtx, conn, func(msg wsMessage) bool {
		return len(msg.Data.Quotas) > 0 &&
			len(msg.Data.Quotas[0].Windows) > 0 &&
			msg.Data.Quotas[0].Windows[0].UsedPercent == 42
	})
	if got := second.Data.Quotas[0].Windows[0].UsedPercent; got != 42 {
		t.Errorf("used percent = %v, want 42", got)
	}
	// The token data is still there.
	if got := totalTokens(second.Data); got != 10 {
		t.Errorf("total = %d, want 10", got)
	}
}

// TestQuotaUnchangedDoesNotBroadcast checks that an unchanged quota status
// does not produce any new message.
func TestQuotaUnchangedDoesNotBroadcast(t *testing.T) {
	now := time.Now()
	src := newFakeSource("claude")
	src.set("fp-1", []usage.Record{
		record("claude", "anthropic", "sonnet", now.Add(-time.Hour), 7, 3),
	})

	status := quotaStatus("anthropic", "claude-statusline", 10)
	qp := newFakeQuota("anthropic", status)
	s := New([]source.Source{src}, []quota.Provider{qp}, 50*time.Millisecond, nil, testIndex())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, func() bool {
		return snapshotTotal(t, s) == 10
	}, 2*time.Second)

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	conn, _, err := websocket.Dial(ctx, wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()

	// Consume the messages until the quota shows up.
	readUntil(t, readCtx, conn, func(msg wsMessage) bool {
		return len(msg.Data.Quotas) > 0
	})

	// The quota does not change: no new message must arrive.
	quietCtx, quietCancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer quietCancel()

	for {
		_, _, err := conn.Read(quietCtx)
		if err != nil {
			break
		}
	}
	if quietCtx.Err() == nil {
		t.Error("received an unexpected message with an unchanged quota")
	}
}

// TestSnapshotIncludesQuotas checks that /api/snapshot exposes the quota
// statuses.
func TestSnapshotIncludesQuotas(t *testing.T) {
	now := time.Now()
	src := newFakeSource("claude")
	src.set("fp-1", []usage.Record{
		record("claude", "anthropic", "sonnet", now.Add(-time.Hour), 7, 3),
	})

	qp := newFakeQuota("anthropic", quotaStatus("anthropic", "claude-statusline", 25))
	s := New([]source.Source{src}, []quota.Provider{qp}, 50*time.Millisecond, nil, testIndex())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, func() bool {
		return snapshotTotal(t, s) == 10
	}, 2*time.Second)

	waitFor(t, func() bool {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/snapshot", nil))
		if rec.Code != http.StatusOK {
			return false
		}
		var body snapshotResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			return false
		}
		return len(body.Quotas) == 1
	}, 2*time.Second)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/snapshot", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body snapshotResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Quotas) != 1 {
		t.Fatalf("quotas = %v, want one quota", body.Quotas)
	}
	if got := body.Quotas[0].Provider; got != "anthropic" {
		t.Errorf("provider = %q, want anthropic", got)
	}
	if got := body.Quotas[0].Windows[0].UsedPercent; got != 25 {
		t.Errorf("used percent = %v, want 25", got)
	}
}

// TestSetProvidersAfterRunSwapsSource checks that the sources replaced
// after the server started are used by the next refresh and that the
// WebSocket clients are notified.
func TestSetProvidersAfterRunSwapsSource(t *testing.T) {
	now := time.Now()

	old := newFakeSource("claude")
	old.set("fp-old", []usage.Record{
		record("claude", "anthropic", "sonnet", now.Add(-time.Hour), 7, 3),
	})

	fresh := newFakeSource("codex")
	fresh.set("fp-new", []usage.Record{
		record("codex", "openai", "gpt", now.Add(-time.Hour), 100, 50),
	})

	s := New([]source.Source{old}, nil, 50*time.Millisecond, nil, testIndex())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, func() bool {
		return snapshotTotal(t, s) == 10
	}, 2*time.Second)

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	conn, _, err := websocket.Dial(ctx, wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
	defer readCancel()

	first := readUntil(t, readCtx, conn, func(msg wsMessage) bool {
		return totalTokens(msg.Data) == 10
	})
	if got := totalTokens(first.Data); got != 10 {
		t.Fatalf("total = %d, want 10", got)
	}

	// The source is replaced at runtime.
	s.SetProviders([]source.Source{fresh}, nil, nil)

	// The client must receive a snapshot built from the new source.
	second := readUntil(t, readCtx, conn, func(msg wsMessage) bool {
		return totalTokens(msg.Data) == 150
	})
	if got := totalTokens(second.Data); got != 150 {
		t.Fatalf("total = %d, want 150", got)
	}

	// The old source is not read anymore.
	waitFor(t, func() bool {
		return s.sourceCount() == 1
	}, 2*time.Second)
}

// sourceCount returns the number of registered sources.
func (s *Server) sourceCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sources)
}

// TestSetProvidersAfterRunSwapsQuota checks that the quota providers
// replaced after the server started are the only ones reported in the
// snapshot.
func TestSetProvidersAfterRunSwapsQuota(t *testing.T) {
	now := time.Now()

	src := newFakeSource("claude")
	src.set("fp-1", []usage.Record{
		record("claude", "anthropic", "sonnet", now.Add(-time.Hour), 7, 3),
	})

	oldQuota := newFakeQuota("anthropic", quotaStatus("anthropic", "claude-statusline", 10))
	newQuota := newFakeQuota("openai", quotaStatus("openai", "codex-app-server", 20))

	s := New([]source.Source{src}, []quota.Provider{oldQuota}, 50*time.Millisecond, nil, testIndex())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, func() bool {
		return snapshotTotal(t, s) == 10
	}, 2*time.Second)

	waitFor(t, func() bool {
		return len(s.testSnapshotQuotas()) == 1
	}, 2*time.Second)

	// The quota provider is replaced at runtime.
	s.SetProviders([]source.Source{src}, []quota.Provider{newQuota}, nil)

	// Only the new provider is reported.
	waitFor(t, func() bool {
		quotas := s.testSnapshotQuotas()
		return len(quotas) == 1 && quotas[0].Provider == "openai"
	}, 5*time.Second)

	quotas := s.testSnapshotQuotas()
	if len(quotas) != 1 {
		t.Fatalf("quotas = %v, want one quota", quotas)
	}
	if got := quotas[0].Provider; got != "openai" {
		t.Errorf("provider = %q, want openai", got)
	}
}

// snapshotQuotas returns the quotas of the last snapshot of the server.
func (s *Server) testSnapshotQuotas() []usage.QuotaStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]usage.QuotaStatus(nil), s.snapshot.Quotas...)
}

// TestSetProvidersReportsConfigErrors checks that the configuration errors
// are reported in the snapshot errors, prefixed with "config: ".
func TestSetProvidersReportsConfigErrors(t *testing.T) {
	src := newFakeSource("claude")
	s := New([]source.Source{src}, nil, 50*time.Millisecond, nil, testIndex())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, func() bool {
		return snapshotTotal(t, s) == 0
	}, 2*time.Second)

	s.SetProviders([]source.Source{src}, nil, []string{"bad provider", "missing key"})

	waitFor(t, func() bool {
		return len(s.testSnapshotErrors()) == 2
	}, 5*time.Second)

	errs := s.testSnapshotErrors()
	if len(errs) != 2 {
		t.Fatalf("errors = %v, want two errors", errs)
	}
	if errs[0] != "config: bad provider" {
		t.Errorf("error = %q, want %q", errs[0], "config: bad provider")
	}
	if errs[1] != "config: missing key" {
		t.Errorf("error = %q, want %q", errs[1], "config: missing key")
	}
}

// TestSetProvidersConfigErrorsWithoutSource checks that the configuration
// errors are reported even when there is no source at all.
func TestSetProvidersConfigErrorsWithoutSource(t *testing.T) {
	s := New(nil, nil, 50*time.Millisecond, nil, testIndex())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// Wait for the first refresh to be published.
	waitFor(t, func() bool {
		return s.hasSnapshotNow()
	}, 2*time.Second)

	s.SetProviders(nil, nil, []string{"no provider configured"})

	waitFor(t, func() bool {
		errs := s.testSnapshotErrors()
		return len(errs) == 1 && errs[0] == "config: no provider configured"
	}, 5*time.Second)
}

// snapshotErrors returns the errors of the last snapshot of the server.
func (s *Server) testSnapshotErrors() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.snapshot.Errors...)
}

// hasSnapshotNow tells whether a snapshot was already published.
func (s *Server) hasSnapshotNow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hasSnapshot
}

// TestSetProvidersConcurrent checks that many concurrent calls to
// SetProviders running against a live server neither race nor deadlock.
func TestSetProvidersConcurrent(t *testing.T) {
	now := time.Now()

	src := newFakeSource("claude")
	src.set("fp-1", []usage.Record{
		record("claude", "anthropic", "sonnet", now.Add(-time.Hour), 7, 3),
	})

	s := New([]source.Source{src}, nil, 20*time.Millisecond, nil, testIndex())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, func() bool {
		return snapshotTotal(t, s) == 10
	}, 2*time.Second)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			other := newFakeSource(fmt.Sprintf("source-%d", i))
			other.set(fmt.Sprintf("fp-%d", i), []usage.Record{
				record("other", "openai", "gpt", now.Add(-time.Hour), int64(i+1), 1),
			})

			quotaProvider := newFakeQuota(fmt.Sprintf("quota-%d", i), quotaStatus(fmt.Sprintf("quota-%d", i), "src", float64(i)))

			s.SetProviders([]source.Source{other}, []quota.Provider{quotaProvider}, []string{fmt.Sprintf("error-%d", i)})
		}(i)
	}

	// The wait must not block forever: a deadlock would hang the test.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the concurrent SetProviders calls did not finish")
	}

	// The server is still alive and serving.
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/snapshot", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestSetProvidersBeforeRun checks that the providers set before the
// server starts are the ones used by the first refresh.
func TestSetProvidersBeforeRun(t *testing.T) {
	now := time.Now()

	src := newFakeSource("codex")
	src.set("fp-1", []usage.Record{
		record("codex", "openai", "gpt", now.Add(-time.Hour), 40, 10),
	})

	s := New(nil, nil, 50*time.Millisecond, nil, testIndex())
	s.SetProviders([]source.Source{src}, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, func() bool {
		return snapshotTotal(t, s) == 50
	}, 2*time.Second)
}

// TestOriginCheckForbidden checks that a rejected origin gets a 403
// response and no WebSocket connection.
func TestOriginCheckForbidden(t *testing.T) {
	src := newFakeSource("claude")
	s := New([]source.Source{src}, nil, 50*time.Millisecond, nil, testIndex())
	s.SetOriginCheck(func(r *http.Request) bool { return false })

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, wsURL(ts), nil)
	if err == nil {
		conn.CloseNow()
		t.Fatal("the connection was accepted with a forbidden origin")
	}
	if resp == nil {
		t.Fatal("no HTTP response")
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}

	// The plain HTTP request gets the JSON error.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Origin", "http://example.com")
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"forbidden origin"}` {
		t.Errorf("body = %q, want the forbidden origin error", got)
	}
}

// TestOriginCheckAllowed checks that an accepted origin connects even when
// the origin does not match the host of the server.
func TestOriginCheckAllowed(t *testing.T) {
	now := time.Now()
	src := newFakeSource("claude")
	src.set("fp-1", []usage.Record{
		record("claude", "anthropic", "sonnet", now.Add(-time.Hour), 7, 3),
	})

	s := New([]source.Source{src}, nil, 50*time.Millisecond, nil, testIndex())
	s.SetOriginCheck(func(r *http.Request) bool {
		return r.Header.Get("Origin") == "http://allowed.example"
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, func() bool {
		return snapshotTotal(t, s) == 10
	}, 2*time.Second)

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// The origin does not match the host of the test server: the custom
	// check is the only one deciding.
	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dialCancel()

	conn, _, err := websocket.Dial(dialCtx, wsURL(ts), &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"http://allowed.example"}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
	defer readCancel()

	msg := readMessage(t, readCtx, conn)
	if got := totalTokens(msg.Data); got != 10 {
		t.Errorf("total = %d, want 10", got)
	}
}

// TestOriginCheckForbiddenDifferentOrigin checks that the origin of the
// request is taken into account by the custom check.
func TestOriginCheckForbiddenDifferentOrigin(t *testing.T) {
	src := newFakeSource("claude")
	s := New([]source.Source{src}, nil, 50*time.Millisecond, nil, testIndex())
	s.SetOriginCheck(func(r *http.Request) bool {
		return r.Header.Get("Origin") == "http://allowed.example"
	})

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, wsURL(ts), &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"http://evil.example"}},
	})
	if err == nil {
		conn.CloseNow()
		t.Fatal("the connection was accepted with a forbidden origin")
	}
	if resp == nil {
		t.Fatal("no HTTP response")
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

// TestRunReturnsAfterQuotasFinished checks that Run only returns once every
// quota goroutine finished.
func TestRunReturnsAfterQuotasFinished(t *testing.T) {
	src := newFakeSource("claude")
	qp := newFakeQuota("anthropic", quotaStatus("anthropic", "claude-statusline", 10))
	s := New([]source.Source{src}, []quota.Provider{qp}, 50*time.Millisecond, nil, testIndex())

	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	waitFor(t, func() bool {
		return len(s.testSnapshotQuotas()) == 1
	}, 2*time.Second)

	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}

	// No quota goroutine is left: the generation was stopped.
	s.quotaMu.Lock()
	gen := s.quotaGen
	s.quotaMu.Unlock()
	if gen != nil {
		t.Error("a quota generation is still registered after Run returned")
	}

	// The quota goroutines really left: the goroutine count goes back to
	// its value before the server started.
	waitFor(t, func() bool {
		return runtime.NumGoroutine() <= before
	}, 5*time.Second)
}

// TestSetProvidersStopsOldQuotaGoroutines checks that the quota goroutines
// of the replaced providers are stopped and that the new ones take over.
func TestSetProvidersStopsOldQuotaGoroutines(t *testing.T) {
	src := newFakeSource("claude")

	oldQuota := newFakeQuota("anthropic", quotaStatus("anthropic", "claude-statusline", 10))
	newQuota := newFakeQuota("openai", quotaStatus("openai", "codex-app-server", 20))

	s := New([]source.Source{src}, []quota.Provider{oldQuota}, 50*time.Millisecond, nil, testIndex())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, func() bool {
		return len(s.testSnapshotQuotas()) == 1
	}, 2*time.Second)

	// The old provider is polled.
	waitFor(t, func() bool {
		return oldQuota.fetches() > 0
	}, 2*time.Second)

	// The provider is replaced: the old goroutine must stop.
	s.SetProviders([]source.Source{src}, []quota.Provider{newQuota}, nil)

	waitFor(t, func() bool {
		return newQuota.fetches() > 0
	}, 5*time.Second)

	// The old provider is not polled anymore.
	fetches := oldQuota.fetches()
	time.Sleep(300 * time.Millisecond)
	if got := oldQuota.fetches(); got != fetches {
		t.Errorf("the old provider was polled %d more times after the replacement", got-fetches)
	}
}

// TestSetUpdateBeforeFirstSnapshot checks that an update status received
// before the first refresh is part of the first published snapshot.
func TestSetUpdateBeforeFirstSnapshot(t *testing.T) {
	now := time.Now()
	src := newFakeSource("claude")
	src.set("fp-1", []usage.Record{
		record("claude", "anthropic", "sonnet", now.Add(-time.Hour), 7, 3),
	})

	s := New([]source.Source{src}, nil, 50*time.Millisecond, nil, testIndex())

	s.SetUpdate(usage.UpdateInfo{
		Current:   "v1.2.3",
		Latest:    "v1.3.0",
		Available: true,
		URL:       "https://example.com/release",
		CheckedAt: time.Now(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// The first snapshot already carries the update status.
	waitFor(t, func() bool {
		return s.testSnapshotUpdate() != nil
	}, 2*time.Second)

	update := s.testSnapshotUpdate()
	if update == nil {
		t.Fatal("no update status in the snapshot")
	}
	if got := update.Current; got != "v1.2.3" {
		t.Errorf("current = %q, want v1.2.3", got)
	}
	if got := update.Latest; got != "v1.3.0" {
		t.Errorf("latest = %q, want v1.3.0", got)
	}
	if !update.Available {
		t.Error("available = false, want true")
	}
}

// TestSetUpdateBroadcastsOverWebSocket checks that a client connected to /ws
// is notified when the update status changes.
func TestSetUpdateBroadcastsOverWebSocket(t *testing.T) {
	now := time.Now()
	src := newFakeSource("claude")
	src.set("fp-1", []usage.Record{
		record("claude", "anthropic", "sonnet", now.Add(-time.Hour), 7, 3),
	})

	s := New([]source.Source{src}, nil, 50*time.Millisecond, nil, testIndex())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, func() bool {
		return snapshotTotal(t, s) == 10
	}, 2*time.Second)

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	conn, _, err := websocket.Dial(ctx, wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()

	// The client receives the first snapshot.
	first := readUntil(t, readCtx, conn, func(msg wsMessage) bool {
		return totalTokens(msg.Data) == 10
	})
	if got := totalTokens(first.Data); got != 10 {
		t.Fatalf("total = %d, want 10", got)
	}

	// A new version is available: the client must be notified.
	s.SetUpdate(usage.UpdateInfo{
		Current:   "v1.2.3",
		Latest:    "v1.3.0",
		Available: true,
		URL:       "https://example.com/release",
		CheckedAt: time.Now(),
	})

	second := readUntil(t, readCtx, conn, func(msg wsMessage) bool {
		return msg.Data.Update != nil && msg.Data.Update.Available
	})
	if second.Data.Update == nil {
		t.Fatal("no update status in the message")
	}
	if got := second.Data.Update.Latest; got != "v1.3.0" {
		t.Errorf("latest = %q, want v1.3.0", got)
	}
	// The token data is still there.
	if got := totalTokens(second.Data); got != 10 {
		t.Errorf("total = %d, want 10", got)
	}
}

// TestSetUpdateSameStatusDoesNotBroadcast checks that an update status equal
// to the previous one, ignoring the moment of the check, does not produce
// any new message.
func TestSetUpdateSameStatusDoesNotBroadcast(t *testing.T) {
	now := time.Now()
	src := newFakeSource("claude")
	src.set("fp-1", []usage.Record{
		record("claude", "anthropic", "sonnet", now.Add(-time.Hour), 7, 3),
	})

	s := New([]source.Source{src}, nil, 50*time.Millisecond, nil, testIndex())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, func() bool {
		return snapshotTotal(t, s) == 10
	}, 2*time.Second)

	info := usage.UpdateInfo{
		Current:   "v1.2.3",
		Latest:    "v1.3.0",
		Available: true,
		URL:       "https://example.com/release",
		CheckedAt: time.Now(),
	}
	s.SetUpdate(info)

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	conn, _, err := websocket.Dial(ctx, wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()

	// Consume the messages until the update status shows up.
	readUntil(t, readCtx, conn, func(msg wsMessage) bool {
		return msg.Data.Update != nil && msg.Data.Update.Available
	})

	// The same status is reported again, with another check time: no new
	// message must arrive.
	info.CheckedAt = time.Now().Add(time.Minute)
	s.SetUpdate(info)

	quietCtx, quietCancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer quietCancel()

	for {
		_, _, err := conn.Read(quietCtx)
		if err != nil {
			break
		}
	}
	if quietCtx.Err() == nil {
		t.Error("received an unexpected message with an unchanged update status")
	}
}

// TestSnapshotIncludesUpdate checks that /api/snapshot exposes the update
// status.
func TestSnapshotIncludesUpdate(t *testing.T) {
	now := time.Now()
	src := newFakeSource("claude")
	src.set("fp-1", []usage.Record{
		record("claude", "anthropic", "sonnet", now.Add(-time.Hour), 7, 3),
	})

	s := New([]source.Source{src}, nil, 50*time.Millisecond, nil, testIndex())

	s.SetUpdate(usage.UpdateInfo{
		Current:   "v1.2.3",
		Latest:    "v1.3.0",
		Available: true,
		URL:       "https://example.com/release",
		CheckedAt: time.Now(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, func() bool {
		return snapshotTotal(t, s) == 10
	}, 2*time.Second)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/snapshot", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body snapshotResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Update == nil {
		t.Fatal("no update status in the snapshot")
	}
	if got := body.Update.Current; got != "v1.2.3" {
		t.Errorf("current = %q, want v1.2.3", got)
	}
	if got := body.Update.Latest; got != "v1.3.0" {
		t.Errorf("latest = %q, want v1.3.0", got)
	}
	if !body.Update.Available {
		t.Error("available = false, want true")
	}
}

// testSnapshotUpdate returns the update status of the last snapshot of the
// server.
func (s *Server) testSnapshotUpdate() *usage.UpdateInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot.Update == nil {
		return nil
	}
	copied := *s.snapshot.Update
	return &copied
}
