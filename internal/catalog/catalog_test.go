package catalog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"llmwatch/internal/store"
)

// testEnv returns an Env pointing at temporary directories.
func testEnv(t *testing.T) Env {
	t.Helper()
	dir := t.TempDir()
	return Env{
		Home:     filepath.Join(dir, "home"),
		CacheDir: filepath.Join(dir, "cache"),
		GOOS:     "linux",
	}
}

// typeByID returns the type with the given id.
func typeByID(t *testing.T, env Env, id string) Type {
	t.Helper()
	typ, ok := Lookup(env, id)
	if !ok {
		t.Fatalf("tipo %q não encontrado no catálogo", id)
	}
	return typ
}

// fieldByKey returns the field with the given key.
func fieldByKey(t *testing.T, typ Type, key string) Field {
	t.Helper()
	for _, f := range typ.Fields {
		if f.Key == key {
			return f
		}
	}
	t.Fatalf("campo %q não encontrado no tipo %q", key, typ.ID)
	return Field{}
}

func TestTypes(t *testing.T) {
	env := testEnv(t)
	types := Types(env)

	if len(types) != 6 {
		t.Fatalf("esperava 6 tipos, obteve %d", len(types))
	}

	want := map[string]string{
		TypeClaudeCodeLogs: TypeClaudeCodeLogs,
		TypeOpencodeDB:     TypeOpencodeDB,
		TypeCodexDB:        TypeCodexDB,
		TypeClaudeStatus:   TypeClaudeStatus,
		TypeMiniMaxPlan:    TypeMiniMaxPlan,
		TypeCodexAppServer: TypeCodexAppServer,
	}

	for _, typ := range types {
		if _, ok := want[typ.ID]; !ok {
			t.Errorf("tipo inesperado %q", typ.ID)
		}
		if typ.Label == "" {
			t.Errorf("tipo %q sem label", typ.ID)
		}
		if typ.Description == "" {
			t.Errorf("tipo %q sem descrição", typ.ID)
		}
		if typ.Kind != KindUsage && typ.Kind != KindQuota {
			t.Errorf("tipo %q com kind inválido %q", typ.ID, typ.Kind)
		}
		if len(typ.Fields) == 0 {
			t.Errorf("tipo %q sem campos", typ.ID)
		}
		for _, f := range typ.Fields {
			if f.Label == "" {
				t.Errorf("campo %q do tipo %q sem label", f.Key, typ.ID)
			}
			switch f.Kind {
			case KindText, KindSecret, KindPath:
			default:
				t.Errorf("campo %q do tipo %q com kind inválido %q", f.Key, typ.ID, f.Kind)
			}
		}
	}

	// Defaults dos paths.
	claude := typeByID(t, env, TypeClaudeCodeLogs)
	if got := fieldByKey(t, claude, "path").Default; got != filepath.Join(env.Home, ".claude", "projects") {
		t.Errorf("default do claude-code-logs: %q", got)
	}

	opencode := typeByID(t, env, TypeOpencodeDB)
	wantOpencode := filepath.Join(env.Home, ".local", "share", "opencode", "opencode.db")
	if got := fieldByKey(t, opencode, "path").Default; got != wantOpencode {
		t.Errorf("default do opencode-db: %q", got)
	}

	codex := typeByID(t, env, TypeCodexDB)
	if got := fieldByKey(t, codex, "path").Default; got != filepath.Join(env.Home, ".codex", "state_5.sqlite") {
		t.Errorf("default do codex-db: %q", got)
	}

	status := typeByID(t, env, TypeClaudeStatus)
	wantStatus := filepath.Join(env.CacheDir, "llmwatch", "claude-statusline.json")
	if got := fieldByKey(t, status, "path").Default; got != wantStatus {
		t.Errorf("default do claude-statusline: %q", got)
	}
	if help := fieldByKey(t, status, "path").Help; !strings.Contains(help, "llmwatch statusline") {
		t.Errorf("help do claude-statusline sem menção ao statusline: %q", help)
	}

	minimax := typeByID(t, env, TypeMiniMaxPlan)
	key := fieldByKey(t, minimax, "api_key")
	if key.Kind != KindSecret || !key.Required {
		t.Errorf("api_key deveria ser secret e obrigatório: %+v", key)
	}
	if !strings.Contains(strings.ToLower(key.Help), "plan details") {
		t.Errorf("help do api_key sem menção ao Plan Details: %q", key.Help)
	}
	url := fieldByKey(t, minimax, "url")
	if url.Default != "https://www.minimax.io/v1/token_plan/remains" {
		t.Errorf("default da url do minimax: %q", url.Default)
	}

	appServer := typeByID(t, env, TypeCodexAppServer)
	bin := fieldByKey(t, appServer, "bin")
	if bin.Kind != KindPath || bin.Default != "codex" {
		t.Errorf("campo bin inesperado: %+v", bin)
	}
	if !strings.Contains(bin.Help, "codex login") {
		t.Errorf("help do bin sem menção ao codex login: %q", bin.Help)
	}
}

