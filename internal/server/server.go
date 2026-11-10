// Package server exposes the llmwatch usage data over HTTP and WebSocket.
//
// The server aggregates the records of every source into a snapshot and
// pushes it to the connected WebSocket clients whenever the data changes.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/coder/websocket"

	"llmwatch/internal/quota"
	"llmwatch/internal/source"
	"llmwatch/internal/usage"
)

const (
	// writeTimeout bounds a single WebSocket write.
	writeTimeout = 10 * time.Second
	// pingInterval is the interval between two WebSocket pings.
	pingInterval = 30 * time.Second
	// pingTimeout bounds the wait for the pong answering a ping.
	pingTimeout = 10 * time.Second
	// forcedRefreshInterval is the interval between two refreshes performed
	// even when no source changed, so the time windows and the block
	// counters keep moving.
	forcedRefreshInterval = 60 * time.Second
	// quotaFetchTimeout bounds a single quota fetch.
	quotaFetchTimeout = 30 * time.Second
)

// message is the envelope sent to the WebSocket clients.
type message struct {
	Type string         `json:"type"`
	Data usage.Snapshot `json:"data"`
}

// client is one connected WebSocket client.
type client struct {
	conn *websocket.Conn
	// ch carries the pending payload to the writer goroutine. It has a
	// buffer of one element: a broadcast never blocks, the newest message
	// replaces the pending one.
	ch chan []byte
	// done is closed when the client must be removed.
	done chan struct{}
	// closeOnce guards the closing of done.
	closeOnce sync.Once
}

// close signals the writer goroutine to stop.
func (c *client) close() {
	c.closeOnce.Do(func() { close(c.done) })
}

// quotaGeneration holds the goroutines polling one set of quota providers.
//
// Every call to SetProviders starts a new generation: the previous one is
// canceled and waited for, so no goroutine outlives its providers.
type quotaGeneration struct {
	// id identifies the generation: the statuses published by a stale
	// generation are dropped.
	id uint64
	// cancel stops the goroutines of the generation.
	cancel context.CancelFunc
	// wg waits for the goroutines of the generation.
	wg sync.WaitGroup
}

// Server aggregates sources and serves the result over HTTP and WebSocket.
type Server struct {
	interval time.Duration
	limits   map[string]int64
	index    fs.FS

	// mu guards the fields below.
	mu sync.Mutex
	// setMu serializes the calls to SetProviders: the last call wins.
	setMu sync.Mutex
	// quotaMu serializes the transitions of the quota generations: a
	// generation is never started while another one is being stopped.
	quotaMu sync.Mutex
	// sources holds the sources read by the refreshes.
	sources []source.Source
	// quotas holds the quota providers polled by the quota goroutines.
	quotas []quota.Provider
	// configErrors holds the errors of the configuration, reported in
	// the snapshot errors.
	configErrors []string
	// loadErrors holds the errors of the last refresh of the sources.
	loadErrors []string
	// snapshot holds the last aggregated snapshot.
	snapshot usage.Snapshot
	// hasSnapshot tells whether a refresh already happened.
	hasSnapshot bool
	// payload holds the serialized WebSocket message of the last snapshot.
	payload []byte
	// clients holds the connected WebSocket clients.
	clients map[*client]struct{}

	// quotaStatus holds the last quota status reported by every provider,
	// keyed by provider name.
	quotaStatus map[string]usage.QuotaStatus
	// quotaNames holds the names of the registered quota providers.
	quotaNames map[string]struct{}

	// update holds the last update status reported by the update checker.
	// It is nil when no status was received yet.
	update *usage.UpdateInfo

	// genCounter numbers the generations of quota goroutines.
	genCounter uint64
	// genID is the identifier of the current generation of quota
	// goroutines.
	genID uint64

	// fingerprints holds the concatenated fingerprint of the last refresh,
	// used to skip the refresh when nothing changed.
	fingerprint string
	// fingerprintSet tells whether a fingerprint was already computed.
	fingerprintSet bool

	// baseCtx is the context of the running server. It outlives the HTTP
	// requests: a hijacked WebSocket connection must not be tied to the
	// request context, which is canceled as soon as the handler returns.
	baseCtx context.Context
	// runCtx is the context of the running Run loop.
	runCtx context.Context
	// running tells whether Run is currently running.
	running bool
	// quotaGen is the generation of the quota goroutines currently
	// running. It is nil when no quota goroutine is alive.
	quotaGen *quotaGeneration
	// refreshCh asks the Run loop to refresh the data right away. It has
	// a buffer of one element: a pending request is enough.
	refreshCh chan struct{}
	// originCheck verifies the origin of the WebSocket requests. It is
	// nil when the default verification of the library is used.
	originCheck func(r *http.Request) bool
}

