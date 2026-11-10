package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// statuslineLogPath returns the path of the log of the status line inside
// the given cache directory.
func statuslineLogPath(dir string) string {
	return filepath.Join(dir, "llmwatch", statuslineLogName)
}

// readStatuslineLog returns the lines of the log of the status line.
func readStatuslineLog(t *testing.T, dir string) []string {
	t.Helper()

	data, err := os.ReadFile(statuslineLogPath(dir))
	if err != nil {
		t.Fatalf("ler log: %v", err)
	}
	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// TestAppendStatuslineLog checks the line written for a call of the status
// line.
func TestAppendStatuslineLog(t *testing.T) {
	dir := t.TempDir()
	payload := []byte(`{"model":{"id":"claude-opus-5-5","display_name":"Opus"},` +
		`"version":"2.1.0",` +
		`"rate_limits":{"five_hour":{"used_percentage":16},` +
		`"seven_day":{"used_percentage":2}}}`)

	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := appendStatuslineLog(dir, now, payload, nil); err != nil {
		t.Fatalf("appendStatuslineLog: %v", err)
	}

	lines := readStatuslineLog(t, dir)
	if len(lines) != 1 {
		t.Fatalf("linhas = %d, quer 1", len(lines))
	}
	line := lines[0]

	if !strings.Contains(line, "rate_limits=five_hour,seven_day") {
		t.Fatalf("linha = %q, quer rate_limits=five_hour,seven_day", line)
	}
	if !strings.Contains(line, "version=2.1.0") {
		t.Fatalf("linha = %q, quer version=2.1.0", line)
	}
	if !strings.Contains(line, "model=claude-opus-5-5") {
		t.Fatalf("linha = %q, quer model=claude-opus-5-5", line)
	}
	if !strings.Contains(line, "erro=-") {
		t.Fatalf("linha = %q, quer erro=-", line)
	}
	if !strings.Contains(line, fmt.Sprintf("bytes=%d", len(payload))) {
		t.Fatalf("linha = %q, quer bytes=%d", line, len(payload))
	}
	if !strings.HasPrefix(line, now.Format(time.RFC3339)) {
		t.Fatalf("linha = %q, quer o começo %q", line, now.Format(time.RFC3339))
	}
}

// TestAppendStatuslineLogKeepsPayloadPrivate checks that the payload is not
// written in the log, because it holds paths and data of the user.
func TestAppendStatuslineLogKeepsPayloadPrivate(t *testing.T) {
	dir := t.TempDir()
	payload := []byte(`{"model":{"id":"m"},"cwd":"/home/ana/projeto-secreto"}`)

	if err := appendStatuslineLog(dir, time.Now(), payload, nil); err != nil {
		t.Fatalf("appendStatuslineLog: %v", err)
	}

	data, err := os.ReadFile(statuslineLogPath(dir))
	if err != nil {
		t.Fatalf("ler log: %v", err)
	}
	if strings.Contains(string(data), "projeto-secreto") {
		t.Fatalf("log com o payload: %q", string(data))
	}
}

// TestAppendStatuslineLogWithoutRateLimits checks the line of a payload
// without rate limits.
func TestAppendStatuslineLogWithoutRateLimits(t *testing.T) {
	dir := t.TempDir()
	payload := []byte(`{"model":{"id":"claude-opus-5-5"}}`)

	if err := appendStatuslineLog(dir, time.Now(), payload, nil); err != nil {
		t.Fatalf("appendStatuslineLog: %v", err)
	}

	lines := readStatuslineLog(t, dir)
	if len(lines) != 1 {
		t.Fatalf("linhas = %d, quer 1", len(lines))
	}
	if !strings.Contains(lines[0], "rate_limits=ausente") {
		t.Fatalf("linha = %q, quer rate_limits=ausente", lines[0])
	}
}

// TestAppendStatuslineLogInvalidJSON checks the line of a payload that is
// not a valid JSON document.
func TestAppendStatuslineLogInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	payload := []byte(`{"model":`)

	err := appendStatuslineLog(dir, time.Now(), payload, errors.New("json inválido"))
	if err != nil {
		t.Fatalf("appendStatuslineLog: %v", err)
	}

	lines := readStatuslineLog(t, dir)
	if len(lines) != 1 {
		t.Fatalf("linhas = %d, quer 1", len(lines))
	}
	if !strings.Contains(lines[0], "erro=json inválido") {
		t.Fatalf("linha = %q, quer o erro preenchido", lines[0])
	}
	if !strings.Contains(lines[0], "rate_limits=ausente") {
		t.Fatalf("linha = %q, quer rate_limits=ausente", lines[0])
	}
}

