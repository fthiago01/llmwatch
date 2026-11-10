// Command llmwatch serves a live dashboard of the local LLM usage.
//
// It reads the transcripts of several tools (Claude Code, opencode and Codex)
// and exposes the aggregated usage over HTTP and WebSocket.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"llmwatch/internal/admin"
	"llmwatch/internal/auth"
	"llmwatch/internal/catalog"
	"llmwatch/internal/server"
	"llmwatch/internal/store"
	"llmwatch/internal/update"
	usageinfo "llmwatch/internal/usage"
	"llmwatch/web"
)

// Environment variables understood by the program.
const (
	envAddr       = "LLMWATCH_ADDR"
	envLimits     = "LLMWATCH_LIMITS"
	envTrustProxy = "LLMWATCH_TRUST_PROXY"
	// envUpdateCheck disables the update check when it holds a false
	// value.
	envUpdateCheck = "LLMWATCH_UPDATE_CHECK"
	// envUpdateURL overrides the endpoint of the release API.
	envUpdateURL = "LLMWATCH_UPDATE_URL"
)

// defaultAddr is the address the server listens on when nothing is set.
const defaultAddr = "127.0.0.1:18123"

// refreshInterval is how often the sources are checked for changes.
const refreshInterval = 3 * time.Second

// shutdownTimeout bounds the graceful shutdown of the HTTP server.
const shutdownTimeout = 5 * time.Second

// sessionPurgeInterval is how often the expired sessions are removed.
const sessionPurgeInterval = time.Hour

// updateInterval is how often the release API is queried.
const updateInterval = 6 * time.Hour

// updateInitialDelay is how long the program waits before the first check:
// zero, so every start or restart checks for a new release right away.
const updateInitialDelay = 0

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("llmwatch: ")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(run(ctx, os.Args[1:]))
}

// cmdServe starts the HTTP server and waits for the shutdown signal.
func cmdServe(ctx context.Context) int {
	addr := os.Getenv(envAddr)
	if addr == "" {
		addr = defaultAddr
	}
	trustProxy := truthy(os.Getenv(envTrustProxy))

	limits, err := loadLimits(os.Getenv(envLimits))
	if err != nil {
		log.Fatalf("%s: %v", envLimits, err)
	}

	path, err := dbPath()
	if err != nil {
		log.Fatalf("diretório de dados: %v", err)
	}

	db, err := store.Open(path)
	if err != nil {
		log.Fatalf("abrir banco: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Printf("fechar banco: %v", err)
		}
	}()

	m := auth.New(db, trustProxy)

	pw, created, err := ensureInitialized(ctx, db, m)
	if err != nil {
		log.Fatalf("inicializar: %v", err)
	}
	if created && pw != "" { // senha vinda de LLMWATCH_PASSWORD não gera arquivo
		file, err := initialPasswordPath()
		if err != nil {
			log.Printf("senha inicial: %v", err)
		}
		if pw != "" {
			log.Printf("senha inicial: %s", pw)
		}
		if file != "" {
			log.Printf("senha inicial gravada em %s", file)
		}
	}

	env, err := catalog.DefaultEnv()
	if err != nil {
		log.Fatalf("ambiente: %v", err)
	}
	if err := seedProviders(ctx, db, env); err != nil {
		log.Printf("semear provedores: %v", err)
	}

	srv := server.New(nil, nil, refreshInterval, limits, web.FS)
	srv.SetOriginCheck(m.OriginAllowed)

	// The footer shows the version in use, even when the update check is
	// disabled.
	srv.SetUpdate(usageinfo.UpdateInfo{Current: version})

	// The update check is optional: it can be disabled with
	// LLMWATCH_UPDATE_CHECK=0.
	if updateCheckEnabled(os.Getenv(envUpdateCheck)) {
		checker := update.New(version, os.Getenv(envUpdateURL), nil)
		go checker.Run(ctx, updateInterval, updateInitialDelay, func(info usageinfo.UpdateInfo) {
			if info.Available {
				log.Printf("nova versão %s disponível (atual %s)", info.Latest, info.Current)
			}
			srv.SetUpdate(info)
		})
	}

	// The providers are read from the database and handed to the server.
	// The function is called again after every change made in the admin
	// page.
	reload := func(ctx context.Context) error {
		ps, err := db.ListProviders(ctx)
		if err != nil {
			return fmt.Errorf("listar provedores: %w", err)
		}
		built, errs := catalog.Build(env, ps)
		srv.SetProviders(built.Sources, built.Quotas, errs)
		return nil
	}
	if err := reload(ctx); err != nil {
		log.Printf("carregar provedores: %v", err)
	}

	adm := admin.New(db, env, m, reload)

	root := http.NewServeMux()
	root.Handle("GET /login", servePage(web.FS, "login.html"))
	root.Handle("POST /api/login", adm.Public())

	// The progressive web app assets are public: the browser fetches the
	// manifest, the service worker and the icons without the session cookie.
	root.Handle("GET /manifest.webmanifest", serveAsset("manifest.webmanifest",
		"application/manifest+json", "no-cache"))
	root.Handle("GET /sw.js", serveServiceWorker())
	root.Handle("GET /icons/icon.svg", serveAsset("icon.svg",
		"image/svg+xml", "public, max-age=86400"))
	root.Handle("GET /icons/icon-192.png", serveAsset("icon-192.png",
		"image/png", "public, max-age=86400"))
	root.Handle("GET /icons/icon-512.png", serveAsset("icon-512.png",
		"image/png", "public, max-age=86400"))

	protected := http.NewServeMux()
	protected.Handle("/api/snapshot", srv.Handler())
	protected.Handle("/ws", srv.Handler())
	protected.Handle("/api/", adm.Protected())
	protected.Handle("GET /admin", servePage(web.FS, "admin.html"))
	protected.Handle("GET /password", servePage(web.FS, "password.html"))
	// Only the exact root serves the dashboard.
	protected.Handle("GET /{$}", srv.Handler())

	root.Handle("/", m.Require(protected))

	handler := securityHeaders(root)

	go srv.Run(ctx)
	go purgeSessions(ctx, db)

	if !isLoopbackAddr(addr) && !trustProxy {
		log.Print("aviso: o servidor está exposto fora do loopback sem HTTPS — " +
			"use um proxy HTTPS e LLMWATCH_TRUST_PROXY=1")
	}

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("listening on http://%s", addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		if err != nil {
			log.Fatalf("http server: %v", err)
		}
		return 0
	case <-ctx.Done():
	}

	log.Print("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
	return 0
}

// servePage serves an embedded HTML page.
//
// A missing file yields a plain 404 response.
func servePage(files fs.FS, name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := fs.ReadFile(files, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if _, err := w.Write(data); err != nil {
			log.Printf("write %s: %v", name, err)
		}
	})
}