func TestLookup(t *testing.T) {
	env := testEnv(t)

	if _, ok := Lookup(env, TypeClaudeCodeLogs); !ok {
		t.Error("tipo existente não encontrado")
	}
	if _, ok := Lookup(env, "nao-existe"); ok {
		t.Error("tipo inexistente encontrado")
	}
}

func TestValidate(t *testing.T) {
	env := testEnv(t)
	minimax := typeByID(t, env, TypeMiniMaxPlan)
	claude := typeByID(t, env, TypeClaudeCodeLogs)

	t.Run("chave desconhecida", func(t *testing.T) {
		err := Validate(claude, map[string]string{"path": "/tmp", "extra": "1"})
		if err == nil {
			t.Fatal("esperava erro para chave desconhecida")
		}
		if !strings.Contains(err.Error(), "extra") {
			t.Errorf("erro não cita a chave: %v", err)
		}
	})

	t.Run("required vazio", func(t *testing.T) {
		err := Validate(minimax, map[string]string{"api_key": "   "})
		if err == nil {
			t.Fatal("esperava erro para campo obrigatório vazio")
		}
		label := fieldByKey(t, minimax, "api_key").Label
		if !strings.Contains(err.Error(), label) {
			t.Errorf("erro não cita o label %q: %v", label, err)
		}
	})

	t.Run("ok", func(t *testing.T) {
		if err := Validate(minimax, map[string]string{"api_key": "abc", "url": "https://x"}); err != nil {
			t.Fatalf("esperava configuração válida: %v", err)
		}
		if err := Validate(claude, map[string]string{"path": "/tmp"}); err != nil {
			t.Fatalf("esperava configuração válida: %v", err)
		}
		if err := Validate(claude, nil); err != nil {
			t.Fatalf("esperava configuração vazia válida: %v", err)
		}
	})
}

func TestNormalize(t *testing.T) {
	env := testEnv(t)
	minimax := typeByID(t, env, TypeMiniMaxPlan)
	claude := typeByID(t, env, TypeClaudeCodeLogs)

	out := Normalize(minimax, map[string]string{"api_key": "  abc  "})
	if out["api_key"] != "abc" {
		t.Errorf("api_key não foi aparado: %q", out["api_key"])
	}
	if out["url"] != "https://www.minimax.io/v1/token_plan/remains" {
		t.Errorf("default da url não aplicado: %q", out["url"])
	}

	// Valores vazios recebem o default.
	out = Normalize(claude, map[string]string{"path": "   "})
	if out["path"] != fieldByKey(t, claude, "path").Default {
		t.Errorf("default do path não aplicado: %q", out["path"])
	}

	// O map original não é alterado.
	cfg := map[string]string{"path": "  /tmp  "}
	Normalize(claude, cfg)
	if cfg["path"] != "  /tmp  " {
		t.Errorf("Normalize alterou o map original: %q", cfg["path"])
	}
}

