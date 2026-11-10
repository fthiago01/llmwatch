// Package admin exposes the administration API of llmwatch.
//
// The package serves two groups of routes: the public ones, used to log in,
// and the protected ones, used to manage the providers. The protected routes
// are wrapped by the same origin check, so a cross origin request never
// changes the configuration.
//
// No route ever answers with a secret in clear: the configuration of a
// provider is masked with catalog.Mask before it is serialized.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llmwatch/internal/auth"
	"llmwatch/internal/catalog"
	"llmwatch/internal/store"
)

const (
	// maxBodySize bounds the size of the body of a request.
	maxBodySize = 64 << 10

	// maxNameLen is the maximum length of the name of a provider, in
	// runes.
	maxNameLen = 60

	// testTimeout bounds the test of a single provider.
	testTimeout = 30 * time.Second
)

// Handler serves the administration API.
type Handler struct {
	db     *store.DB
	env    catalog.Env
	auth   *auth.Manager
	reload func(ctx context.Context) error
}

// New creates an administration handler.
//
// The reload function is called after every change of the provider list, so
// the running server picks the new configuration up. It may be nil, in which
// case no reload is attempted.
func New(db *store.DB, env catalog.Env, m *auth.Manager, reload func(ctx context.Context) error) *Handler {
	return &Handler{
		db:     db,
		env:    env,
		auth:   m,
		reload: reload,
	}
}

// Public returns the handler of the routes that do not need a session.
//
// The routes are wrapped by the same origin check of the authentication
// manager, so a login can only be attempted from the page itself.
func (h *Handler) Public() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", h.handleLogin)
	return h.auth.SameOrigin(mux)
}

// Protected returns the handler of the routes that need a session.
//
// The caller is expected to wrap the returned handler with the Require
// method of the authentication manager. The same origin check is applied
// here, so the routes that change the configuration are protected even when
// the caller forgets to do it.
func (h *Handler) Protected() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/logout", h.handleLogout)
	mux.HandleFunc("GET /api/me", h.handleMe)
	mux.HandleFunc("POST /api/password", h.handlePassword)
	mux.HandleFunc("GET /api/admin/types", h.handleTypes)
	mux.HandleFunc("GET /api/admin/providers", h.handleListProviders)
	mux.HandleFunc("POST /api/admin/providers", h.handleCreateProvider)
	mux.HandleFunc("PUT /api/admin/providers/{id}", h.handleUpdateProvider)
	mux.HandleFunc("DELETE /api/admin/providers/{id}", h.handleDeleteProvider)
	mux.HandleFunc("POST /api/admin/providers/{id}/test", h.handleTestProvider)
	return h.auth.SameOrigin(mux)
}

// loginRequest is the body of a login request.
type loginRequest struct {
	Password string `json:"password"`
}

