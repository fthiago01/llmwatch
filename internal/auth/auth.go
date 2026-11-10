// Package auth provides password authentication and session management.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"golang.org/x/crypto/bcrypt"
)

const (
	// CookieName is the name of the authentication cookie.
	CookieName = "llmwatch_session"
	// SessionTTL is how long an authentication session remains valid.
	SessionTTL = 30 * 24 * time.Hour

	passwordHashSetting   = "password_hash"
	mustChangePasswordKey = "must_change_password"
)

var (
	// ErrWeakPassword indicates that a password does not meet the password policy.
	ErrWeakPassword = errors.New("senha fraca")
	// ErrBadCredentials indicates that the supplied credentials are invalid.
	ErrBadCredentials = errors.New("credenciais inválidas")
	// ErrRateLimited indicates that too many failed login attempts occurred.
	ErrRateLimited = errors.New("muitas tentativas de login")
)

// Store is the persistence required by Manager. store.DB implements this interface.
type Store interface {
	GetSetting(ctx context.Context, key string) (string, bool, error)
	SetSetting(ctx context.Context, key, value string) error
	CreateSession(ctx context.Context, tokenHash string, expiresAt time.Time) error
	SessionValid(ctx context.Context, tokenHash string, now time.Time) (bool, error)
	DeleteSession(ctx context.Context, tokenHash string) error
	DeleteAllSessions(ctx context.Context) error
}

// ValidatePassword checks the password policy. The returned error wraps
// ErrWeakPassword and describes the violated rule in Brazilian Portuguese.
func ValidatePassword(pw string) error {
	if len([]rune(pw)) < 12 {
		return fmt.Errorf("%w: a senha deve ter pelo menos 12 caracteres", ErrWeakPassword)
	}
	var hasLetter, hasDigit bool
	for _, r := range pw {
		if unicode.IsLetter(r) {
			hasLetter = true
		}
		if unicode.IsDigit(r) {
			hasDigit = true
		}
	}
	if !hasLetter {
		return fmt.Errorf("%w: a senha deve conter pelo menos uma letra", ErrWeakPassword)
	}
	if !hasDigit {
		return fmt.Errorf("%w: a senha deve conter pelo menos um dígito", ErrWeakPassword)
	}
	if strings.TrimSpace(pw) != pw || strings.HasPrefix(pw, " ") || strings.HasSuffix(pw, " ") {
		return fmt.Errorf("%w: a senha não pode começar ou terminar com espaço", ErrWeakPassword)
	}
	return nil
}

// GeneratePassword creates a cryptographically random 20-character password.
func GeneratePassword() (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	const digits = "23456789"
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate random password: %w", err)
	}
	pw := make([]byte, len(raw))
	for i, b := range raw {
		pw[i] = alphabet[int(b)%len(alphabet)]
	}
	// Replace positions with guaranteed letter and digit classes.
	pw[0] = alphabet[int(raw[0])%26]
	pw[1] = digits[int(raw[1])%len(digits)]
	return string(pw), nil
}

// Manager manages passwords and authenticated sessions.
type Manager struct {
	store      Store
	trustProxy bool
	limiter    *limiter
}

// New creates an authentication manager.
func New(s Store, trustProxy bool) *Manager {
	return &Manager{store: s, trustProxy: trustProxy, limiter: newLimiter()}
}

// Initialized reports whether a password has been configured.
func (m *Manager) Initialized(ctx context.Context) (bool, error) {
	_, ok, err := m.store.GetSetting(ctx, passwordHashSetting)
	return ok, err
}

// SetPassword stores a password and invalidates all existing sessions.
func (m *Manager) SetPassword(ctx context.Context, pw string, mustChange bool) error {
	if !mustChange {
		if err := ValidatePassword(pw); err != nil {
			return err
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), BcryptCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	if err := m.store.SetSetting(ctx, passwordHashSetting, string(hash)); err != nil {
		return err
	}
	flag := "0"
	if mustChange {
		flag = "1"
	}
	if err := m.store.SetSetting(ctx, mustChangePasswordKey, flag); err != nil {
		return err
	}
	return m.store.DeleteAllSessions(ctx)
}

// MustChange reports whether the authenticated user must change its password.
func (m *Manager) MustChange(ctx context.Context) (bool, error) {
	value, ok, err := m.store.GetSetting(ctx, mustChangePasswordKey)
	if err != nil || !ok {
		return false, err
	}
	return value == "1", nil
}

// Login validates credentials and creates an authenticated session.
func (m *Manager) Login(ctx context.Context, w http.ResponseWriter, r *http.Request, pw string) error {
	ip := m.ClientIP(r)
	if !m.limiter.allow(ip, time.Now()) {
		return ErrRateLimited
	}
	hash, _, err := m.store.GetSetting(ctx, passwordHashSetting)
	if err != nil {
		return err
	}
	if hash == "" || bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) != nil {
		m.limiter.fail(ip, time.Now())
		return ErrBadCredentials
	}
	if err := m.createSession(ctx, w, r); err != nil {
		return err
	}
	m.limiter.reset(ip)
	return nil
}