func TestMask(t *testing.T) {
	env := testEnv(t)
	minimax := typeByID(t, env, TypeMiniMaxPlan)
	claude := typeByID(t, env, TypeClaudeCodeLogs)

	out := Mask(minimax, map[string]string{"api_key": "", "url": "https://x"})
	if out["api_key"] != "" {
		t.Errorf("secret vazio deveria ficar vazio: %q", out["api_key"])
	}
	if out["url"] != "https://x" {
		t.Errorf("campo não secret alterado: %q", out["url"])
	}

	out = Mask(minimax, map[string]string{"api_key": "12345678"})
	if out["api_key"] != "••••" {
		t.Errorf("secret curto deveria virar apenas ••••: %q", out["api_key"])
	}

	out = Mask(minimax, map[string]string{"api_key": "abcdefghij"})
	if out["api_key"] != "••••ghij" {
		t.Errorf("secret longo deveria virar ••••ghij: %q", out["api_key"])
	}

	// Campos não secret permanecem intactos.
	out = Mask(claude, map[string]string{"path": "/tmp/xyz"})
	if out["path"] != "/tmp/xyz" {
		t.Errorf("path alterado: %q", out["path"])
	}
}

func TestMergeSecrets(t *testing.T) {
	env := testEnv(t)
	minimax := typeByID(t, env, TypeMiniMaxPlan)

	old := map[string]string{"api_key": "old-secret-value", "url": "https://old"}

	// Secret vazio mantém o valor antigo.
	out := MergeSecrets(minimax, old, map[string]string{"api_key": "", "url": "https://new"})
	if out["api_key"] != "old-secret-value" {
		t.Errorf("secret deveria ter sido preservado: %q", out["api_key"])
	}
	if out["url"] != "https://new" {
		t.Errorf("url deveria ter sido trocada: %q", out["url"])
	}

	// Secret preenchido substitui o valor antigo.
	out = MergeSecrets(minimax, old, map[string]string{"api_key": "new-secret-value"})
	if out["api_key"] != "new-secret-value" {
		t.Errorf("secret deveria ter sido trocado: %q", out["api_key"])
	}

	// Os maps de entrada não são alterados.
	if old["url"] != "https://old" {
		t.Errorf("MergeSecrets alterou o map antigo: %q", old["url"])
	}
}

