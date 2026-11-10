package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"llmwatch/internal/store"
)

func testStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "llmwatch.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name string
		pw   string
		weak bool
	}{
		{"curta", "Ab1def", true},
		{"sem dígito", "abcdefghijkl", true},
		{"sem letra", "123456789012", true},
		{"válida", "abcdefghij1A", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePassword(tt.pw)
			if tt.weak {
				if !errors.Is(err, ErrWeakPassword) {
					t.Fatalf("expected weak password error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected valid password, got %v", err)
			}
		})
	}
}

func TestGeneratePassword(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 50; i++ {
		pw, err := GeneratePassword()
		if err != nil {
			t.Fatalf("generate password: %v", err)
		}
		if err := ValidatePassword(pw); err != nil {
			t.Fatalf("generated password is invalid: %v", err)
		}
		if seen[pw] {
			t.Fatalf("duplicate generated password %q", pw)
		}
		seen[pw] = true
	}
}

func TestLoginLogout(t *testing.T) {
	db := testStore(t)
	m := New(db, false)
	ctx := context.Background()
	const password = "abcdefghij1A"
	if err := m.SetPassword(ctx, password, false); err != nil {
		t.Fatalf("set password: %v", err)
	}

	for _, secure := range []bool{false, true} {
		t.Run("secure="+map[bool]string{true: "true", false: "false"}[secure], func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if secure {
				req.Header.Set("X-Forwarded-Proto", "https")
			}
			rec := httptest.NewRecorder()
			manager := New(db, secure)
			if err := manager.Login(ctx, rec, req, password); err != nil {
				t.Fatalf("login: %v", err)
			}
			cookies := rec.Result().Cookies()
			if len(cookies) != 1 || !cookies[0].HttpOnly {
				t.Fatalf("expected HttpOnly session cookie, got %#v", cookies)
			}
			if cookies[0].Secure != secure {
				t.Fatalf("expected Secure=%v, got %v", secure, cookies[0].Secure)
			}
			req.AddCookie(cookies[0])
			if !manager.Authenticated(req) {
				t.Fatal("expected authenticated request")
			}
			rec = httptest.NewRecorder()
			if err := manager.Logout(ctx, rec, req); err != nil {
				t.Fatalf("logout: %v", err)
			}
			if manager.Authenticated(req) {
				t.Fatal("expected unauthenticated request after logout")
			}
		})
	}
}

func TestLoginErrorsAndRateLimit(t *testing.T) {
	db := testStore(t)
	m := New(db, false)
	ctx := context.Background()
	if err := m.SetPassword(ctx, "abcdefghij1A", false); err != nil {
		t.Fatalf("set password: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	for i := 0; i < 5; i++ {
		err := m.Login(ctx, httptest.NewRecorder(), req, "wrong-password")
		if !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("attempt %d: expected bad credentials, got %v", i+1, err)
		}
	}
	if err := m.Login(ctx, httptest.NewRecorder(), req, "abcdefghij1A"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected rate limit, got %v", err)
	}
	other := httptest.NewRequest(http.MethodGet, "/", nil)
	other.RemoteAddr = "192.0.2.2:1234"
	if err := m.Login(ctx, httptest.NewRecorder(), other, "abcdefghij1A"); err != nil {
		t.Fatalf("other IP affected by rate limit: %v", err)
	}
}

func TestClientIPUsesLastForwardedAddress(t *testing.T) {
	m := New(testStore(t), true)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "1.1.1.1, 9.9.9.9")
	if got := m.ClientIP(req); got != "9.9.9.9" {
		t.Fatalf("expected last forwarded IP, got %q", got)
	}
	req.Header.Set("X-Real-IP", "8.8.8.8")
	if got := m.ClientIP(req); got != "8.8.8.8" {
		t.Fatalf("expected X-Real-IP to take precedence, got %q", got)
	}
}

func TestRateLimitCannotBeBypassedWithFirstForwardedIP(t *testing.T) {
	db := testStore(t)
	m := New(db, true)
	ctx := context.Background()
	if err := m.SetPassword(ctx, "abcdefghij1A", false); err != nil {
		t.Fatalf("set password: %v", err)
	}
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Forwarded-For", "1.1.1.1, 9.9.9.9")
		if err := m.Login(ctx, httptest.NewRecorder(), req, "wrong-password"); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("attempt %d: expected bad credentials, got %v", i+1, err)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "2.2.2.2, 9.9.9.9")
	if err := m.Login(ctx, httptest.NewRecorder(), req, "abcdefghij1A"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected rate limit to survive changed forwarded prefix, got %v", err)
	}
}

