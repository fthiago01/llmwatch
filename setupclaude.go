package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// envClaudeConfigDir holds the directory of the configuration of the Claude
// Code. When it is set it replaces the ".claude" directory of the home.
const envClaudeConfigDir = "CLAUDE_CONFIG_DIR"

// backupSuffix is appended to the settings file to name the backup written
// before every change.
const backupSuffix = ".llmwatch-bak"

// statusLineKey is the key of the Claude Code settings holding the status
// line configuration.
const statusLineKey = "statusLine"

// Results returned by claudeSetup.apply.
const (
	setupResultConfigured = "configurado"
	setupResultUpdated    = "atualizado"
	setupResultSame       = "ja configurado"
	setupResultOther      = "outra status line existente"
	setupResultRemoved    = "removido"
	setupResultNothing    = "nada a remover"
)

// claudeSetup holds the options of the setup-claude subcommand.
type claudeSetup struct {
	// SettingsPath is the path of the settings.json of the Claude Code.
	SettingsPath string
	// Command is the command written in the status line configuration.
	Command string
	// Force replaces a status line owned by another program.
	Force bool
	// Remove removes the status line owned by the llmwatch.
	Remove bool
}

// claudeSettingsPath returns the path of the settings.json of the Claude
// Code.
//
// It is the CLAUDE_CONFIG_DIR directory when the variable is set, and the
// ".claude" directory of the home of the user otherwise.
func claudeSettingsPath() (string, error) {
	if dir := os.Getenv(envClaudeConfigDir); dir != "" {
		return filepath.Join(dir, "settings.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("diretório do usuário: %w", err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// claudeCommand returns the command used in the status line.
//
// It is the path of the running program, resolved through the symbolic links,
// followed by the "statusline" argument. On Windows the separators are
// replaced by slashes because the Claude Code runs the command with the Git
// Bash.
func claudeCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("localizar o executável: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if runtime.GOOS == "windows" {
		exe = strings.ReplaceAll(exe, `\`, "/")
	}
	return quoteCommandPath(exe) + " statusline", nil
}

// quoteCommandPath surrounds a path with double quotes when it holds a
// space, so the shell of the Claude Code runs it as a single command.
func quoteCommandPath(path string) string {
	if strings.Contains(path, " ") {
		return `"` + path + `"`
	}
	return path
}

// apply configures the status line of the Claude Code.
//
// It returns a short description of what was done: "configurado",
// "atualizado", "ja configurado", "outra status line existente", "removido"
// or "nada a remover".
func (c claudeSetup) apply() (string, error) {
	if c.SettingsPath == "" {
		return "", fmt.Errorf("caminho das configurações não informado")
	}

	raw, err := os.ReadFile(c.SettingsPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("ler %s: %w", c.SettingsPath, err)
		}
		if c.Remove {
			return setupResultNothing, nil
		}
		if err := c.writeNew(); err != nil {
			return "", err
		}
		return setupResultConfigured, nil
	}

	settings := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return "", fmt.Errorf("%s contém um JSON inválido: %w "+
			"(o arquivo não foi alterado; corrija-o ou remova-o para continuar)",
			c.SettingsPath, err)
	}

	current, has := settings[statusLineKey]
	owned := has && isLlmwatchStatusLine(current)

	if c.Remove {
		if !owned {
			return setupResultNothing, nil
		}
		delete(settings, statusLineKey)
		if err := c.write(settings, raw); err != nil {
			return "", err
		}
		return setupResultRemoved, nil
	}

	switch {
	case !has:
		settings[statusLineKey] = c.statusLineValue()
		if err := c.write(settings, raw); err != nil {
			return "", err
		}
		return setupResultConfigured, nil
	case owned:
		if currentCommand(current) == c.Command {
			return setupResultSame, nil
		}
		settings[statusLineKey] = c.statusLineValue()
		if err := c.write(settings, raw); err != nil {
			return "", err
		}
		return setupResultUpdated, nil
	case c.Force:
		settings[statusLineKey] = c.statusLineValue()
		if err := c.write(settings, raw); err != nil {
			return "", err
		}
		return setupResultConfigured, nil
	default:
		return setupResultOther, nil
	}
}

// statusLineValue returns the JSON value of the status line configuration.
func (c claudeSetup) statusLineValue() json.RawMessage {
	value := map[string]any{
		"type":    "command",
		"command": c.Command,
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		// The map holds only strings, so the encoding cannot fail.
		return json.RawMessage(`{"type":"command"}`)
	}
	return encoded
}

// writeNew creates the settings file with the status line configuration.
func (c claudeSetup) writeNew() error {
	dir := filepath.Dir(c.SettingsPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("criar diretório %s: %w", dir, err)
	}
	settings := map[string]json.RawMessage{
		statusLineKey: c.statusLineValue(),
	}
	data, err := encodeSettings(settings)
	if err != nil {
		return err
	}
	if err := os.WriteFile(c.SettingsPath, data, 0o600); err != nil {
		return fmt.Errorf("gravar %s: %w", c.SettingsPath, err)
	}
	return nil
}

// write saves the settings, keeping a backup of the previous content.
//
// The backup is written before the file is replaced and the new file is
// written in a temporary file of the same directory, renamed over the
// original one.
func (c claudeSetup) write(settings map[string]json.RawMessage, original []byte) error {
	backup := c.SettingsPath + backupSuffix
	if err := os.WriteFile(backup, original, filePerm(c.SettingsPath)); err != nil {
		return fmt.Errorf("gravar backup %s: %w", backup, err)
	}

	data, err := encodeSettings(settings)
	if err != nil {
		return err
	}

	dir := filepath.Dir(c.SettingsPath)
	tmp, err := os.CreateTemp(dir, ".settings-*.json")
	if err != nil {
		return fmt.Errorf("criar arquivo temporário: %w", err)
	}
	name := tmp.Name()
	defer func() {
		_ = os.Remove(name)
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("gravar %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("fechar %s: %w", name, err)
	}
	if err := os.Chmod(name, filePerm(c.SettingsPath)); err != nil {
		return fmt.Errorf("ajustar permissão de %s: %w", name, err)
	}
	if err := os.Rename(name, c.SettingsPath); err != nil {
		return fmt.Errorf("renomear %s: %w", name, err)
	}
	return nil
}

// encodeSettings encodes the settings with an indentation of two spaces and a
// final newline.
func encodeSettings(settings map[string]json.RawMessage) ([]byte, error) {
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("codificar configurações: %w", err)
	}
	return append(data, '\n'), nil
}

// filePerm returns the permission of a file, defaulting to 0600.
func filePerm(path string) os.FileMode {
	info, err := os.Stat(path)
	if err != nil {
		return 0o600
	}
	return info.Mode().Perm()
}

// isLlmwatchStatusLine reports whether the status line configuration was
// written by the llmwatch.
func isLlmwatchStatusLine(raw json.RawMessage) bool {
	var value struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	command := strings.ToLower(value.Command)
	return strings.Contains(command, "llmwatch") &&
		strings.Contains(command, "statusline")
}

// currentCommand returns the command of a status line configuration.
func currentCommand(raw json.RawMessage) string {
	var value struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value.Command
}

// runSetupClaude configures the status line of the Claude Code.
//
// The subcommand is called by the dispatcher of cli.go: an error is printed
// in the standard error and the program exits with status 1.
func runSetupClaude(args []string, stdout io.Writer) error {
	setup := claudeSetup{}

	for _, arg := range args {
		switch arg {
		case "--force", "-f":
			setup.Force = true
		case "--remove":
			setup.Remove = true
		default:
			return fmt.Errorf("opção desconhecida %q "+
				"(use --force ou --remove)", arg)
		}
	}

	path, err := claudeSettingsPath()
	if err != nil {
		return err
	}
	setup.SettingsPath = path

	command, err := claudeCommand()
	if err != nil {
		return err
	}
	setup.Command = command

	result, err := setup.apply()
	if err != nil {
		return err
	}

	switch result {
	case setupResultOther:
		fmt.Fprintf(stdout, "Já existe uma status line de outro programa em %s.\n", path)
		fmt.Fprintln(stdout, "Para substituí-la, use: llmwatch setup-claude --force")
	case setupResultSame:
		fmt.Fprintf(stdout, "Status line do Claude Code já configurada em %s.\n", path)
	case setupResultUpdated:
		fmt.Fprintf(stdout, "Status line do Claude Code atualizada em %s. Reinicie o Claude Code.\n", path)
	case setupResultRemoved:
		fmt.Fprintf(stdout, "Status line do Claude Code removida de %s.\n", path)
	case setupResultNothing:
		fmt.Fprintf(stdout, "Nada a remover em %s.\n", path)
	default:
		fmt.Fprintf(stdout, "Status line do Claude Code configurada em %s. Reinicie o Claude Code.\n", path)
	}
	return nil
}