// TestAppendStatuslineLogInvalidPayload checks the line of a payload that is
// not a valid JSON document, with the error of the parsing.
func TestAppendStatuslineLogInvalidPayload(t *testing.T) {
	dir := t.TempDir()
	payload := []byte(`{"model":`)

	err := appendStatuslineLog(dir, time.Now(), payload, json.Unmarshal(payload, &statuslineInput{}))
	if err != nil {
		t.Fatalf("appendStatuslineLog: %v", err)
	}

	lines := readStatuslineLog(t, dir)
	if len(lines) != 1 {
		t.Fatalf("linhas = %d, quer 1", len(lines))
	}
	if strings.Contains(lines[0], "erro=-") {
		t.Fatalf("linha = %q, quer o erro preenchido", lines[0])
	}
}

// TestAppendStatuslineLogAppends checks that every call adds a line.
func TestAppendStatuslineLogAppends(t *testing.T) {
	dir := t.TempDir()
	payload := []byte(`{"model":{"id":"m"}}`)

	for i := 0; i < 3; i++ {
		if err := appendStatuslineLog(dir, time.Now(), payload, nil); err != nil {
			t.Fatalf("appendStatuslineLog: %v", err)
		}
	}

	lines := readStatuslineLog(t, dir)
	if len(lines) != 3 {
		t.Fatalf("linhas = %d, quer 3", len(lines))
	}
}

// TestAppendStatuslineLogRotates checks that the log is trimmed when it
// grows past the limit, keeping only the last lines.
func TestAppendStatuslineLogRotates(t *testing.T) {
	dir := t.TempDir()
	path := statuslineLogPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("criar diretório: %v", err)
	}

	// A log bigger than the limit, with many lines.
	padding := strings.Repeat("x", 1024)
	var buf bytes.Buffer
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&buf, "linha %d %s\n", i, padding)
	}
	if buf.Len() <= statuslineLogMaxSize {
		t.Fatalf("log de %d bytes, quer mais de %d", buf.Len(), statuslineLogMaxSize)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("gravar log: %v", err)
	}

	payload := []byte(`{"model":{"id":"m"}}`)
	if err := appendStatuslineLog(dir, time.Now(), payload, nil); err != nil {
		t.Fatalf("appendStatuslineLog: %v", err)
	}

	lines := readStatuslineLog(t, dir)
	if len(lines) != statuslineLogKeepLines {
		t.Fatalf("linhas = %d, quer %d", len(lines), statuslineLogKeepLines)
	}
	// The oldest lines are gone and the newest ones are kept.
	if !strings.HasPrefix(lines[0], "linha 301 ") {
		t.Fatalf("primeira linha = %q", lines[0])
	}
	if !strings.HasPrefix(lines[len(lines)-2], "linha 499 ") {
		t.Fatalf("última linha antiga = %q", lines[len(lines)-2])
	}
	if !strings.Contains(lines[len(lines)-1], "model=m") {
		t.Fatalf("última linha = %q", lines[len(lines)-1])
	}

	// The log is smaller than the limit after the rotation.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("ler log: %v", err)
	}
	if info.Size() > statuslineLogMaxSize {
		t.Fatalf("log de %d bytes, quer no máximo %d", info.Size(), statuslineLogMaxSize)
	}
}