// New returns a server reading from the given sources.
//
// The interval defines how often the sources are checked for changes and the
// limits map the per provider token limits. The index filesystem holds the
// static assets served at the root path. The quota providers are polled
// independently, each one at its own interval.
//
// The sources and the quotas are the initial configuration: it is the
// equivalent of a SetProviders call made before Run.
func New(sources []source.Source, quotas []quota.Provider, interval time.Duration, limits map[string]int64, index fs.FS) *Server {
	s := &Server{
		interval:    interval,
		limits:      limits,
		index:       index,
		clients:     map[*client]struct{}{},
		quotaStatus: map[string]usage.QuotaStatus{},
		quotaNames:  map[string]struct{}{},
		refreshCh:   make(chan struct{}, 1),
	}
	s.sources = append([]source.Source(nil), sources...)
	s.quotas = append([]quota.Provider(nil), quotas...)
	for _, p := range s.quotas {
		s.quotaNames[p.Name()] = struct{}{}
	}
	return s
}

// SetProviders replaces the sources and the quota providers of the server.
//
// It may be called before or after Run, from any goroutine, as many times
// as needed. The new sources are used by the next refresh and the new
// quota providers are polled right away when the server is running.
//
// The configErrors are the errors of the configuration: they are reported
// in the snapshot errors, prefixed with "config: ".
func (s *Server) SetProviders(sources []source.Source, quotas []quota.Provider, configErrors []string) {
	s.setMu.Lock()
	defer s.setMu.Unlock()

	s.mu.Lock()
	s.sources = append([]source.Source(nil), sources...)
	s.quotas = append([]quota.Provider(nil), quotas...)
	s.configErrors = append([]string(nil), configErrors...)
	s.fingerprint = ""
	s.fingerprintSet = false

	// The statuses of the providers that are gone are dropped: the
	// snapshot only holds the statuses of the registered providers.
	names := make(map[string]struct{}, len(s.quotas))
	for _, p := range s.quotas {
		names[p.Name()] = struct{}{}
	}
	s.quotaNames = names
	for name := range s.quotaStatus {
		if _, ok := names[name]; !ok {
			delete(s.quotaStatus, name)
		}
	}

	s.mu.Unlock()

	// The quota providers of the previous generation are stopped and
	// the new ones are started.
	s.restartQuotas(quotas)

	// Ask the Run loop to refresh the data right away.
	s.requestRefresh()
}

// SetUpdate stores the update status of the program.
//
// It may be called before or after Run, from any goroutine, as many times
// as needed. The status is part of the snapshot published to the clients:
// when it changes and a snapshot was already published, the new snapshot is
// broadcast right away.
//
// A status equal to the previous one, ignoring the moment of the check, is
// not published again: only the stored copy is refreshed.
func (s *Server) SetUpdate(info usage.UpdateInfo) {
	s.mu.Lock()
	unchanged := s.update != nil && sameUpdate(*s.update, info)
	s.update = &info
	snapshot := s.snapshot
	ready := s.hasSnapshot
	if ready {
		snapshot.Quotas = s.quotaStatuses()
		snapshot.Errors = s.snapshotErrors(s.loadErrors...)
		snapshot.Update = s.updateCopy()
	}
	s.mu.Unlock()

	if unchanged {
		// Nothing changed: the clients are not bothered.
		return
	}

	if !ready {
		// There is no token snapshot yet: the status is only kept and
		// published by the first refresh.
		return
	}

	payload, err := json.Marshal(message{Type: "snapshot", Data: snapshot})
	if err != nil {
		log.Printf("server: marshal snapshot: %v", err)
		return
	}

	s.mu.Lock()
	s.snapshot = snapshot
	s.payload = payload
	s.mu.Unlock()

	s.broadcast(payload)
}

