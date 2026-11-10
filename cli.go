package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"llmwatch/internal/auth"
	"llmwatch/internal/catalog"
	"llmwatch/internal/store"
)

// version is the version of the program, filled at build time with
// -ldflags "-X main.version=...".
var version = "dev"

// Environment variables read by the subcommands.
const (
	// envDataDir overrides the directory holding the database and the
	// initial password file.
	envDataDir = "LLMWATCH_DATA_DIR"
	// envPassword holds the password used on the very first run.
	envPassword = "LLMWATCH_PASSWORD"
)

// Names of the files kept in the data directory.
const (
	dbFileName             = "llmwatch.db"
	initialPasswordFile    = "initial-password.txt"
	providersSeededSetting = "providers_seeded"
)

// maxStatuslineInput bounds the size of the status line payload read from the
// standard input.
const maxStatuslineInput = 1 << 20

// usage is the help text of the program, in Brazilian Portuguese.
const usage = `llmwatch — painel de uso de LLMs locais

Uso:
  llmwatch [comando]

Comandos:
  serve           Inicia o servidor (padrão)
  init            Prepara o banco de dados e a senha inicial
  reset-password  Gera uma nova senha e encerra as sessões
  statusline      Escreve o arquivo de status line do Claude Code
  setup-claude    Configura a status line do Claude Code para o llmwatch
  start           Inicia o serviço do llmwatch
  stop            Para o serviço do llmwatch
  restart         Reinicia o serviço do llmwatch
  status          Mostra o estado do serviço do llmwatch
  version         Mostra a versão do programa
  help            Mostra esta ajuda

Variáveis de ambiente:
  LLMWATCH_DATA_DIR    Diretório de dados (padrão: config dir/llmwatch)
  LLMWATCH_ADDR        Endereço do servidor (padrão: 127.0.0.1:18123)
  LLMWATCH_TRUST_PROXY Confia nos cabeçalhos do proxy (1, true ou yes)
  LLMWATCH_LIMITS      JSON com os limites de tokens por provedor
  LLMWATCH_PASSWORD    Senha usada na primeira inicialização
`

// run dispatches the subcommand named in args and returns the exit code.
func run(ctx context.Context, args []string) int {
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}

	switch cmd {
	case "serve":
		return cmdServe(ctx)
	case "init":
		return cmdInit(ctx)
	case "reset-password":
		return cmdResetPassword(ctx)
	case "statusline":
		return cmdStatusline(ctx)
	case "setup-claude":
		return cmdSetupClaude(ctx)
	case "start":
		return cmdService("start")
	case "stop":
		return cmdService("stop")
	case "restart":
		return cmdService("restart")
	case "status":
		return cmdService("status")
	case "version":
		return cmdVersion(ctx)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "llmwatch: comando desconhecido %q\n\n", cmd)
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
}

// dataDir returns the directory holding the database and the initial password
// file.
//
// It is the value of LLMWATCH_DATA_DIR, when set, or the "llmwatch"
// directory inside the configuration directory of the user.
func dataDir() (string, error) {
	if dir := os.Getenv(envDataDir); dir != "" {
		return dir, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("diretório de configuração: %w", err)
	}
	return filepath.Join(base, "llmwatch"), nil
}

// dbPath returns the path of the database file.
func dbPath() (string, error) {
	dir, err := dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, dbFileName), nil
}

// initialPasswordPath returns the path of the file holding the password
// generated on the first run.
func initialPasswordPath() (string, error) {
	dir, err := dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, initialPasswordFile), nil
}

// ensureInitialized makes sure a password is configured.
//
// It returns the generated password and whether a password was created in
// this call. When the password comes from the LLMWATCH_PASSWORD environment
// variable it is validated against the password policy.
func ensureInitialized(ctx context.Context, db *store.DB, m *auth.Manager) (string, bool, error) {
	initialized, err := m.Initialized(ctx)
	if err != nil {
		return "", false, fmt.Errorf("verificar inicialização: %w", err)
	}
	if initialized {
		return "", false, nil
	}

	if pw := os.Getenv(envPassword); pw != "" {
		if err := auth.ValidatePassword(pw); err != nil {
			return "", false, fmt.Errorf("%s inválida: %w", envPassword, err)
		}
		if err := m.SetPassword(ctx, pw, false); err != nil {
			return "", false, fmt.Errorf("definir senha: %w", err)
		}
		return "", true, nil
	}

	generated, err := auth.GeneratePassword()
	if err != nil {
		return "", false, fmt.Errorf("gerar senha: %w", err)
	}
	if err := m.SetPassword(ctx, generated, true); err != nil {
		return "", false, fmt.Errorf("definir senha: %w", err)
	}
	if err := writeInitialPassword(generated); err != nil {
		return "", false, err
	}
	return generated, true, nil
}