// TestAppendStatuslineLogPermissions checks that the log is only readable
// by its owner.
func TestAppendStatuslineLogPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permissões não valem no Windows")
	}

	dir := t.TempDir()
	payload := []byte(`{"model":{"id":"m"}}`)

	if err := appendStatuslineLog(dir, time.Now(), payload, nil); err != nil {
		t.Fatalf("appendStatuslineLog: %v", err)
	}

	info, err := os.Stat(statuslineLogPath(dir))
	if err != nil {
		t.Fatalf("ler log: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("permissão = %o, quer 600", perm)
	}

	// The directory of the log is only accessible by its owner too.
	dirInfo, err := os.Stat(filepath.Dir(statuslineLogPath(dir)))
	if err != nil {
		t.Fatalf("ler diretório: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("permissão do diretório = %o, quer 700", perm)
	}
}

// TestStatuslineLogLineFormat checks the format of the line of the log.
func TestStatuslineLogLineFormat(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	payload := []byte(`{"model":{"id":"m"},"version":"1.0",` +
		`"rate_limits":{"seven_day":{},"five_hour":{}}}`)

	line := statuslineLogLine(now, payload, nil)
	want := now.Format(time.RFC3339) + " bytes=" +
		fmt.Sprint(len(payload)) +
		" rate_limits=five_hour,seven_day model=m version=1.0 erro=-\n"
	if line != want {
		t.Fatalf("linha = %q, quer %q", line, want)
	}
}

// TestRateLimitKeys checks the keys of the rate limits of a payload.
func TestRateLimitKeys(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"vazio", ``, ""},
		{"nulo", `null`, ""},
		{"objeto", `{"b":{},"a":{}}`, "a,b"},
		{"inválido", `[1,2]`, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := json.RawMessage(tc.raw)
			if tc.raw == "" {
				raw = nil
			}
			keys := rateLimitKeys(raw)
			if got := strings.Join(keys, ","); got != tc.want {
				t.Fatalf("chaves = %q, quer %q", got, tc.want)
			}
		})
	}
}

// TestPrintClaudeStatuslineNeverCalled checks the status of a status line
// that was never called.
func TestPrintClaudeStatuslineNeverCalled(t *testing.T) {
	dir := t.TempDir()
	e := serviceEnv{CacheDir: dir}
	var out bytes.Buffer

	printClaudeStatusline(e, &out)

	if !strings.Contains(out.String(), "status line do Claude: nunca chamada") {
		t.Fatalf("saída = %q", out.String())
	}
}

// TestPrintClaudeStatuslineLastCall checks the status of a status line that
// was called recently.
func TestPrintClaudeStatuslineLastCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "llmwatch", "claude-statusline.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("criar diretório: %v", err)
	}
	payload := `{"model":{"id":"m"},"rate_limits":{"five_hour":{},"seven_day":{}}}`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatalf("gravar arquivo: %v", err)
	}
	when := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("mudar a data: %v", err)
	}

	e := serviceEnv{CacheDir: dir}
	var out bytes.Buffer

	printClaudeStatusline(e, &out)

	text := out.String()
	if !strings.Contains(text, "status line do Claude: última chamada há") {
		t.Fatalf("saída = %q", text)
	}
	if !strings.Contains(text, "five_hour, seven_day") {
		t.Fatalf("saída = %q, quer as chaves dos rate limits", text)
	}
}

// TestPrintClaudeStatuslineInvalid checks the status of a status line whose
// file is not a valid JSON document.
func TestPrintClaudeStatuslineInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "llmwatch", "claude-statusline.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("criar diretório: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"model":`), 0o600); err != nil {
		t.Fatalf("gravar arquivo: %v", err)
	}

	e := serviceEnv{CacheDir: dir}
	var out bytes.Buffer

	printClaudeStatusline(e, &out)

	if !strings.Contains(out.String(), "status line do Claude: arquivo inválido") {
		t.Fatalf("saída = %q", out.String())
	}
}

// TestServiceStatusShowsClaudeStatusline checks that the status of the
// service shows the state of the status line of the Claude Code.
func TestServiceStatusShowsClaudeStatusline(t *testing.T) {
	f := newFakeEnv("linux")
	f.env.CacheDir = t.TempDir()
	var out bytes.Buffer

	if err := serviceStatus(f.env, &out); err != nil {
		t.Fatalf("serviceStatus: %v", err)
	}
	if !strings.Contains(out.String(), "status line do Claude: nunca chamada") {
		t.Fatalf("saída = %q", out.String())
	}
}

// TestDefaultServiceEnvCacheDir checks that the production environment
// knows the cache directory of the user.
func TestDefaultServiceEnvCacheDir(t *testing.T) {
	e := defaultServiceEnv()

	if e.CacheDir == "" {
		t.Fatal("CacheDir vazio")
	}
	want, err := os.UserCacheDir()
	if err != nil {
		t.Skipf("diretório de cache indisponível: %v", err)
	}
	if e.CacheDir != want {
		t.Fatalf("CacheDir = %q, quer %q", e.CacheDir, want)
	}
}