func TestChangePassword(t *testing.T) {
	db := testStore(t)
	m := New(db, false)
	ctx := context.Background()
	const oldPassword = "abcdefghij1A"
	const newPassword = "newpassword12B"
	if err := m.SetPassword(ctx, oldPassword, false); err != nil {
		t.Fatalf("set password: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if err := m.Login(ctx, rec, req, oldPassword); err != nil {
		t.Fatalf("login: %v", err)
	}
	oldCookie := rec.Result().Cookies()[0]
	oldReq := httptest.NewRequest(http.MethodGet, "/", nil)
	oldReq.AddCookie(oldCookie)

	if err := m.ChangePassword(ctx, httptest.NewRecorder(), oldReq, "wrong-password", newPassword); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("expected bad credentials, got %v", err)
	}
	if err := m.ChangePassword(ctx, httptest.NewRecorder(), oldReq, oldPassword, "weak"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("expected weak password, got %v", err)
	}
	if err := m.ChangePassword(ctx, httptest.NewRecorder(), oldReq, oldPassword, oldPassword); err == nil {
		t.Fatal("expected same-password error")
	}
	rec = httptest.NewRecorder()
	if err := m.ChangePassword(ctx, rec, oldReq, oldPassword, newPassword); err != nil {
		t.Fatalf("change password: %v", err)
	}
	if m.Authenticated(oldReq) {
		t.Fatal("old session should be invalidated")
	}
	newReq := httptest.NewRequest(http.MethodGet, "/", nil)
	newReq.AddCookie(rec.Result().Cookies()[0])
	if !m.Authenticated(newReq) {
		t.Fatal("new session should be valid")
	}
}

func TestRequire(t *testing.T) {
	db := testStore(t)
	m := New(db, false)
	ctx := context.Background()
	handler := m.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin", nil))
	if rec.Code != http.StatusSeeOther || !strings.HasSuffix(rec.Header().Get("Location"), "next=%2Fadmin") {
		t.Fatalf("expected login redirect, got %d %q", rec.Code, rec.Header().Get("Location"))
	}

	if err := m.SetPassword(ctx, "abcdefghij1A", true); err != nil {
		t.Fatalf("set password: %v", err)
	}
	rec = httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodGet, "/", nil)
	if err := m.Login(ctx, rec, loginReq, "abcdefghij1A"); err != nil {
		t.Fatalf("login: %v", err)
	}
	cookieReq := httptest.NewRequest(http.MethodGet, "/admin", nil)
	cookieReq.AddCookie(rec.Result().Cookies()[0])
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, cookieReq)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/password" {
		t.Fatalf("expected password redirect, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = httptest.NewRecorder()
	apiReq := httptest.NewRequest(http.MethodGet, "/api/providers", nil)
	apiReq.AddCookie(cookieReq.Cookies()[0])
	handler.ServeHTTP(rec, apiReq)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestSameOriginForwardedHost(t *testing.T) {
	m := New(testStore(t), true)
	handler := m.SameOrigin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	post := func(origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Host = "127.0.0.1:18123"
		req.Header.Set("X-Forwarded-Host", "watch.exemplo.com")
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := post("https://watch.exemplo.com"); rec.Code != http.StatusOK {
		t.Fatalf("expected forwarded origin to pass, got %d", rec.Code)
	}
	if rec := post("https://evil.com"); rec.Code != http.StatusForbidden {
		t.Fatalf("expected different origin to fail, got %d", rec.Code)
	}
}

func TestSameOriginForwardedHostIgnoredWithoutTrustProxy(t *testing.T) {
	m := New(testStore(t), false)
	handler := m.SameOrigin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Host = "127.0.0.1:18123"
	req.Header.Set("X-Forwarded-Host", "watch.exemplo.com")
	req.Header.Set("Origin", "https://watch.exemplo.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected forwarded host to be ignored, got %d", rec.Code)
	}
}

func TestSameOrigin(t *testing.T) {
	m := New(testStore(t), false)
	handler := m.SameOrigin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	post := func(origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Host = "example.com"
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := post(""); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without Origin, got %d", rec.Code)
	}
	if rec := post("https://evil.com"); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for different origin, got %d", rec.Code)
	}
	if rec := post("https://example.com"); rec.Code != http.StatusOK {
		t.Fatalf("expected same origin to pass, got %d", rec.Code)
	}
}

func TestSafeNextPath(t *testing.T) {
	if got := safeNextPath("//evil.com"); got != "" {
		t.Fatalf("expected unsafe path to be discarded, got %q", got)
	}
}