// sameUpdate reports whether two update statuses are equal, ignoring the
// moment of the check.
func sameUpdate(a, b usage.UpdateInfo) bool {
	a.CheckedAt = time.Time{}
	b.CheckedAt = time.Time{}
	return reflect.DeepEqual(a, b)
}

// updateCopy returns a copy of the stored update status.
//
// It returns nil when no status was received yet.
//
// The caller must hold the mutex.
func (s *Server) updateCopy() *usage.UpdateInfo {
	if s.update == nil {
		return nil
	}
	copied := *s.update
	return &copied
}

// restartQuotas stops the quota goroutines of the previous generation and
// starts the ones of the given providers.
//
// The goroutines of the previous generation are canceled and waited for
// before the new ones start, so two generations never poll at the same
// time.
func (s *Server) restartQuotas(providers []quota.Provider) {
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()

	s.mu.Lock()
	old := s.quotaGen
	s.quotaGen = nil
	running := s.running
	runCtx := s.runCtx
	s.mu.Unlock()

	if old != nil {
		old.cancel()
		old.wg.Wait()
	}

	if !running || runCtx == nil {
		// The server is not running: Run starts the current providers
		// when it starts.
		return
	}

	s.startQuotaGenerationLocked(runCtx, providers)
}

// startQuotaGenerationLocked starts the goroutines polling the given
// providers.
//
// The new generation becomes the current one: the statuses published by the
// previous generations are dropped.
//
// The caller must hold the quota mutex.
func (s *Server) startQuotaGenerationLocked(ctx context.Context, providers []quota.Provider) {
	quotaCtx, cancel := context.WithCancel(ctx)

	s.mu.Lock()
	s.genCounter++
	id := s.genCounter
	s.genID = id
	s.mu.Unlock()

	gen := &quotaGeneration{id: id, cancel: cancel}
	for _, p := range providers {
		gen.wg.Add(1)
		go func(p quota.Provider) {
			defer gen.wg.Done()
			s.runQuota(quotaCtx, p, gen.id)
		}(p)
	}

	s.mu.Lock()
	s.quotaGen = gen
	s.mu.Unlock()
}

// requestRefresh asks the Run loop to refresh the data right away.
//
// The refresh is performed by the Run loop, not by the caller: the caller
// never blocks and never runs a refresh concurrently with the loop.
func (s *Server) requestRefresh() {
	select {
	case s.refreshCh <- struct{}{}:
	default:
		// A refresh is already pending: it will pick up the new
		// configuration anyway.
	}
}

// SetOriginCheck sets the function verifying the origin of the WebSocket
// requests.
//
// A nil function restores the default verification of the library. When a
// function is set, it decides alone whether the request is accepted.
func (s *Server) SetOriginCheck(f func(r *http.Request) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.originCheck = f
}

// originAllowed reports whether the origin of the request is accepted.
//
// The second result tells whether a custom check is in place: when it is
// false, the default verification of the library must be used.
func (s *Server) originAllowed(r *http.Request) (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.originCheck == nil {
		return false, false
	}
	return s.originCheck(r), true
}

