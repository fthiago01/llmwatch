// Package catalog describes the provider types that can be registered in the
// admin page.
//
// A Type is the schema of a provider configuration: the fields it accepts,
// their kind, their default value and the help text shown to the user. The
// defaults of the path fields depend on the machine, so they are computed
// from an Env.
package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Field kinds accepted by the admin form.
const (
	// KindText is a plain text field.
	KindText = "text"
	// KindSecret is a sensitive field, masked when the configuration is
	// read back.
	KindSecret = "secret"
	// KindPath is a filesystem path field.
	KindPath = "path"
)

// Type kinds accepted by the admin form.
const (
	// KindUsage marks a type backed by a usage source.
	KindUsage = "usage"
	// KindQuota marks a type backed by a quota provider.
	KindQuota = "quota"
)

// Field is one configuration field of a provider type.
type Field struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Kind     string `json:"kind"`
	Default  string `json:"default"`
	Help     string `json:"help"`
	Required bool   `json:"required"`
}

// Type is a registrable provider type.
type Type struct {
	ID          string  `json:"id"`
	Label       string  `json:"label"`
	Kind        string  `json:"kind"`
	Description string  `json:"description"`
	Fields      []Field `json:"fields"`
}

// Env holds the machine-dependent paths used to compute the field defaults.
type Env struct {
	Home     string
	CacheDir string
	GOOS     string
}

// DefaultEnv returns the environment of the current user.
//
// Home is the user home directory and CacheDir the user cache directory.
func DefaultEnv() (Env, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Env{}, fmt.Errorf("home do usuário: %w", err)
	}

	cache, err := os.UserCacheDir()
	if err != nil {
		return Env{}, fmt.Errorf("diretório de cache: %w", err)
	}

	return Env{Home: home, CacheDir: cache, GOOS: os.Getenv("GOOS")}, nil
}

// Type identifiers of the catalog.
const (
	TypeClaudeCodeLogs = "claude-code-logs"
	TypeOpencodeDB     = "opencode-db"
	TypeCodexDB        = "codex-db"
	TypeClaudeStatus   = "claude-statusline"
	TypeMiniMaxPlan    = "minimax-plan"
	TypeCodexAppServer = "codex-app-server"
)

// Types returns every registrable provider type, with the field defaults
// computed from env.
//
// The returned slice is never nil.
func Types(env Env) []Type {
	return []Type{
		{
			ID:    TypeClaudeCodeLogs,
			Label: "Claude Code (logs)",
			Kind:  KindUsage,
			Description: "Lê os transcripts do Claude Code para contabilizar os tokens " +
				"consumidos por modelo.",
			Fields: []Field{
				{
					Key:     "path",
					Label:   "Diretório de transcripts",
					Kind:    KindPath,
					Default: filepath.Join(env.Home, ".claude", "projects"),
					Help:    "Diretório ~/.claude/projects com os transcripts do Claude Code.",
				},
			},
		},
		{
			ID:    TypeOpencodeDB,
			Label: "opencode",
			Kind:  KindUsage,
			Description: "Lê o banco de dados do opencode para contabilizar os tokens " +
				"consumidos por modelo.",
			Fields: []Field{
				{
					Key:     "path",
					Label:   "Banco de dados",
					Kind:    KindPath,
					Default: filepath.Join(env.Home, ".local", "share", "opencode", "opencode.db"),
					Help:    "Arquivo opencode.db; o caminho é o mesmo em todos os sistemas operacionais.",
				},
			},
		},
		{
			ID:    TypeCodexDB,
			Label: "Codex (logs)",
			Kind:  KindUsage,
			Description: "Lê o banco de dados do Codex CLI para contabilizar os tokens " +
				"consumidos por modelo.",
			Fields: []Field{
				{
					Key:     "path",
					Label:   "Banco de dados",
					Kind:    KindPath,
					Default: filepath.Join(env.Home, ".codex", "state_5.sqlite"),
					Help:    "Arquivo state_5.sqlite do Codex CLI.",
				},
			},
		},
		{
			ID:    TypeClaudeStatus,
			Label: "Claude (limites)",
			Kind:  KindQuota,
			Description: "Lê os limites de uso do Claude Code gravados pela status line " +
				"do próprio Claude Code.",
			Fields: []Field{
				{
					Key:     "path",
					Label:   "Arquivo da status line",
					Kind:    KindPath,
					Default: filepath.Join(env.CacheDir, "llmwatch", "claude-statusline.json"),
					Help: "A status line do Claude Code deve rodar \"llmwatch statusline\" para " +
						"gravar os limites neste arquivo.",
				},
			},
		},
		{
			ID:    TypeMiniMaxPlan,
			Label: "MiniMax (limites)",
			Kind:  KindQuota,
			Description: "Consulta a API do M Plan da MiniMax para mostrar os limites " +
				"restantes do plano.",
			Fields: []Field{
				{
					Key:      "api_key",
					Label:    "Chave da API",
					Kind:     KindSecret,
					Required: true,
					Help:     "Chave do M Plan, disponível em Plan Details no painel da MiniMax.",
				},
				{
					Key:     "url",
					Label:   "URL da API",
					Kind:    KindText,
					Default: "https://www.minimax.io/v1/token_plan/remains",
					Help:    "Endpoint que informa os limites restantes do token plan.",
				},
			},
		},
		{
			ID:    TypeCodexAppServer,
			Label: "Codex (limites)",
			Kind:  KindQuota,
			Description: "Consulta o app-server do Codex CLI para mostrar os limites " +
				"da conta.",
			Fields: []Field{
				{
					Key:     "bin",
					Label:   "Executável do codex",
					Kind:    KindPath,
					Default: "codex",
					Help:    "Requer \"codex login\" configurado no Codex CLI.",
				},
			},
		},
	}
}