// writeInitialPassword saves the generated password in the data directory.
//
// The file is only readable by its owner and holds the password followed by
// a newline.
func writeInitialPassword(pw string) error {
	path, err := initialPasswordPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("criar diretório de dados: %w", err)
	}
	if err := os.WriteFile(path, []byte(pw+"\n"), 0o600); err != nil {
		return fmt.Errorf("gravar %s: %w", path, err)
	}
	return nil
}

// seedProviders registers the providers detected on the machine.
//
// It does nothing when the providers were already seeded or when the
// database already holds providers. The seeding is marked as done even when
// the detection finds nothing, so it runs only once.
func seedProviders(ctx context.Context, db *store.DB, env catalog.Env) error {
	_, ok, err := db.GetSetting(ctx, providersSeededSetting)
	if err != nil {
		return fmt.Errorf("ler configuração: %w", err)
	}
	if ok {
		return nil
	}

	count, err := db.CountProviders(ctx)
	if err != nil {
		return fmt.Errorf("contar provedores: %w", err)
	}
	if count == 0 {
		for _, p := range catalog.Detect(env, exec.LookPath, os.Getenv) {
			if _, err := db.CreateProvider(ctx, p); err != nil {
				if errors.Is(err, store.ErrDuplicateName) {
					continue
				}
				return fmt.Errorf("criar provedor %s: %w", p.Name, err)
			}
		}
	}

	if err := db.SetSetting(ctx, providersSeededSetting, "1"); err != nil {
		return fmt.Errorf("gravar configuração: %w", err)
	}
	return nil
}

// cmdInit prepares the database, the password and the detected providers.
func cmdInit(ctx context.Context) int {
	path, err := dbPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "llmwatch: %v\n", err)
		return 1
	}

	db, err := store.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "llmwatch: %v\n", err)
		return 1
	}
	defer func() {
		if err := db.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "llmwatch: fechar banco: %v\n", err)
		}
	}()

	m := auth.New(db, false)

	pw, created, err := ensureInitialized(ctx, db, m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "llmwatch: %v\n", err)
		return 1
	}

	env, err := catalog.DefaultEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "llmwatch: %v\n", err)
		return 1
	}
	if err := seedProviders(ctx, db, env); err != nil {
		fmt.Fprintf(os.Stderr, "llmwatch: %v\n", err)
		return 1
	}

	if created {
		if pw != "" {
			fmt.Printf("Senha inicial: %s\n", pw)
			fmt.Println("Troque-a no primeiro acesso.")
		} else {
			fmt.Println("Senha inicial definida a partir de " + envPassword + ".")
			fmt.Println("Troque-a no primeiro acesso.")
		}
		return 0
	}

	fmt.Println("llmwatch já inicializado.")
	return 0
}

// cmdResetPassword generates a new password and drops every session.
func cmdResetPassword(ctx context.Context) int {
	path, err := dbPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "llmwatch: %v\n", err)
		return 1
	}

	db, err := store.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "llmwatch: %v\n", err)
		return 1
	}
	defer func() {
		if err := db.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "llmwatch: fechar banco: %v\n", err)
		}
	}()

	m := auth.New(db, false)

	pw, err := auth.GeneratePassword()
	if err != nil {
		fmt.Fprintf(os.Stderr, "llmwatch: gerar senha: %v\n", err)
		return 1
	}
	if err := m.SetPassword(ctx, pw, true); err != nil {
		fmt.Fprintf(os.Stderr, "llmwatch: definir senha: %v\n", err)
		return 1
	}

	// The old file is replaced by the new password.
	if err := writeInitialPassword(pw); err != nil {
		fmt.Fprintf(os.Stderr, "llmwatch: %v\n", err)
		return 1
	}

	fmt.Printf("Nova senha: %s\n", pw)
	return 0
}