// Run refreshes the data until the context is canceled.
//
// It blocks until ctx.Done() and until every quota goroutine finished.
func (s *Server) Run(ctx context.Context) {
	baseCtx, baseCancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.baseCtx = baseCtx
	s.runCtx = ctx
	s.running = true
	s.mu.Unlock()

	defer func() {
		baseCancel()
		s.closeClients()

		// The server stops accepting new quota providers before the
		// current ones are stopped: a SetProviders call running at
		// the same time must not start a generation that would
		// outlive Run.
		s.quotaMu.Lock()
		s.mu.Lock()
		s.running = false
		s.runCtx = nil
		s.mu.Unlock()
		s.stopQuotasLocked()
		s.quotaMu.Unlock()
	}()

	// Every quota provider is polled by its own goroutine.
	s.startQuotas(ctx)

	s.refresh(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	forced := time.NewTicker(forcedRefreshInterval)
	defer forced.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.refreshCh:
			s.refresh(ctx)
		case <-ticker.C:
			if s.changed() {
				s.refresh(ctx)
			}
		case <-forced.C:
			s.refresh(ctx)
		}
	}
}

// startQuotas starts the goroutines polling the current quota providers.
func (s *Server) startQuotas(ctx context.Context) {
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()

	s.mu.Lock()
	providers := append([]quota.Provider(nil), s.quotas...)
	s.mu.Unlock()

	s.startQuotaGenerationLocked(ctx, providers)
}

// stopQuotas stops the quota goroutines and waits for them.
//
// The caller must hold the quota mutex.
func (s *Server) stopQuotasLocked() {
	s.mu.Lock()
	gen := s.quotaGen
	s.quotaGen = nil
	s.mu.Unlock()

	if gen == nil {
		return
	}
	gen.cancel()
	gen.wg.Wait()
}

