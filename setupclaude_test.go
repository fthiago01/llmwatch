package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// settingsOf reads the settings file and decodes it in a map.
func settingsOf(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ler %s: %v", path, err)
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("decodificar %s: %v", path, err)
	}
	return settings
}

// writeSettings writes a settings file with the given content.
func writeSettings(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("criar diretório: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("gravar %s: %v", path, err)
	}
}

// statusLineCommand returns the command of the status line of a settings map.
func statusLineCommand(t *testing.T, settings map[string]any) string {
	t.Helper()
	value, ok := settings[statusLineKey]
	if !ok {
		t.Fatalf("settings sem statusLine: %v", settings)
	}
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("statusLine não é um objeto: %v", value)
	}
	command, _ := object["command"].(string)
	return command
}

func TestClaudeSettingsPathUsesConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envClaudeConfigDir, dir)

	path, err := claudeSettingsPath()
	if err != nil {
		t.Fatalf("claudeSettingsPath: %v", err)
	}
	want := filepath.Join(dir, "settings.json")
	if path != want {
		t.Fatalf("path = %q, quer %q", path, want)
	}
}

func TestClaudeSettingsPathUsesHome(t *testing.T) {
	t.Setenv(envClaudeConfigDir, "")

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	path, err := claudeSettingsPath()
	if err != nil {
		t.Fatalf("claudeSettingsPath: %v", err)
	}
	want := filepath.Join(home, ".claude", "settings.json")
	if path != want {
		t.Fatalf("path = %q, quer %q", path, want)
	}
}

func TestClaudeCommandQuotesPathWithSpace(t *testing.T) {
	command, err := claudeCommand()
	if err != nil {
		t.Fatalf("claudeCommand: %v", err)
	}
	if !strings.HasSuffix(command, " statusline") {
		t.Fatalf("command %q não termina com statusline", command)
	}
	// The path is either free of spaces or surrounded by double quotes.
	path := strings.TrimSuffix(command, " statusline")
	if strings.Contains(path, " ") {
		if !strings.HasPrefix(path, `"`) || !strings.HasSuffix(path, `"`) {
			t.Fatalf("command %q com espaço sem aspas", command)
		}
	}
}

func TestClaudeCommandQuotesSpaces(t *testing.T) {
	// The quoting helper is exercised with a path holding a space.
	path := filepath.Join("C:", "Program Files", "llmwatch", "llmwatch.exe")
	quoted := quoteCommandPath(path)
	if quoted != `"`+path+`"` {
		t.Fatalf("quoted = %q", quoted)
	}
	if got := quoteCommandPath("/usr/bin/llmwatch"); got != "/usr/bin/llmwatch" {
		t.Fatalf("quoted = %q", got)
	}
}

func TestApplyCreatesMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "settings.json")

	setup := claudeSetup{SettingsPath: path, Command: "/usr/bin/llmwatch statusline"}
	result, err := setup.apply()
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result != setupResultConfigured {
		t.Fatalf("result = %q, quer %q", result, setupResultConfigured)
	}

	settings := settingsOf(t, path)
	if got := statusLineCommand(t, settings); got != setup.Command {
		t.Fatalf("command = %q, quer %q", got, setup.Command)
	}
	if settings[statusLineKey].(map[string]any)["type"] != "command" {
		t.Fatalf("type inválido: %v", settings[statusLineKey])
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ler: %v", err)
	}
	if !bytes.HasSuffix(raw, []byte("\n")) {
		t.Fatal("arquivo sem newline final")
	}
	if !bytes.Contains(raw, []byte("\n  \"statusLine\"")) {
		t.Fatalf("sem indentação de dois espaços: %q", raw)
	}
}

func TestApplyPreservesOtherKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	writeSettings(t, path, `{"model":"opus","permissions":{"allow":["x"]}}`)

	setup := claudeSetup{SettingsPath: path, Command: "/usr/bin/llmwatch statusline"}
	result, err := setup.apply()
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result != setupResultConfigured {
		t.Fatalf("result = %q, quer %q", result, setupResultConfigured)
	}

	settings := settingsOf(t, path)
	if settings["model"] != "opus" {
		t.Fatalf("model perdido: %v", settings)
	}
	permissions, ok := settings["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("permissions perdido: %v", settings)
	}
	allow, ok := permissions["allow"].([]any)
	if !ok || len(allow) != 1 || allow[0] != "x" {
		t.Fatalf("permissions.allow perdido: %v", permissions)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	setup := claudeSetup{SettingsPath: path, Command: "/usr/bin/llmwatch statusline"}
	if _, err := setup.apply(); err != nil {
		t.Fatalf("primeira aplicação: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ler: %v", err)
	}
	infoBefore, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	time.Sleep(10 * time.Millisecond)

	result, err := setup.apply()
	if err != nil {
		t.Fatalf("segunda aplicação: %v", err)
	}
	if result != setupResultSame {
		t.Fatalf("result = %q, quer %q", result, setupResultSame)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ler: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("conteúdo mudou:\n%s\n%s", before, after)
	}
	infoAfter, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !infoAfter.ModTime().Equal(infoBefore.ModTime()) {
		t.Fatalf("mtime mudou: %v -> %v", infoBefore.ModTime(), infoAfter.ModTime())
	}
}

func TestApplyUpdatesDifferentCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	first := claudeSetup{SettingsPath: path, Command: "/usr/bin/llmwatch statusline"}
	if _, err := first.apply(); err != nil {
		t.Fatalf("primeira aplicação: %v", err)
	}

	second := claudeSetup{SettingsPath: path, Command: "/opt/llmwatch statusline"}
	result, err := second.apply()
	if err != nil {
		t.Fatalf("segunda aplicação: %v", err)
	}
	if result != setupResultUpdated {
		t.Fatalf("result = %q, quer %q", result, setupResultUpdated)
	}

	settings := settingsOf(t, path)
	if got := statusLineCommand(t, settings); got != second.Command {
		t.Fatalf("command = %q, quer %q", got, second.Command)
	}
}

func TestApplyKeepsOtherStatusLineWithoutForce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	content := `{"statusLine":{"type":"command","command":"other-tool statusline"}}`
	writeSettings(t, path, content)

	setup := claudeSetup{SettingsPath: path, Command: "/usr/bin/llmwatch statusline"}
	result, err := setup.apply()
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result != setupResultOther {
		t.Fatalf("result = %q, quer %q", result, setupResultOther)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ler: %v", err)
	}
	if string(after) != content {
		t.Fatalf("arquivo alterado: %q", after)
	}
	if _, err := os.Stat(path + backupSuffix); err == nil {
		t.Fatal("backup criado sem necessidade")
	}
}

func TestApplyReplacesOtherStatusLineWithForce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	writeSettings(t, path, `{"statusLine":{"type":"command","command":"other-tool statusline"}}`)

	setup := claudeSetup{
		SettingsPath: path,
		Command:      "/usr/bin/llmwatch statusline",
		Force:        true,
	}
	result, err := setup.apply()
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result != setupResultConfigured {
		t.Fatalf("result = %q, quer %q", result, setupResultConfigured)
	}

	settings := settingsOf(t, path)
	if got := statusLineCommand(t, settings); got != setup.Command {
		t.Fatalf("command = %q, quer %q", got, setup.Command)
	}
}

func TestApplyWritesBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := `{"model":"opus"}`
	writeSettings(t, path, original)

	setup := claudeSetup{SettingsPath: path, Command: "/usr/bin/llmwatch statusline"}
	if _, err := setup.apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}

	backup, err := os.ReadFile(path + backupSuffix)
	if err != nil {
		t.Fatalf("ler backup: %v", err)
	}
	if string(backup) != original {
		t.Fatalf("backup = %q, quer %q", backup, original)
	}
}

func TestApplyInvalidJSONKeepsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	content := `{"model": "opus",,}`
	writeSettings(t, path, content)

	setup := claudeSetup{SettingsPath: path, Command: "/usr/bin/llmwatch statusline"}
	result, err := setup.apply()
	if err == nil {
		t.Fatalf("apply sem erro (result %q)", result)
	}
	if !strings.Contains(err.Error(), "JSON inválido") {
		t.Fatalf("erro sem mensagem clara: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ler: %v", err)
	}
	if string(after) != content {
		t.Fatalf("arquivo alterado: %q", after)
	}
	if _, err := os.Stat(path + backupSuffix); err == nil {
		t.Fatal("backup criado com JSON inválido")
	}
}

func TestApplyRemovesLlmwatchStatusLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	writeSettings(t, path, `{"model":"opus"}`)

	setup := claudeSetup{SettingsPath: path, Command: "/usr/bin/llmwatch statusline"}
	if _, err := setup.apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}

	removal := claudeSetup{SettingsPath: path, Remove: true}
	result, err := removal.apply()
	if err != nil {
		t.Fatalf("remoção: %v", err)
	}
	if result != setupResultRemoved {
		t.Fatalf("result = %q, quer %q", result, setupResultRemoved)
	}

	settings := settingsOf(t, path)
	if _, ok := settings[statusLineKey]; ok {
		t.Fatalf("statusLine ainda presente: %v", settings)
	}
	if settings["model"] != "opus" {
		t.Fatalf("model perdido: %v", settings)
	}
}

func TestApplyRemoveOtherStatusLineDoesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	content := `{"statusLine":{"type":"command","command":"other-tool statusline"}}`
	writeSettings(t, path, content)

	removal := claudeSetup{SettingsPath: path, Remove: true}
	result, err := removal.apply()
	if err != nil {
		t.Fatalf("remoção: %v", err)
	}
	if result != setupResultNothing {
		t.Fatalf("result = %q, quer %q", result, setupResultNothing)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ler: %v", err)
	}
	if string(after) != content {
		t.Fatalf("arquivo alterado: %q", after)
	}
}

func TestApplyRemoveMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	removal := claudeSetup{SettingsPath: path, Remove: true}
	result, err := removal.apply()
	if err != nil {
		t.Fatalf("remoção: %v", err)
	}
	if result != setupResultNothing {
		t.Fatalf("result = %q, quer %q", result, setupResultNothing)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("arquivo criado na remoção: %v", err)
	}
}

func TestApplyKeepsPermission(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permissões não são relevantes no Windows")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	writeSettings(t, path, `{"model":"opus"}`)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	setup := claudeSetup{SettingsPath: path, Command: "/usr/bin/llmwatch statusline"}
	if _, err := setup.apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("permissão = %v, quer 0600", info.Mode().Perm())
	}
}

func TestApplyCreatedFilePermission(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permissões não são relevantes no Windows")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	setup := claudeSetup{SettingsPath: path, Command: "/usr/bin/llmwatch statusline"}
	if _, err := setup.apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("permissão = %v, quer 0600", info.Mode().Perm())
	}
}

func TestRunSetupClaudeReportsOtherStatusLine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envClaudeConfigDir, dir)

	path := filepath.Join(dir, "settings.json")
	writeSettings(t, path, `{"statusLine":{"type":"command","command":"other-tool statusline"}}`)

	var stdout bytes.Buffer
	if err := runSetupClaude(nil, &stdout); err != nil {
		t.Fatalf("runSetupClaude: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "--force") {
		t.Fatalf("saída sem sugestão de --force: %q", out)
	}
}

func TestRunSetupClaudeConfigures(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envClaudeConfigDir, dir)

	var stdout bytes.Buffer
	if err := runSetupClaude(nil, &stdout); err != nil {
		t.Fatalf("runSetupClaude: %v", err)
	}
	if !strings.Contains(stdout.String(), "Reinicie o Claude Code") {
		t.Fatalf("saída inesperada: %q", stdout.String())
	}

	path := filepath.Join(dir, "settings.json")
	settings := settingsOf(t, path)
	if got := statusLineCommand(t, settings); !strings.Contains(got, "statusline") {
		t.Fatalf("command = %q", got)
	}
}

func TestRunSetupClaudeRejectsUnknownFlag(t *testing.T) {
	var stdout bytes.Buffer
	if err := runSetupClaude([]string{"--nope"}, &stdout); err == nil {
		t.Fatal("runSetupClaude sem erro")
	}
}