func (m *Manager) createSession(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("generate session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	if err := m.store.CreateSession(ctx, hashToken(token), time.Now().Add(SessionTTL)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(SessionTTL / time.Second),
		Secure:   m.isHTTPS(r),
	})
	return nil
}

// Logout invalidates the current session and expires its cookie.
func (m *Manager) Logout(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	if cookie, err := r.Cookie(CookieName); err == nil && cookie.Value != "" {
		if err := m.store.DeleteSession(ctx, hashToken(cookie.Value)); err != nil {
			return err
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
		Secure:   m.isHTTPS(r),
	})
	return nil
}

// Authenticated reports whether the request contains a valid session.
func (m *Manager) Authenticated(r *http.Request) bool {
	cookie, err := r.Cookie(CookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	valid, err := m.store.SessionValid(r.Context(), hashToken(cookie.Value), time.Now())
	if err != nil {
		log.Printf("auth: validate session: %v", err)
		return false
	}
	return valid
}

// ChangePassword changes a password and creates a fresh session.
func (m *Manager) ChangePassword(ctx context.Context, w http.ResponseWriter, r *http.Request, current, next string) error {
	hash, _, err := m.store.GetSetting(ctx, passwordHashSetting)
	if err != nil {
		return err
	}
	if hash == "" || bcrypt.CompareHashAndPassword([]byte(hash), []byte(current)) != nil {
		return ErrBadCredentials
	}
	if err := ValidatePassword(next); err != nil {
		return err
	}
	if current == next {
		return fmt.Errorf("a nova senha deve ser diferente da senha atual")
	}
	if err := m.SetPassword(ctx, next, false); err != nil {
		return err
	}
	return m.createSession(ctx, w, r)
}

func (m *Manager) isHTTPS(r *http.Request) bool {
	return r.TLS != nil || (m.trustProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"))
}

// ClientIP returns the client address, honoring trusted proxy headers.
func (m *Manager) ClientIP(r *http.Request) string {
	if m.trustProxy {
		if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
			return real
		}
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			parts := strings.Split(forwarded, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

// Require rejects unauthenticated requests and users that must change password.
func (m *Manager) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.Authenticated(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/ws" {
				writeJSONError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			nextPath := safeNextPath(r.URL.Path)
			target := "/login"
			if nextPath != "" {
				target += "?next=" + url.QueryEscape(nextPath)
			}
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		mustChange, err := m.MustChange(r.Context())
		if err != nil {
			log.Printf("auth: read must-change setting: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		if !mustChange || allowedWhilePasswordChangeRequired(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSONError(w, http.StatusForbidden, "password change required")
			return
		}
		http.Redirect(w, r, "/password", http.StatusSeeOther)
	})
}

func allowedWhilePasswordChangeRequired(path string) bool {
	if path == "/password" || path == "/api/password" || path == "/logout" {
		return true
	}
	return strings.HasPrefix(path, "/assets/") || path == "/assets"
}

// OriginAllowed reports whether the origin of the request comes from the
// same host serving it.
//
// The request is accepted when the Origin header is present and its host
// matches the expected host: the first value of the X-Forwarded-Host header
// when the proxy is trusted and the header is present, the host of the
// request otherwise.
func (m *Manager) OriginAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == m.expectedHost(r)
}

// expectedHost returns the host the origin of the request must match.
func (m *Manager) expectedHost(r *http.Request) string {
	if m.trustProxy {
		if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" {
			return strings.TrimSpace(strings.Split(forwarded, ",")[0])
		}
	}
	return r.Host
}

// SameOrigin rejects state-changing cross-origin requests.
func (m *Manager) SameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if !m.OriginAllowed(r) {
			writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func safeNextPath(path string) string {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return ""
	}
	return path
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"` + message + `"}`))
}

// BcryptCost is the bcrypt work factor; tests may lower it.
var BcryptCost = 12