// cmdStatusline reads the status line payload of the Claude Code from the
// standard input, saves it in the cache directory and prints a summary.
//
// It never fails: a broken status line must not break the Claude Code, so
// any error is reported by printing the plain program name.
func cmdStatusline(ctx context.Context) int {
	fallback := func() int {
		fmt.Println("llmwatch")
		return 0
	}

	payload, err := io.ReadAll(io.LimitReader(os.Stdin, maxStatuslineInput))
	if err != nil {
		return fallback()
	}

	var status statuslineInput
	if err := json.Unmarshal(payload, &status); err != nil {
		return fallback()
	}

	if err := saveStatusline(payload); err != nil {
		return fallback()
	}

	fmt.Println(statuslineSummary(status))
	return 0
}

// statuslineInput is the payload written by the Claude Code status line.
type statuslineInput struct {
	Model struct {
		DisplayName string `json:"display_name"`
	} `json:"model"`
	RateLimits struct {
		FiveHour struct {
			UsedPercentage *float64 `json:"used_percentage"`
		} `json:"five_hour"`
		SevenDay struct {
			UsedPercentage *float64 `json:"used_percentage"`
		} `json:"seven_day"`
	} `json:"rate_limits"`
}

// statuslineSummary formats the status line payload as a single line.
//
// The percentages are rounded down and a dash is used when a value is
// missing.
func statuslineSummary(status statuslineInput) string {
	name := status.Model.DisplayName
	if name == "" {
		name = "Claude"
	}
	return fmt.Sprintf("%s · 5h %s · 7d %s",
		name,
		formatPercentage(status.RateLimits.FiveHour.UsedPercentage),
		formatPercentage(status.RateLimits.SevenDay.UsedPercentage),
	)
}

// formatPercentage formats a used percentage as an integer.
//
// The dash is returned when the value is absent.
func formatPercentage(value *float64) string {
	if value == nil {
		return "–"
	}
	return fmt.Sprintf("%d%%", int(*value))
}

// saveStatusline writes the status line payload in the cache directory.
//
// The file is written in a temporary file of the same directory and renamed,
// so a reader never sees a partial document.
func saveStatusline(payload []byte) error {
	dir, err := os.UserCacheDir()
	if err != nil {
		return fmt.Errorf("diretório de cache: %w", err)
	}
	dir = filepath.Join(dir, "llmwatch")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("criar diretório %s: %w", dir, err)
	}

	final := filepath.Join(dir, "claude-statusline.json")
	tmp, err := os.CreateTemp(dir, "claude-statusline-*.json")
	if err != nil {
		return fmt.Errorf("criar arquivo temporário: %w", err)
	}
	name := tmp.Name()
	defer func() {
		// The temporary file is removed when it was not renamed.
		_ = os.Remove(name)
	}()

	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("gravar %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("fechar %s: %w", name, err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return fmt.Errorf("ajustar permissão de %s: %w", name, err)
	}
	if err := os.Rename(name, final); err != nil {
		return fmt.Errorf("renomear %s: %w", name, err)
	}
	return nil
}

// cmdSetupClaude configures the status line of the Claude Code.
func cmdSetupClaude(ctx context.Context) int {
	if err := runSetupClaude(os.Args[2:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "llmwatch: %v\n", err)
		return 1
	}
	return 0
}

// cmdVersion prints the version of the program.
func cmdVersion(ctx context.Context) int {
	fmt.Printf("llmwatch %s\n", version)
	return 0
}

// cmdService runs one of the service subcommands.
//
// The state of the service is always printed on the standard output and
// the errors go to the standard error.
func cmdService(action string) int {
	e := defaultServiceEnv()

	var err error
	switch action {
	case "start":
		err = serviceStart(e, os.Stdout)
	case "stop":
		err = serviceStop(e, os.Stdout)
	case "restart":
		err = serviceRestart(e, os.Stdout)
	case "status":
		err = serviceStatus(e, os.Stdout)
	default:
		err = fmt.Errorf("comando desconhecido %q", action)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "llmwatch: %v\n", err)
		return 1
	}
	return 0
}