func TestBuild(t *testing.T) {
	env := testEnv(t)

	t.Run("ignora desabilitado", func(t *testing.T) {
		built, errs := Build(env, []store.Provider{
			{Type: TypeClaudeCodeLogs, Name: "Desligado", Enabled: false, Config: map[string]string{}},
		})
		if len(errs) != 0 {
			t.Errorf("esperava nenhum erro: %v", errs)
		}
		if len(built.Sources) != 0 || len(built.Quotas) != 0 {
			t.Errorf("esperava listas vazias: %+v", built)
		}
		if built.Sources == nil || built.Quotas == nil {
			t.Error("as listas não devem ser nil")
		}
	})

	t.Run("tipo desconhecido", func(t *testing.T) {
		built, errs := Build(env, []store.Provider{
			{Type: "inexistente", Name: "Ruim", Enabled: true},
		})
		if len(errs) != 1 {
			t.Fatalf("esperava 1 erro, obteve %d: %v", len(errs), errs)
		}
		if !strings.HasPrefix(errs[0], "Ruim: ") {
			t.Errorf("erro sem o prefixo do nome: %q", errs[0])
		}
		if len(built.Sources) != 0 || len(built.Quotas) != 0 {
			t.Errorf("esperava listas vazias: %+v", built)
		}
	})

	t.Run("config invalida", func(t *testing.T) {
		built, errs := Build(env, []store.Provider{
			{Type: TypeMiniMaxPlan, Name: "MiniMax", Enabled: true, Config: map[string]string{"api_key": "  "}},
		})
		if len(errs) != 1 {
			t.Fatalf("esperava 1 erro, obteve %d: %v", len(errs), errs)
		}
		if !strings.HasPrefix(errs[0], "MiniMax: ") {
			t.Errorf("erro sem o prefixo do nome: %q", errs[0])
		}
		if len(built.Quotas) != 0 {
			t.Errorf("esperava nenhuma quota: %+v", built.Quotas)
		}
	})

	t.Run("nomes cadastrados", func(t *testing.T) {
		built, errs := Build(env, []store.Provider{
			{
				Type:    TypeClaudeCodeLogs,
				Name:    "Meus logs",
				Enabled: true,
				Config:  map[string]string{"path": filepath.Join(env.Home, ".claude", "projects")},
			},
			{
				Type:    TypeClaudeStatus,
				Name:    "Meus limites",
				Enabled: true,
				Config:  map[string]string{"path": filepath.Join(env.CacheDir, "llmwatch", "claude-statusline.json")},
			},
		})
		if len(errs) != 0 {
			t.Fatalf("esperava nenhum erro: %v", errs)
		}
		if len(built.Sources) != 1 {
			t.Fatalf("esperava 1 source, obteve %d", len(built.Sources))
		}
		if got := built.Sources[0].Name(); got != "Meus logs" {
			t.Errorf("nome da source: %q", got)
		}
		if len(built.Quotas) != 1 {
			t.Fatalf("esperava 1 quota, obteve %d", len(built.Quotas))
		}
		if got := built.Quotas[0].Name(); got != "Meus limites" {
			t.Errorf("nome da quota: %q", got)
		}

		// O arquivo da status line não existe: o status carrega o erro, mas
		// o nome cadastrado é preenchido.
		status := built.Quotas[0].Fetch(context.Background())
		if status.Name != "Meus limites" {
			t.Errorf("status.Name: %q", status.Name)
		}
		if status.Error == "" {
			t.Error("esperava erro no status da status line inexistente")
		}
	})

	t.Run("todos os tipos", func(t *testing.T) {
		ps := []store.Provider{
			{Type: TypeClaudeCodeLogs, Name: "a", Enabled: true, Config: map[string]string{}},
			{Type: TypeOpencodeDB, Name: "b", Enabled: true, Config: map[string]string{}},
			{Type: TypeCodexDB, Name: "c", Enabled: true, Config: map[string]string{}},
			{Type: TypeClaudeStatus, Name: "d", Enabled: true, Config: map[string]string{}},
			{Type: TypeMiniMaxPlan, Name: "e", Enabled: true, Config: map[string]string{"api_key": "k"}},
			{Type: TypeCodexAppServer, Name: "f", Enabled: true, Config: map[string]string{}},
		}
		built, errs := Build(env, ps)
		if len(errs) != 0 {
			t.Fatalf("esperava nenhum erro: %v", errs)
		}
		if len(built.Sources) != 3 {
			t.Errorf("esperava 3 sources, obteve %d", len(built.Sources))
		}
		if len(built.Quotas) != 3 {
			t.Errorf("esperava 3 quotas, obteve %d", len(built.Quotas))
		}
	})
}

