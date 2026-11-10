package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"llmwatch/internal/auth"
	"llmwatch/internal/catalog"
	"llmwatch/internal/store"
)

const testPassword = "abcdefghij1A"

// testEnv returns an Env pointing at temporary directories.
func testEnv(t *testing.T) catalog.Env {
	t.Helper()
	dir := t.TempDir()
	return catalog.Env{
		Home:     filepath.Join(dir, "home"),
		CacheDir: filepath.Join(dir, "cache"),
		GOOS:     "linux",
	}
}

// reloadCounter counts how many times the providers were reloaded.
type reloadCounter struct {
	mu    sync.Mutex
	calls int
	err   error
}

// reload records a reload call.
func (c *reloadCounter) reload(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.err
}

// count returns how many times reload was called.
func (c *reloadCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// fixture holds everything a test needs to talk to the handler.
type fixture struct {
	t        *testing.T
	db       *store.DB
	manager  *auth.Manager
	handler  *Handler
	reloader *reloadCounter
	cookie   *http.Cookie
}

// newFixture creates a handler backed by a temporary database.
func newFixture(t *testing.T) *fixture {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "llmwatch.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	manager := auth.New(db, false)
	if err := manager.SetPassword(context.Background(), testPassword, false); err != nil {
		t.Fatalf("set password: %v", err)
	}

	reloader := &reloadCounter{}
	handler := New(db, testEnv(t), manager, reloader.reload)

	return &fixture{
		t:        t,
		db:       db,
		manager:  manager,
		handler:  handler,
		reloader: reloader,
	}
}

// public returns the handler of the public routes.
func (f *fixture) public() http.Handler {
	return f.handler.Public()
}

// protected returns the handler of the protected routes, wrapped by the
// session requirement of the authentication manager.
func (f *fixture) protected() http.Handler {
	return f.manager.Require(f.handler.Protected())
}

// login authenticates the user and keeps the session cookie.
func (f *fixture) login() {
	f.t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"password":"`+testPassword+`"}`))
	req.Host = "llmwatch.test"
	req.Header.Set("Origin", "http://llmwatch.test")
	rec := httptest.NewRecorder()
	f.public().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		f.t.Fatalf("login: status %d, body %q", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		f.t.Fatal("login: nenhum cookie de sessão")
	}
	f.cookie = cookies[0]
}

// do performs a request against the protected routes.
//
// The session cookie and the origin header are sent when they are known.
func (f *fixture) do(method, path, body string) *httptest.ResponseRecorder {
	f.t.Helper()

	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}

	req := httptest.NewRequest(method, path, reader)
	req.Host = "llmwatch.test"
	if f.cookie != nil {
		req.AddCookie(f.cookie)
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Origin", "http://llmwatch.test")
	}

	rec := httptest.NewRecorder()
	f.protected().ServeHTTP(rec, req)
	return rec
}

// decodeBody reads the JSON body of a response.
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return payload
}

func TestLoginSuccess(t *testing.T) {
	f := newFixture(t)

	req := httptest.NewRequest(http.MethodPost, "/api/login",
		strings.NewReader(`{"password":"`+testPassword+`"}`))
	req.Host = "llmwatch.test"
	req.Header.Set("Origin", "http://llmwatch.test")
	rec := httptest.NewRecorder()
	f.public().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, esperava 204", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != auth.CookieName {
		t.Fatalf("esperava o cookie de sessão, obteve %#v", cookies)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	f := newFixture(t)

	req := httptest.NewRequest(http.MethodPost, "/api/login",
		strings.NewReader(`{"password":"errada-12345"}`))
	req.Host = "llmwatch.test"
	req.Header.Set("Origin", "http://llmwatch.test")
	rec := httptest.NewRecorder()
	f.public().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, esperava 401", rec.Code)
	}
	if got := decodeBody(t, rec)["error"]; got != "senha incorreta" {
		t.Errorf("erro inesperado: %v", got)
	}
}

func TestLoginRateLimited(t *testing.T) {
	f := newFixture(t)

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/login",
			strings.NewReader(`{"password":"errada-12345"}`))
		req.Host = "llmwatch.test"
		req.Header.Set("Origin", "http://llmwatch.test")
		req.RemoteAddr = "192.0.2.10:1234"
		rec := httptest.NewRecorder()
		f.public().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("tentativa %d: status %d, esperava 401", i+1, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/api/login",
		strings.NewReader(`{"password":"`+testPassword+`"}`))
	req.Host = "llmwatch.test"
	req.Header.Set("Origin", "http://llmwatch.test")
	req.RemoteAddr = "192.0.2.10:1234"
	rec := httptest.NewRecorder()
	f.public().ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, esperava 429", rec.Code)
	}
	if got := decodeBody(t, rec)["error"]; got != "muitas tentativas, aguarde 15 minutos" {
		t.Errorf("erro inesperado: %v", got)
	}
}

func TestProtectedWithoutSession(t *testing.T) {
	f := newFixture(t)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/providers", nil)
	req.Host = "llmwatch.test"
	rec := httptest.NewRecorder()
	f.protected().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, esperava 401", rec.Code)
	}
}

func TestCreateProviderMasksSecret(t *testing.T) {
	f := newFixture(t)
	f.login()

	const secret = "chave-secreta-minimax-1234"
	rec := f.do(http.MethodPost, "/api/admin/providers", `{
		"type": "minimax-plan",
		"name": "MiniMax pessoal",
		"enabled": true,
		"config": {"api_key": "`+secret+`"}
	}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, esperava 201: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if strings.Contains(body, secret) {
		t.Fatalf("a resposta contém a chave em claro: %s", body)
	}

	payload := decodeBody(t, rec)
	config, ok := payload["config"].(map[string]any)
	if !ok {
		t.Fatalf("config ausente na resposta: %s", body)
	}
	masked, ok := config["api_key"].(string)
	if !ok {
		t.Fatalf("api_key ausente na resposta: %s", body)
	}
	if !strings.HasPrefix(masked, "••••") || strings.Contains(masked, secret) {
		t.Errorf("api_key deveria estar mascarada, obteve %q", masked)
	}

	// O segredo continua guardado no banco.
	stored, err := f.db.ListProviders(context.Background())
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("esperava 1 provider, obteve %d", len(stored))
	}
	if stored[0].Config["api_key"] != secret {
		t.Errorf("o segredo não foi guardado: %q", stored[0].Config["api_key"])
	}
}