// runQuota polls a quota provider until the context is canceled.
//
// The provider is queried right away and then at every interval. Each status
// is compared to the previous one: only a change is broadcast.
func (s *Server) runQuota(ctx context.Context, p quota.Provider, genID uint64) {
	interval := p.Interval()
	if interval <= 0 {
		interval = time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		s.fetchQuota(ctx, p, genID)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// fetchQuota queries a quota provider and publishes the result when it
// changed.
func (s *Server) fetchQuota(ctx context.Context, p quota.Provider, genID uint64) {
	fetchCtx, cancel := context.WithTimeout(ctx, quotaFetchTimeout)
	defer cancel()

	status := p.Fetch(fetchCtx)
	s.setQuotaStatus(p.Name(), status, genID)
}

// setQuotaStatus stores the status of a quota provider and broadcasts the
// new snapshot when the status changed.
//
// The status of a provider that is no longer registered is dropped, as well
// as the status published by a stale generation of quota goroutines.
func (s *Server) setQuotaStatus(name string, status usage.QuotaStatus, genID uint64) {
	s.mu.Lock()
	if genID != s.genID {
		// The provider was replaced while the status was fetched: the
		// status is stale.
		s.mu.Unlock()
		return
	}
	if _, ok := s.quotaNames[name]; !ok {
		// The provider is not registered anymore.
		s.mu.Unlock()
		return
	}
	prev, ok := s.quotaStatus[name]
	if ok && reflect.DeepEqual(prev, status) {
		// Nothing changed: no need to bother the clients.
		s.mu.Unlock()
		return
	}
	s.quotaStatus[name] = status
	snapshot := s.snapshot
	ready := s.hasSnapshot
	if ready {
		snapshot.Quotas = s.quotaStatuses()
		snapshot.Errors = s.snapshotErrors(s.loadErrors...)
		snapshot.Update = s.updateCopy()
	}
	s.mu.Unlock()

	if !ready {
		// There is no token snapshot yet: the status is only kept.
		return
	}

	payload, err := json.Marshal(message{Type: "snapshot", Data: snapshot})
	if err != nil {
		log.Printf("server: marshal snapshot: %v", err)
		return
	}

	s.mu.Lock()
	s.payload = payload
	s.mu.Unlock()

	s.broadcast(payload)
}

// quotaStatuses returns the known quota statuses ordered by provider name.
//
// The caller must hold the mutex.
func (s *Server) quotaStatuses() []usage.QuotaStatus {
	names := make([]string, 0, len(s.quotaStatus))
	for name := range s.quotaStatus {
		names = append(names, name)
	}
	sort.Strings(names)

	statuses := make([]usage.QuotaStatus, 0, len(names))
	for _, name := range names {
		statuses = append(statuses, s.quotaStatus[name])
	}
	return statuses
}

// changed reports whether the concatenated fingerprint of the sources differs
// from the one of the last refresh.
//
// A fingerprint error forces a refresh, so a broken source is still
// reported instead of silently freezing the data.
func (s *Server) changed() bool {
	fp, err := s.computeFingerprint()
	if err != nil {
		log.Printf("server: fingerprint: %v", err)
		return true
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.fingerprintSet {
		s.fingerprintSet = true
		s.fingerprint = fp
		return true
	}
	if fp == s.fingerprint {
		return false
	}
	s.fingerprint = fp
	return true
}

// computeFingerprint returns the concatenation of the fingerprint of every
// source.
func (s *Server) computeFingerprint() (string, error) {
	s.mu.Lock()
	sources := append([]source.Source(nil), s.sources...)
	s.mu.Unlock()

	var out string
	for _, src := range sources {
		fp, err := src.Fingerprint()
		if err != nil {
			return "", fmt.Errorf("%s: %w", src.Name(), err)
		}
		out += src.Name() + ":" + fp + "\n"
	}
	return out, nil
}

// refresh loads every source, aggregates the records and broadcasts the
// resulting snapshot.
//
// A failing source does not stop the refresh: its error is reported in the
// snapshot errors and the remaining sources are still loaded.
func (s *Server) refresh(ctx context.Context) {
	s.mu.Lock()
	sources := append([]source.Source(nil), s.sources...)
	s.mu.Unlock()

	var (
		recs []usage.Record
		errs []string
	)
	for _, src := range sources {
		loaded, err := src.Load(ctx)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", src.Name(), err))
			log.Printf("server: load %s: %v", src.Name(), err)
			continue
		}
		recs = append(recs, loaded...)
	}

	snapshot := usage.Aggregate(recs, time.Now(), s.limits)

	s.mu.Lock()
	// The quotas collected so far belong to the snapshot as well.
	snapshot.Quotas = s.quotaStatuses()
	// The errors of the configuration are reported next to the load
	// errors.
	s.loadErrors = errs
	snapshot.Errors = s.snapshotErrors(errs...)
	// The update status of the program is part of the snapshot.
	snapshot.Update = s.updateCopy()
	s.mu.Unlock()

	payload, err := json.Marshal(message{Type: "snapshot", Data: snapshot})
	if err != nil {
		log.Printf("server: marshal snapshot: %v", err)
		return
	}

	s.mu.Lock()
	s.snapshot = snapshot
	s.hasSnapshot = true
	s.payload = payload
	s.mu.Unlock()

	s.broadcast(payload)
}

// snapshotErrors returns the errors of the snapshot: the errors of the
// configuration, prefixed with "config: ", and the given load errors.
//
// The caller must hold the mutex.
func (s *Server) snapshotErrors(loadErrs ...string) []string {
	errs := make([]string, 0, len(s.configErrors)+len(loadErrs))
	for _, e := range s.configErrors {
		errs = append(errs, "config: "+e)
	}
	errs = append(errs, loadErrs...)
	if len(errs) == 0 {
		return []string{}
	}
	return errs
}

// broadcast sends the payload to every connected client.
//
// It never blocks: the pending message of a client is replaced by the
// newest one.
func (s *Server) broadcast(payload []byte) {
	s.mu.Lock()
	clients := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()

	for _, c := range clients {
		c.send(payload)
	}
}

// send queues the payload for the client, replacing the pending one.
func (c *client) send(payload []byte) {
	select {
	case c.ch <- payload:
		return
	default:
	}

	// The buffer is full: drop the pending message and queue the newest
	// one instead.
	select {
	case <-c.ch:
	default:
	}
	select {
	case c.ch <- payload:
	default:
	}
}

// Handler returns the HTTP handler of the server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /ws", s.handleWS)
	mux.Handle("GET /", http.FileServerFS(s.index))

	return mux
}

// handleSnapshot serves the last snapshot as JSON.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	snapshot := s.snapshot
	ready := s.hasSnapshot
	s.mu.Unlock()

	if !ready {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		if _, err := w.Write([]byte(`{"error":"not ready"}`)); err != nil {
			log.Printf("server: write snapshot error: %v", err)
		}
		return
	}

	body, err := json.Marshal(snapshot)
	if err != nil {
		log.Printf("server: marshal snapshot: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		if _, err := w.Write([]byte(`{"error":"internal error"}`)); err != nil {
			log.Printf("server: write snapshot error: %v", err)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		log.Printf("server: write snapshot: %v", err)
	}
}

// context returns the context bounding the lifetime of the client
// connections.
//
// The request context cannot be used here: it is canceled as soon as the
// handler returns, which would kill every WebSocket connection right after
// the handshake.
func (s *Server) context() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.baseCtx == nil {
		return context.Background()
	}
	return s.baseCtx
}