func TestDetect(t *testing.T) {
	env := testEnv(t)

	// Cria os diretórios detectados.
	if err := os.MkdirAll(filepath.Join(env.Home, ".claude", "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(env.Home, ".local", "share", "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(env.Home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(env.Home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	// O banco do opencode é um arquivo, não um diretório.
	if err := os.WriteFile(filepath.Join(env.Home, ".local", "share", "opencode", "opencode.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.Home, ".codex", "state_5.sqlite"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	lookPath := func(name string) (string, error) {
		if name == "codex" {
			return "/usr/local/bin/codex", nil
		}
		return "", errors.New("não encontrado")
	}
	getenv := func(name string) string {
		switch name {
		case "LLMWATCH_MINIMAX_KEY":
			return "chave-secreta"
		case "LLMWATCH_MINIMAX_URL":
			return "https://exemplo.test/remains"
		}
		return ""
	}

	got := Detect(env, lookPath, getenv)

	byType := map[string]store.Provider{}
	for _, p := range got {
		if !p.Enabled {
			t.Errorf("provider %q deveria estar habilitado", p.Name)
		}
		if p.Name == "" {
			t.Errorf("provider do tipo %q sem nome", p.Type)
		}
		byType[p.Type] = p
	}

	wantNames := map[string]string{
		TypeClaudeCodeLogs: "Claude Code (logs)",
		TypeOpencodeDB:     "opencode",
		TypeCodexDB:        "Codex (logs)",
		TypeClaudeStatus:   "Claude (limites)",
		TypeCodexAppServer: "Codex (limites)",
		TypeMiniMaxPlan:    "MiniMax (limites)",
	}

	for id, name := range wantNames {
		p, ok := byType[id]
		if !ok {
			t.Errorf("tipo %q não detectado", id)
			continue
		}
		if p.Name != name {
			t.Errorf("nome do tipo %q: %q, esperava %q", id, p.Name, name)
		}
	}

	if p := byType[TypeClaudeCodeLogs]; p.Config["path"] != filepath.Join(env.Home, ".claude", "projects") {
		t.Errorf("path do claude-code-logs: %q", p.Config["path"])
	}
	if p := byType[TypeOpencodeDB]; p.Config["path"] != filepath.Join(env.Home, ".local", "share", "opencode", "opencode.db") {
		t.Errorf("path do opencode-db: %q", p.Config["path"])
	}
	if p := byType[TypeCodexDB]; p.Config["path"] != filepath.Join(env.Home, ".codex", "state_5.sqlite") {
		t.Errorf("path do codex-db: %q", p.Config["path"])
	}
	if p := byType[TypeClaudeStatus]; p.Config["path"] != filepath.Join(env.CacheDir, "llmwatch", "claude-statusline.json") {
		t.Errorf("path do claude-statusline: %q", p.Config["path"])
	}
	if p := byType[TypeCodexAppServer]; p.Config["bin"] != "/usr/local/bin/codex" {
		t.Errorf("bin do codex-app-server: %q", p.Config["bin"])
	}
	if p := byType[TypeMiniMaxPlan]; p.Config["api_key"] != "chave-secreta" {
		t.Errorf("api_key do minimax-plan: %q", p.Config["api_key"])
	}
	if p := byType[TypeMiniMaxPlan]; p.Config["url"] != "https://exemplo.test/remains" {
		t.Errorf("url do minimax-plan: %q", p.Config["url"])
	}
}

func TestDetectNothing(t *testing.T) {
	env := testEnv(t)

	lookPath := func(string) (string, error) { return "", errors.New("não encontrado") }
	getenv := func(string) string { return "" }

	got := Detect(env, lookPath, getenv)
	if len(got) != 0 {
		t.Errorf("esperava nenhuma sugestão, obteve %d: %+v", len(got), got)
	}
}

func TestDetectMiniMaxDefaultURL(t *testing.T) {
	env := testEnv(t)

	lookPath := func(string) (string, error) { return "", errors.New("não encontrado") }
	getenv := func(name string) string {
		if name == "LLMWATCH_MINIMAX_KEY" {
			return "chave"
		}
		return ""
	}

	got := Detect(env, lookPath, getenv)
	if len(got) != 1 {
		t.Fatalf("esperava 1 sugestão, obteve %d: %+v", len(got), got)
	}
	if got[0].Type != TypeMiniMaxPlan {
		t.Errorf("tipo detectado: %q", got[0].Type)
	}
	if got[0].Config["url"] != "https://www.minimax.io/v1/token_plan/remains" {
		t.Errorf("url padrão do minimax: %q", got[0].Config["url"])
	}
}