func TestCreateProviderRejectsInvalidInput(t *testing.T) {
	f := newFixture(t)
	f.login()

	tests := []struct {
		name string
		body string
	}{
		{"nome vazio", `{"type":"minimax-plan","name":"  ","config":{"api_key":"x"}}`},
		{"nome longo", `{"type":"minimax-plan","name":"` + strings.Repeat("a", 61) + `","config":{"api_key":"x"}}`},
		{"tipo desconhecido", `{"type":"nao-existe","name":"x","config":{}}`},
		{"campo obrigatório vazio", `{"type":"minimax-plan","name":"x","config":{"api_key":""}}`},
		{"campo desconhecido", `{"type":"minimax-plan","name":"x","config":{"api_key":"k","extra":"1"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(http.MethodPost, "/api/admin/providers", tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, esperava 400: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestCreateProviderDuplicateName(t *testing.T) {
	f := newFixture(t)
	f.login()

	body := `{"type":"minimax-plan","name":"Igual","config":{"api_key":"chave-1234"}}`
	rec := f.do(http.MethodPost, "/api/admin/providers", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, esperava 201: %s", rec.Code, rec.Body.String())
	}

	rec = f.do(http.MethodPost, "/api/admin/providers", body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, esperava 409: %s", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["error"]; got != "já existe um provider com esse nome" {
		t.Errorf("erro inesperado: %v", got)
	}
}

func TestUpdateProviderKeepsSecret(t *testing.T) {
	f := newFixture(t)
	f.login()

	const secret = "chave-secreta-minimax-1234"
	rec := f.do(http.MethodPost, "/api/admin/providers",
		`{"type":"minimax-plan","name":"MiniMax","config":{"api_key":"`+secret+`"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, esperava 201: %s", rec.Code, rec.Body.String())
	}
	id := int(decodeBody(t, rec)["id"].(float64))

	// A chave vazia mantém a antiga.
	rec = f.do(http.MethodPut, "/api/admin/providers/"+itoa(id),
		`{"type":"minimax-plan","name":"MiniMax renomeado","config":{"api_key":""}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, esperava 200: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, secret) {
		t.Fatalf("a resposta contém a chave em claro: %s", body)
	}

	stored, err := f.db.GetProvider(context.Background(), int64(id))
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if stored.Config["api_key"] != secret {
		t.Errorf("o segredo deveria ter sido mantido, obteve %q", stored.Config["api_key"])
	}
	if stored.Name != "MiniMax renomeado" {
		t.Errorf("nome não foi atualizado: %q", stored.Name)
	}
}

func TestUpdateProviderRejectsTypeChange(t *testing.T) {
	f := newFixture(t)
	f.login()

	rec := f.do(http.MethodPost, "/api/admin/providers",
		`{"type":"minimax-plan","name":"MiniMax","config":{"api_key":"chave-1234"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, esperava 201: %s", rec.Code, rec.Body.String())
	}
	id := int(decodeBody(t, rec)["id"].(float64))

	rec = f.do(http.MethodPut, "/api/admin/providers/"+itoa(id),
		`{"type":"claude-code-logs","name":"MiniMax","config":{"path":"/tmp"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, esperava 400: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateAndDeleteNotFound(t *testing.T) {
	f := newFixture(t)
	f.login()

	rec := f.do(http.MethodPut, "/api/admin/providers/4242",
		`{"type":"minimax-plan","name":"X","config":{"api_key":"k"}}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, esperava 404: %s", rec.Code, rec.Body.String())
	}

	rec = f.do(http.MethodDelete, "/api/admin/providers/4242", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, esperava 404: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateProviderInvalidID(t *testing.T) {
	f := newFixture(t)
	f.login()

	rec := f.do(http.MethodPut, "/api/admin/providers/abc",
		`{"type":"minimax-plan","name":"X","config":{"api_key":"k"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, esperava 400: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteProvider(t *testing.T) {
	f := newFixture(t)
	f.login()

	rec := f.do(http.MethodPost, "/api/admin/providers",
		`{"type":"minimax-plan","name":"MiniMax","config":{"api_key":"chave-1234"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, esperava 201: %s", rec.Code, rec.Body.String())
	}
	id := int(decodeBody(t, rec)["id"].(float64))

	rec = f.do(http.MethodDelete, "/api/admin/providers/"+itoa(id), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, esperava 204: %s", rec.Code, rec.Body.String())
	}

	if _, err := f.db.GetProvider(context.Background(), int64(id)); err == nil {
		t.Error("o provider deveria ter sido removido")
	}
}

func TestReloadCalledAfterChanges(t *testing.T) {
	f := newFixture(t)
	f.login()

	rec := f.do(http.MethodPost, "/api/admin/providers",
		`{"type":"minimax-plan","name":"MiniMax","config":{"api_key":"chave-1234"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, esperava 201: %s", rec.Code, rec.Body.String())
	}
	if got := f.reloader.count(); got != 1 {
		t.Errorf("reload chamado %d vezes após o create, esperava 1", got)
	}
	id := int(decodeBody(t, rec)["id"].(float64))

	rec = f.do(http.MethodPut, "/api/admin/providers/"+itoa(id),
		`{"type":"minimax-plan","name":"MiniMax 2","config":{"api_key":"chave-1234"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, esperava 200: %s", rec.Code, rec.Body.String())
	}
	if got := f.reloader.count(); got != 2 {
		t.Errorf("reload chamado %d vezes após o update, esperava 2", got)
	}

	rec = f.do(http.MethodDelete, "/api/admin/providers/"+itoa(id), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, esperava 204: %s", rec.Code, rec.Body.String())
	}
	if got := f.reloader.count(); got != 3 {
		t.Errorf("reload chamado %d vezes após o delete, esperava 3", got)
	}
}

func TestReloadFailureDoesNotFailRequest(t *testing.T) {
	f := newFixture(t)
	f.login()
	f.reloader.err = context.DeadlineExceeded

	rec := f.do(http.MethodPost, "/api/admin/providers",
		`{"type":"minimax-plan","name":"MiniMax","config":{"api_key":"chave-1234"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, esperava 201: %s", rec.Code, rec.Body.String())
	}
}

func TestListProviders(t *testing.T) {
	f := newFixture(t)
	f.login()

	rec := f.do(http.MethodPost, "/api/admin/providers",
		`{"type":"minimax-plan","name":"MiniMax","config":{"api_key":"chave-secreta-1234"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, esperava 201: %s", rec.Code, rec.Body.String())
	}

	rec = f.do(http.MethodGet, "/api/admin/providers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, esperava 200: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "chave-secreta-1234") {
		t.Fatalf("a lista contém o segredo em claro: %s", rec.Body.String())
	}

	var providers []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &providers); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	if len(providers) != 1 {
		t.Fatalf("esperava 1 provider, obteve %d", len(providers))
	}
	for _, key := range []string{"id", "type", "name", "enabled", "config", "created_at", "updated_at"} {
		if _, ok := providers[0][key]; !ok {
			t.Errorf("campo %q ausente na resposta: %s", key, rec.Body.String())
		}
	}
}

func TestTypes(t *testing.T) {
	f := newFixture(t)
	f.login()

	rec := f.do(http.MethodGet, "/api/admin/types", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, esperava 200: %s", rec.Code, rec.Body.String())
	}

	var types []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &types); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	if len(types) != len(catalog.Types(f.handler.env)) {
		t.Fatalf("esperava %d tipos, obteve %d", len(catalog.Types(f.handler.env)), len(types))
	}
}

func TestMe(t *testing.T) {
	f := newFixture(t)
	f.login()

	rec := f.do(http.MethodGet, "/api/me", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, esperava 200: %s", rec.Code, rec.Body.String())
	}
	payload := decodeBody(t, rec)
	mustChange, ok := payload["must_change_password"].(bool)
	if !ok {
		t.Fatalf("campo must_change_password ausente: %s", rec.Body.String())
	}
	if mustChange {
		t.Error("must_change_password deveria ser falso")
	}
}

func TestTestProviderClaudeStatusLineMissingFile(t *testing.T) {
	f := newFixture(t)
	f.login()

	missing := filepath.Join(t.TempDir(), "statusline.json")
	body, err := json.Marshal(map[string]any{
		"type":    "claude-statusline",
		"name":    "Limites",
		"enabled": false,
		"config":  map[string]string{"path": missing},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	rec := f.do(http.MethodPost, "/api/admin/providers", string(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, esperava 201: %s", rec.Code, rec.Body.String())
	}
	id := int(decodeBody(t, rec)["id"].(float64))

	rec = f.do(http.MethodPost, "/api/admin/providers/"+itoa(id)+"/test", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, esperava 200: %s", rec.Code, rec.Body.String())
	}

	payload := decodeBody(t, rec)
	ok, isBool := payload["ok"].(bool)
	if !isBool {
		t.Fatalf("campo ok ausente: %s", rec.Body.String())
	}
	if ok {
		t.Errorf("ok deveria ser falso: %s", rec.Body.String())
	}
	message, _ := payload["message"].(string)
	if message == "" {
		t.Errorf("mensagem vazia: %s", rec.Body.String())
	}
	if _, hasQuota := payload["quota"]; !hasQuota {
		t.Errorf("campo quota ausente: %s", rec.Body.String())
	}
}

func TestTestProviderUnknownID(t *testing.T) {
	f := newFixture(t)
	f.login()

	rec := f.do(http.MethodPost, "/api/admin/providers/999/test", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, esperava 404: %s", rec.Code, rec.Body.String())
	}
}

func TestMutatingRequestWithoutOrigin(t *testing.T) {
	f := newFixture(t)
	f.login()

	req := httptest.NewRequest(http.MethodPost, "/api/admin/providers",
		strings.NewReader(`{"type":"minimax-plan","name":"X","config":{"api_key":"k"}}`))
	req.Host = "llmwatch.test"
	req.AddCookie(f.cookie)
	rec := httptest.NewRecorder()
	f.protected().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, esperava 403: %s", rec.Code, rec.Body.String())
	}
}

func TestPasswordChange(t *testing.T) {
	f := newFixture(t)
	f.login()

	rec := f.do(http.MethodPost, "/api/password",
		`{"current":"`+testPassword+`","next":"fraca"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, esperava 400: %s", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["error"]; got == "" {
		t.Errorf("erro vazio: %s", rec.Body.String())
	}

	rec = f.do(http.MethodPost, "/api/password",
		`{"current":"senha-errada-123","next":"novasenha12345A"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, esperava 400: %s", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["error"]; got != "senha atual incorreta" {
		t.Errorf("erro inesperado: %v", got)
	}

	rec = f.do(http.MethodPost, "/api/password",
		`{"current":"`+testPassword+`","next":"`+testPassword+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, esperava 400: %s", rec.Code, rec.Body.String())
	}

	const newPassword = "novasenha12345A"
	rec = f.do(http.MethodPost, "/api/password",
		`{"current":"`+testPassword+`","next":"`+newPassword+`"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, esperava 204: %s", rec.Code, rec.Body.String())
	}

	// A nova senha vale e a antiga não vale mais.
	req := httptest.NewRequest(http.MethodPost, "/api/login",
		strings.NewReader(`{"password":"`+newPassword+`"}`))
	req.Host = "llmwatch.test"
	req.Header.Set("Origin", "http://llmwatch.test")
	rec = httptest.NewRecorder()
	f.public().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("login com a nova senha: status %d", rec.Code)
	}
}

func TestInvalidJSONBody(t *testing.T) {
	f := newFixture(t)
	f.login()

	rec := f.do(http.MethodPost, "/api/admin/providers", "{não é json}")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, esperava 400: %s", rec.Code, rec.Body.String())
	}
}

func TestBodyTooLarge(t *testing.T) {
	f := newFixture(t)
	f.login()

	big := strings.Repeat("a", maxBodySize+1024)
	rec := f.do(http.MethodPost, "/api/admin/providers", `{"name":"`+big+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, esperava 400: %s", rec.Code, rec.Body.String())
	}
}

// itoa converts an int into a string.
func itoa(v int) string {
	return strconv.Itoa(v)
}