// handleLogin authenticates the user and starts a session.
func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body loginRequest
	if !decodeJSON(w, r, &body) {
		return
	}

	err := h.auth.Login(r.Context(), w, r, body.Password)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, auth.ErrBadCredentials):
		writeError(w, http.StatusUnauthorized, "senha incorreta")
	case errors.Is(err, auth.ErrRateLimited):
		writeError(w, http.StatusTooManyRequests, "muitas tentativas, aguarde 15 minutos")
	default:
		log.Printf("admin: login: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// handleLogout ends the current session.
func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := h.auth.Logout(r.Context(), w, r); err != nil {
		log.Printf("admin: logout: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// meResponse describes the state of the authenticated user.
type meResponse struct {
	MustChangePassword bool `json:"must_change_password"`
}

// handleMe reports whether the user must change the password.
func (h *Handler) handleMe(w http.ResponseWriter, r *http.Request) {
	mustChange, err := h.auth.MustChange(r.Context())
	if err != nil {
		log.Printf("admin: read must-change setting: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, meResponse{MustChangePassword: mustChange})
}

// passwordRequest is the body of a password change request.
type passwordRequest struct {
	Current string `json:"current"`
	Next    string `json:"next"`
}

// handlePassword changes the password of the authenticated user.
func (h *Handler) handlePassword(w http.ResponseWriter, r *http.Request) {
	var body passwordRequest
	if !decodeJSON(w, r, &body) {
		return
	}

	if body.Current == body.Next {
		writeError(w, http.StatusBadRequest, "a nova senha deve ser diferente da senha atual")
		return
	}

	err := h.auth.ChangePassword(r.Context(), w, r, body.Current, body.Next)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, auth.ErrBadCredentials):
		writeError(w, http.StatusBadRequest, "senha atual incorreta")
	case errors.Is(err, auth.ErrWeakPassword):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		log.Printf("admin: change password: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// handleTypes describes the provider types that can be registered.
func (h *Handler) handleTypes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, catalog.Types(h.env))
}

// providerView is the representation of a provider sent to the client.
//
// The configuration is always masked: the secrets are never sent in clear.
type providerView struct {
	ID        int64             `json:"id"`
	Type      string            `json:"type"`
	Name      string            `json:"name"`
	Enabled   bool              `json:"enabled"`
	Config    map[string]string `json:"config"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// providerRequest is the body of a provider creation or update request.
type providerRequest struct {
	Type    string            `json:"type"`
	Name    string            `json:"name"`
	Enabled bool              `json:"enabled"`
	Config  map[string]string `json:"config"`
}

// handleListProviders lists the registered providers.
func (h *Handler) handleListProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := h.db.ListProviders(r.Context())
	if err != nil {
		log.Printf("admin: list providers: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	views := make([]providerView, 0, len(providers))
	for _, p := range providers {
		views = append(views, h.view(p))
	}
	writeJSON(w, http.StatusOK, views)
}

// handleCreateProvider registers a new provider.
func (h *Handler) handleCreateProvider(w http.ResponseWriter, r *http.Request) {
	var body providerRequest
	if !decodeJSON(w, r, &body) {
		return
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "o nome do provider é obrigatório")
		return
	}
	if len([]rune(name)) > maxNameLen {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("o nome do provider deve ter no máximo %d caracteres", maxNameLen))
		return
	}

	typ, ok := catalog.Lookup(h.env, body.Type)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("tipo desconhecido: %q", body.Type))
		return
	}

	if err := catalog.Validate(typ, body.Config); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	provider, err := h.db.CreateProvider(r.Context(), store.Provider{
		Type:    typ.ID,
		Name:    name,
		Enabled: body.Enabled,
		Config:  catalog.Normalize(typ, body.Config),
	})
	if err != nil {
		if errors.Is(err, store.ErrDuplicateName) {
			writeError(w, http.StatusConflict, "já existe um provider com esse nome")
			return
		}
		log.Printf("admin: create provider: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	h.reloadProviders(r.Context())
	writeJSON(w, http.StatusCreated, h.view(provider))
}

// handleUpdateProvider changes the fields of a registered provider.
func (h *Handler) handleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := providerID(w, r)
	if !ok {
		return
	}

	var body providerRequest
	if !decodeJSON(w, r, &body) {
		return
	}

	current, err := h.db.GetProvider(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "provider não encontrado")
			return
		}
		log.Printf("admin: get provider %d: %v", id, err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	typ, ok := catalog.Lookup(h.env, current.Type)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if body.Type != "" && body.Type != current.Type {
		writeError(w, http.StatusBadRequest, "o tipo do provider não pode ser alterado")
		return
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "o nome do provider é obrigatório")
		return
	}
	if len([]rune(name)) > maxNameLen {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("o nome do provider deve ter no máximo %d caracteres", maxNameLen))
		return
	}

	// A secret left empty in the request keeps the stored value, so the
	// masked value shown by the form does not erase the real secret.
	config := catalog.MergeSecrets(typ, current.Config, body.Config)
	if err := catalog.Validate(typ, config); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	provider, err := h.db.UpdateProvider(r.Context(), store.Provider{
		ID:      current.ID,
		Type:    current.Type,
		Name:    name,
		Enabled: body.Enabled,
		Config:  catalog.Normalize(typ, config),
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "provider não encontrado")
		case errors.Is(err, store.ErrDuplicateName):
			writeError(w, http.StatusConflict, "já existe um provider com esse nome")
		default:
			log.Printf("admin: update provider %d: %v", id, err)
			writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	h.reloadProviders(r.Context())
	writeJSON(w, http.StatusOK, h.view(provider))
}

// handleDeleteProvider removes a registered provider.
func (h *Handler) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := providerID(w, r)
	if !ok {
		return
	}

	if err := h.db.DeleteProvider(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "provider não encontrado")
			return
		}
		log.Printf("admin: delete provider %d: %v", id, err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	h.reloadProviders(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// testResponse is the answer of a provider test.
type testResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Quota   any    `json:"quota,omitempty"`
}

// handleTestProvider builds a single provider and reports whether it works.
//
// The provider is built even when it is disabled, so a disabled provider can
// be tested before it is enabled.
func (h *Handler) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := providerID(w, r)
	if !ok {
		return
	}

	provider, err := h.db.GetProvider(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "provider não encontrado")
			return
		}
		log.Printf("admin: get provider %d: %v", id, err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	typ, ok := catalog.Lookup(h.env, provider.Type)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("tipo desconhecido: %q", provider.Type))
		return
	}

	// The provider is copied with the enabled flag set, so the test does
	// not depend on the current state of the provider.
	candidate := store.Provider{
		ID:      provider.ID,
		Type:    provider.Type,
		Name:    provider.Name,
		Enabled: true,
		Config:  catalog.Normalize(typ, provider.Config),
	}

	built, errs := catalog.Build(h.env, []store.Provider{candidate})
	if len(errs) > 0 {
		writeJSON(w, http.StatusOK, testResponse{OK: false, Message: errs[0]})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), testTimeout)
	defer cancel()

	switch typ.Kind {
	case catalog.KindQuota:
		if len(built.Quotas) == 0 {
			writeJSON(w, http.StatusOK, testResponse{OK: false, Message: "provider de limites inválido"})
			return
		}
		status := built.Quotas[0].Fetch(ctx)
		message := status.Error
		if message == "" {
			message = fmt.Sprintf("%d janelas", len(status.Windows))
		}
		writeJSON(w, http.StatusOK, testResponse{OK: status.Error == "", Message: message, Quota: status})
	case catalog.KindUsage:
		if len(built.Sources) == 0 {
			writeJSON(w, http.StatusOK, testResponse{OK: false, Message: "provider de uso inválido"})
			return
		}
		records, err := built.Sources[0].Load(ctx)
		if err != nil {
			writeJSON(w, http.StatusOK, testResponse{OK: false, Message: err.Error()})
			return
		}
		var tokens int64
		for _, record := range records {
			tokens += record.Input + record.Output + record.CacheRead + record.CacheWrite + record.Reasoning
		}
		message := fmt.Sprintf("%d registros, %d tokens", len(records), tokens)
		writeJSON(w, http.StatusOK, testResponse{OK: true, Message: message})
	default:
		writeJSON(w, http.StatusOK, testResponse{OK: false, Message: "tipo desconhecido"})
	}
}

// view builds the representation of a provider sent to the client.
//
// The configuration is masked, so the secrets are never sent in clear.
func (h *Handler) view(p store.Provider) providerView {
	typ, ok := catalog.Lookup(h.env, p.Type)
	if !ok {
		return providerView{
			ID:        p.ID,
			Type:      p.Type,
			Name:      p.Name,
			Enabled:   p.Enabled,
			Config:    map[string]string{},
			CreatedAt: p.CreatedAt,
			UpdatedAt: p.UpdatedAt,
		}
	}
	return providerView{
		ID:        p.ID,
		Type:      p.Type,
		Name:      p.Name,
		Enabled:   p.Enabled,
		Config:    catalog.Mask(typ, p.Config),
		CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt,
	}
}

// reloadProviders asks the application to reload the provider list.
//
// A failing reload does not fail the request: the data was stored, so the
// answer is sent anyway and the error is only reported in the log.
func (h *Handler) reloadProviders(ctx context.Context) {
	if h.reload == nil {
		return
	}
	if err := h.reload(ctx); err != nil {
		log.Printf("admin: reload providers: %v", err)
	}
}

// providerID reads the id of the provider from the path of the request.
func providerID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "id de provider inválido")
		return 0, false
	}
	return id, true
}

// decodeJSON reads the JSON body of a request into target.
//
// It answers with a 400 when the body is not valid JSON and returns false
// when the request must not be served any further.
func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)

	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "JSON inválido")
		return false
	}
	return true
}

// writeJSON answers with the JSON encoding of payload.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("admin: marshal response: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		log.Printf("admin: write response: %v", err)
	}
}

// writeError answers with an error message.
func writeError(w http.ResponseWriter, status int, message string) {
	body, err := json.Marshal(map[string]string{"error": message})
	if err != nil {
		body = []byte(`{"error":"internal error"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		log.Printf("admin: write error response: %v", err)
	}
}