// Lookup returns the type with the given id.
func Lookup(env Env, id string) (Type, bool) {
	for _, t := range Types(env) {
		if t.ID == id {
			return t, true
		}
	}
	return Type{}, false
}

// Validate checks a configuration against the schema of a type.
//
// It fails when the configuration holds a key unknown to the type, when a
// required field is empty or when a field is set to a value that is not
// accepted by its kind.
func Validate(t Type, cfg map[string]string) error {
	known := make(map[string]Field, len(t.Fields))
	for _, f := range t.Fields {
		known[f.Key] = f
	}

	for key, value := range cfg {
		field, ok := known[key]
		if !ok {
			return fmt.Errorf("campo desconhecido: %q", key)
		}
		if field.Required && strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s é obrigatório", field.Label)
		}
	}

	for _, f := range t.Fields {
		value, ok := cfg[f.Key]
		if !ok {
			continue
		}
		if f.Required && strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s é obrigatório", f.Label)
		}
	}

	return nil
}

// Normalize applies the field defaults to a configuration.
//
// Every value is trimmed and the empty values are replaced by the default of
// their field. The result is a new map; cfg is not modified.
func Normalize(t Type, cfg map[string]string) map[string]string {
	out := make(map[string]string, len(t.Fields))
	for _, f := range t.Fields {
		value := strings.TrimSpace(cfg[f.Key])
		if value == "" {
			value = f.Default
		}
		out[f.Key] = value
	}
	return out
}

// Mask hides the secret fields of a configuration.
//
// A secret field is replaced by a masked value: the empty string when it is
// empty, "••••" when it is too short to reveal anything, and "••••" followed
// by its last four characters otherwise. The other fields are copied as they
// are. The result is a new map; cfg is not modified.
func Mask(t Type, cfg map[string]string) map[string]string {
	out := make(map[string]string, len(cfg))
	for key, value := range cfg {
		out[key] = value
	}

	for _, f := range t.Fields {
		if f.Kind != KindSecret {
			continue
		}
		value := strings.TrimSpace(cfg[f.Key])
		switch {
		case value == "":
			out[f.Key] = ""
		case len(value) <= 8:
			out[f.Key] = "••••"
		default:
			out[f.Key] = "••••" + value[len(value)-4:]
		}
	}

	return out
}

// MergeSecrets combines a stored configuration with an incoming one.
//
// A secret field left empty in incoming keeps the value stored in old, so
// the masked value shown in the admin form does not erase the real secret.
// The result is a new map; neither old nor incoming is modified.
func MergeSecrets(t Type, old, incoming map[string]string) map[string]string {
	out := make(map[string]string, len(incoming))
	for key, value := range incoming {
		out[key] = value
	}

	for _, f := range t.Fields {
		if f.Kind != KindSecret {
			continue
		}
		if strings.TrimSpace(incoming[f.Key]) != "" {
			continue
		}
		if previous, ok := old[f.Key]; ok {
			out[f.Key] = previous
		}
	}

	return out
}