// serveAsset serves an embedded static asset.
//
// The content type and the cache policy are given by the caller. A missing
// file yields a plain 404 response.
func serveAsset(name, contentType, cacheControl string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := fs.ReadFile(web.FS, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Type", contentType)
		h.Set("Cache-Control", cacheControl)
		if _, err := w.Write(data); err != nil {
			log.Printf("write %s: %v", name, err)
		}
	})
}

// serveServiceWorker serves the service worker script.
//
// The scope header allows the worker to control the whole origin even though
// the script is served from the root path.
func serveServiceWorker() http.Handler {
	next := serveAsset("sw.js", "text/javascript; charset=utf-8", "no-cache")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Service-Worker-Allowed", "/")
		next.ServeHTTP(w, r)
	})
}

// securityHeaders adds the security headers to every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", strings.Join([]string{
			"default-src 'self'",
			"script-src 'self' 'unsafe-inline'",
			"style-src 'self' 'unsafe-inline'",
			"img-src 'self' data:",
			"connect-src 'self'",
			"manifest-src 'self'",
			"worker-src 'self'",
			"frame-ancestors 'none'",
			"base-uri 'none'",
			"form-action 'self'",
		}, "; "))
		next.ServeHTTP(w, r)
	})
}

// purgeSessions removes the expired sessions from the database once an hour.
func purgeSessions(ctx context.Context, db *store.DB) {
	ticker := time.NewTicker(sessionPurgeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := db.PurgeExpiredSessions(ctx, time.Now()); err != nil {
				log.Printf("limpar sessões expiradas: %v", err)
			}
		}
	}
}

// truthy reports whether the value is one of the accepted true spellings.
func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// updateCheckEnabled reports whether the update check must run.
//
// An empty value enables the check. The values "0", "false" and "no"
// disable it.
func updateCheckEnabled(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "1", "true", "yes":
		return true
	default:
		return false
	}
}

// isLoopbackAddr reports whether the address of the server is a loopback
// address.
//
// An empty host is not a loopback address: the server then listens on every
// interface.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// loadLimits parses the optional per provider token limits.
//
// An empty value means no limit at all. An invalid JSON document is a fatal
// configuration error.
func loadLimits(raw string) (map[string]int64, error) {
	if raw == "" {
		return nil, nil
	}
	limits := map[string]int64{}
	if err := json.Unmarshal([]byte(raw), &limits); err != nil {
		return nil, fmt.Errorf("invalid JSON object of per provider limits: %w", err)
	}
	return limits, nil
}