// handleWS upgrades the request to a WebSocket connection and streams the
// snapshots to the client.
//
// When a custom origin check is set, it decides alone whether the request
// is accepted: a rejected request gets a 403 response and no connection.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	allowed, custom := s.originAllowed(r)

	var opts *websocket.AcceptOptions
	if custom {
		if !allowed {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			if _, err := w.Write([]byte(`{"error":"forbidden origin"}`)); err != nil {
				log.Printf("server: write forbidden error: %v", err)
			}
			return
		}
		// The origin was already verified by the custom check.
		opts = &websocket.AcceptOptions{InsecureSkipVerify: true}
	}

	conn, err := websocket.Accept(w, r, opts)
	if err != nil {
		log.Printf("server: accept websocket: %v", err)
		return
	}

	ctx := s.context()

	c := &client{
		conn: conn,
		ch:   make(chan []byte, 1),
		done: make(chan struct{}),
	}

	s.addClient(c)

	// Send the current payload right away, if there is one.
	s.mu.Lock()
	payload := s.payload
	s.mu.Unlock()
	if len(payload) > 0 {
		c.send(payload)
	}

	go func() {
		defer s.removeClient(c)
		c.writeLoop(ctx)
	}()
}

// addClient registers a client.
func (s *Server) addClient(c *client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[c] = struct{}{}
}

// removeClient unregisters a client.
func (s *Server) removeClient(c *client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, c)
}

// clientCount returns the number of connected clients.
func (s *Server) clientCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clients)
}

// closeClients closes every client connection.
func (s *Server) closeClients() {
	s.mu.Lock()
	clients := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.clients = map[*client]struct{}{}
	s.mu.Unlock()

	for _, c := range clients {
		c.close()
		// The return value may be discarded explicitly.
		_ = c.conn.CloseNow()
	}
}

// writeLoop writes the queued payloads to the client and pings it
// periodically.
func (c *client) writeLoop(ctx context.Context) {
	defer func() {
		_ = c.conn.CloseNow()
	}()

	// Discard the incoming messages. The read loop also answers the pings
	// and the pongs, so c.Ping keeps working.
	readCtx := c.conn.CloseRead(ctx)

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.done:
			return
		case <-readCtx.Done():
			return
		case <-ctx.Done():
			return
		case payload := <-c.ch:
			writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.conn.Write(writeCtx, websocket.MessageText, payload)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("server: write websocket: %v", err)
				return
			}
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
			err := c.conn.Ping(pingCtx)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("server: ping websocket: %v", err)
				return
			}
		}
	}
}
