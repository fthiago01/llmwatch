package main

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRun records the commands run by the service subcommands.
type fakeRun struct {
	// commands holds the commands run, in order.
	commands [][]string
	// replies maps a command to its output.
	//
	// The key is the whole command line, joined by spaces.
	replies map[string]string
	// errors maps a command to its error.
	//
	// The key is the whole command line, joined by spaces. A command
	// without an entry fails only when it is listed in "failing".
	errors map[string]error
	// failing holds the commands that must fail.
	//
	// The key is the whole command line, joined by spaces.
	failing map[string]bool
}

// newFakeRun returns a recorder without any reply.
func newFakeRun() *fakeRun {
	return &fakeRun{
		replies: map[string]string{},
		errors:  map[string]error{},
		failing: map[string]bool{},
	}
}

// key returns the key of a command in the maps of the recorder.
func key(name string, args ...string) string {
	return strings.TrimSpace(name + " " + strings.Join(args, " "))
}

// run records the command and returns the canned reply.
func (f *fakeRun) run(name string, args ...string) (string, error) {
	f.commands = append(f.commands, append([]string{name}, args...))
	id := key(name, args...)
	output := f.replies[id]
	if !f.failing[id] {
		return output, nil
	}
	err := f.errors[id]
	if err == nil {
		err = errors.New("exit status 1")
	}
	return output, err
}

// fail makes the given command fail with the given output.
func (f *fakeRun) fail(name string, args []string, output string) {
	id := key(name, args...)
	f.failing[id] = true
	f.errors[id] = errors.New("exit status 1")
	f.replies[id] = output
}

// lines returns the commands run, formatted for the comparison.
func (f *fakeRun) lines() []string {
	lines := make([]string, 0, len(f.commands))
	for _, command := range f.commands {
		parts := make([]string, 0, len(command))
		for _, arg := range command {
			parts = append(parts, quoteArg(arg))
		}
		lines = append(lines, strings.Join(parts, " "))
	}
	return lines
}

// quoteArg quotes the arguments holding a space.
//
// An argument that already holds a quote is left alone, because the
// quoting would break the reading of the command.
func quoteArg(arg string) string {
	if strings.ContainsAny(arg, " ") && !strings.Contains(arg, `"`) {
		return `"` + arg + `"`
	}
	return arg
}

// fakeEnv builds an environment with the given system and recorder.
type fakeEnv struct {
	env serviceEnv
	run *fakeRun
	// probes holds the URLs probed, in order.
	probes []string
	// sleeps holds the durations waited, in order.
	sleeps []time.Duration
	// answers holds the replies of the probe, in order.
	//
	// When it is empty the probe always answers with panelUp.
	answers []bool
	// panelUp is the reply of the probe when no answer is queued.
	panelUp bool
}

// newFakeEnv builds an environment for the given system.
//
// The service is installed and the panel answers, unless the caller says
// otherwise.
func newFakeEnv(goos string) *fakeEnv {
	recorder := newFakeRun()
	f := &fakeEnv{run: recorder, panelUp: true}

	f.env = serviceEnv{
		GOOS:    goos,
		Home:    filepath.Join("/home", "ana"),
		UID:     1000,
		SelfPID: 4242,
		Addr:    "127.0.0.1:18123",
		Run:     recorder.run,
		Exists:  func(string) bool { return true },
		Probe:   f.probe,
		Sleep:   func(d time.Duration) { f.sleeps = append(f.sleeps, d) },
	}
	return f
}

// probe answers with the queued replies.
func (f *fakeEnv) probe(url string) bool {
	f.probes = append(f.probes, url)
	if len(f.answers) == 0 {
		return f.panelUp
	}
	answer := f.answers[0]
	f.answers = f.answers[1:]
	return answer
}

// wantCommands compares the commands run with the expected ones.
func wantCommands(t *testing.T, f *fakeEnv, want []string) {
	t.Helper()
	got := f.run.lines()
	if len(got) != len(want) {
		t.Fatalf("comandos = %v, quer %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("comandos = %v, quer %v", got, want)
		}
	}
}

// TestServiceStartLinux checks the commands of the start on Linux.
func TestServiceStartLinux(t *testing.T) {
	f := newFakeEnv("linux")
	var out bytes.Buffer

	if err := serviceStart(f.env, &out); err != nil {
		t.Fatalf("serviceStart: %v", err)
	}
	wantCommands(t, f, []string{
		"systemctl --user start llmwatch.service",
	})
	if !strings.Contains(out.String(), "llmwatch rodando em http://127.0.0.1:18123") {
		t.Fatalf("saída = %q", out.String())
	}
}

// TestServiceStopLinux checks the commands of the stop on Linux.
func TestServiceStopLinux(t *testing.T) {
	f := newFakeEnv("linux")
	// The panel is down after the stop.
	f.panelUp = false
	var out bytes.Buffer

	if err := serviceStop(f.env, &out); err != nil {
		t.Fatalf("serviceStop: %v", err)
	}
	wantCommands(t, f, []string{
		"systemctl --user stop llmwatch.service",
	})
	if !strings.Contains(out.String(), "llmwatch parado") {
		t.Fatalf("saída = %q", out.String())
	}
}

// TestServiceRestartLinux checks the commands of the restart on Linux.
func TestServiceRestartLinux(t *testing.T) {
	f := newFakeEnv("linux")
	var out bytes.Buffer

	if err := serviceRestart(f.env, &out); err != nil {
		t.Fatalf("serviceRestart: %v", err)
	}
	wantCommands(t, f, []string{
		"systemctl --user restart llmwatch.service",
	})
}

// TestServiceStartDarwinNotLoaded checks the bootstrap of an unloaded
// agent.
func TestServiceStartDarwinNotLoaded(t *testing.T) {
	f := newFakeEnv("darwin")
	// The agent is not loaded: the print fails.
	f.run.fail("launchctl", []string{"print", "gui/1000/io.github.llmwatch"},
		"Could not find service")
	var out bytes.Buffer

	if err := serviceStart(f.env, &out); err != nil {
		t.Fatalf("serviceStart: %v", err)
	}
	wantCommands(t, f, []string{
		"launchctl print gui/1000/io.github.llmwatch",
		"launchctl bootstrap gui/1000 " +
			filepath.Join("/home", "ana", "Library", "LaunchAgents", "io.github.llmwatch.plist"),
	})
}

// TestServiceStartDarwinLoaded checks the kickstart of a loaded agent.
func TestServiceStartDarwinLoaded(t *testing.T) {
	f := newFakeEnv("darwin")
	// The agent is loaded: the print succeeds.
	f.run.replies[key("launchctl", "print", "gui/1000/io.github.llmwatch")] =
		"service = gui/1000/io.github.llmwatch"
	var out bytes.Buffer

	if err := serviceStart(f.env, &out); err != nil {
		t.Fatalf("serviceStart: %v", err)
	}
	wantCommands(t, f, []string{
		"launchctl print gui/1000/io.github.llmwatch",
		"launchctl kickstart gui/1000/io.github.llmwatch",
	})
}

// TestServiceStopDarwin checks the commands of the stop on macOS.
func TestServiceStopDarwin(t *testing.T) {
	f := newFakeEnv("darwin")
	f.panelUp = false
	var out bytes.Buffer

	if err := serviceStop(f.env, &out); err != nil {
		t.Fatalf("serviceStop: %v", err)
	}
	wantCommands(t, f, []string{
		"launchctl bootout gui/1000/io.github.llmwatch",
	})
}

// TestServiceRestartDarwinLoaded checks the kickstart of a loaded agent.
func TestServiceRestartDarwinLoaded(t *testing.T) {
	f := newFakeEnv("darwin")
	f.run.replies[key("launchctl", "print", "gui/1000/io.github.llmwatch")] =
		"service = gui/1000/io.github.llmwatch"
	var out bytes.Buffer

	if err := serviceRestart(f.env, &out); err != nil {
		t.Fatalf("serviceRestart: %v", err)
	}
	wantCommands(t, f, []string{
		"launchctl print gui/1000/io.github.llmwatch",
		"launchctl kickstart -k gui/1000/io.github.llmwatch",
	})
}

// TestServiceRestartDarwinNotLoaded checks the bootstrap of an unloaded
// agent.
func TestServiceRestartDarwinNotLoaded(t *testing.T) {
	f := newFakeEnv("darwin")
	f.run.fail("launchctl", []string{"print", "gui/1000/io.github.llmwatch"},
		"Could not find service")
	var out bytes.Buffer

	if err := serviceRestart(f.env, &out); err != nil {
		t.Fatalf("serviceRestart: %v", err)
	}
	wantCommands(t, f, []string{
		"launchctl print gui/1000/io.github.llmwatch",
		"launchctl bootstrap gui/1000 " +
			filepath.Join("/home", "ana", "Library", "LaunchAgents", "io.github.llmwatch.plist"),
	})
}

// TestServiceStartWindows checks the commands of the start on Windows.
func TestServiceStartWindows(t *testing.T) {
	f := newFakeEnv("windows")
	var out bytes.Buffer

	if err := serviceStart(f.env, &out); err != nil {
		t.Fatalf("serviceStart: %v", err)
	}
	wantCommands(t, f, []string{
		"schtasks /Query /TN llmwatch",
		"schtasks /Run /TN llmwatch",
	})
}

// TestServiceStopWindows checks the commands of the stop on Windows.
func TestServiceStopWindows(t *testing.T) {
	f := newFakeEnv("windows")
	f.panelUp = false
	var out bytes.Buffer

	if err := serviceStop(f.env, &out); err != nil {
		t.Fatalf("serviceStop: %v", err)
	}
	wantCommands(t, f, []string{
		"schtasks /Query /TN llmwatch",
		`taskkill /F /IM llmwatch.exe /FI "PID ne 4242"`,
	})
}

// TestServiceStopWindowsNotFound checks that a missing process is not an
// error.
func TestServiceStopWindowsNotFound(t *testing.T) {
	f := newFakeEnv("windows")
	f.panelUp = false
	f.run.fail("taskkill",
		[]string{"/F", "/IM", "llmwatch.exe", `/FI "PID ne 4242"`},
		`ERROR: The process "llmwatch.exe" not found.`)
	var out bytes.Buffer

	if err := serviceStop(f.env, &out); err != nil {
		t.Fatalf("serviceStop: %v", err)
	}
	if !strings.Contains(out.String(), "llmwatch parado") {
		t.Fatalf("saída = %q", out.String())
	}
}

// TestServiceRestartWindows checks the commands of the restart on
// Windows.
func TestServiceRestartWindows(t *testing.T) {
	f := newFakeEnv("windows")
	// The panel is down after the stop and up again after the start.
	f.answers = []bool{false, true}
	var out bytes.Buffer

	if err := serviceRestart(f.env, &out); err != nil {
		t.Fatalf("serviceRestart: %v", err)
	}
	wantCommands(t, f, []string{
		"schtasks /Query /TN llmwatch",
		`taskkill /F /IM llmwatch.exe /FI "PID ne 4242"`,
		"schtasks /Run /TN llmwatch",
	})

	found := false
	for _, d := range f.sleeps {
		if d == time.Second {
			found = true
		}
	}
	if !found {
		t.Fatalf("esperas = %v, quer uma de 1s", f.sleeps)
	}
}

// TestServiceNotInstalled checks the error of a missing service.
func TestServiceNotInstalled(t *testing.T) {
	cases := []struct {
		name string
		goos string
	}{
		{"linux", "linux"},
		{"darwin", "darwin"},
		{"windows", "windows"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeEnv(tc.goos)
			f.env.Exists = func(string) bool { return false }
			if tc.goos == "windows" {
				// The task is missing, so the query fails.
				f.run.fail("schtasks", []string{"/Query", "/TN", "llmwatch"},
					"ERROR: the system cannot find the file")
			}
			var out bytes.Buffer

			err := serviceStart(f.env, &out)
			if !errors.Is(err, errServiceNotInstalled) {
				t.Fatalf("serviceStart = %v, quer %v", err, errServiceNotInstalled)
			}
			if !strings.Contains(err.Error(), "não está instalado") {
				t.Fatalf("erro = %q", err.Error())
			}
			if err := serviceStop(f.env, &out); !errors.Is(err, errServiceNotInstalled) {
				t.Fatalf("serviceStop = %v, quer %v", err, errServiceNotInstalled)
			}
			if err := serviceRestart(f.env, &out); !errors.Is(err, errServiceNotInstalled) {
				t.Fatalf("serviceRestart = %v, quer %v", err, errServiceNotInstalled)
			}
		})
	}
}

// TestServiceStartProbeTimeout checks the error of a panel that never
// answers.
func TestServiceStartProbeTimeout(t *testing.T) {
	f := newFakeEnv("linux")
	f.panelUp = false
	var out bytes.Buffer

	err := serviceStart(f.env, &out)
	if err == nil {
		t.Fatal("serviceStart: quer erro")
	}
	if !strings.Contains(err.Error(), "não respondeu em 15s") {
		t.Fatalf("erro = %q", err.Error())
	}
	if !strings.Contains(err.Error(), "journalctl --user -u llmwatch -e") {
		t.Fatalf("erro = %q", err.Error())
	}
	// The panel is probed every half second during fifteen seconds.
	if len(f.probes) != 30 {
		t.Fatalf("probes = %d, quer 30", len(f.probes))
	}
	if len(f.sleeps) != 29 {
		t.Fatalf("esperas = %d, quer 29", len(f.sleeps))
	}
}

// TestServiceStartProbeTimeoutLogHint checks the hint of the logs of
// every system.
func TestServiceStartProbeTimeoutLogHint(t *testing.T) {
	hints := map[string]string{
		"linux":   "journalctl --user -u llmwatch -e",
		"darwin":  "~/Library/Logs/llmwatch.log",
		"windows": "rode llmwatch serve num terminal para ver o erro",
	}

	for goos, want := range hints {
		t.Run(goos, func(t *testing.T) {
			f := newFakeEnv(goos)
			f.panelUp = false
			var out bytes.Buffer

			err := serviceStart(f.env, &out)
			if err == nil {
				t.Fatal("serviceStart: quer erro")
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("erro = %q, quer %q", err.Error(), want)
			}
		})
	}
}

// TestServiceProbeUsesLoopback checks that a wildcard host is replaced by
// the loopback address.
func TestServiceProbeUsesLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		{"0.0.0.0:18123", "http://127.0.0.1:18123/login"},
		{"[::]:18123", "http://127.0.0.1:18123/login"},
		{":18123", "http://127.0.0.1:18123/login"},
		{"127.0.0.1:18123", "http://127.0.0.1:18123/login"},
		{"localhost:9000", "http://localhost:9000/login"},
	}

	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			f := newFakeEnv("linux")
			f.env.Addr = tc.addr
			var out bytes.Buffer

			if err := serviceStart(f.env, &out); err != nil {
				t.Fatalf("serviceStart: %v", err)
			}
			if len(f.probes) == 0 {
				t.Fatal("nenhum probe")
			}
			if f.probes[0] != tc.want {
				t.Fatalf("probe = %q, quer %q", f.probes[0], tc.want)
			}
		})
	}
}

// TestServiceStatusStopped checks that the status never fails when the
// service is stopped.
func TestServiceStatusStopped(t *testing.T) {
	cases := []string{"linux", "darwin", "windows"}

	for _, goos := range cases {
		t.Run(goos, func(t *testing.T) {
			f := newFakeEnv(goos)
			f.panelUp = false
			var out bytes.Buffer

			if err := serviceStatus(f.env, &out); err != nil {
				t.Fatalf("serviceStatus: %v", err)
			}
			text := out.String()
			if !strings.Contains(text, "painel: não responde") {
				t.Fatalf("saída = %q", text)
			}
			if !strings.Contains(text, "versão: ") {
				t.Fatalf("saída = %q", text)
			}
		})
	}
}

// TestServiceStatusNotInstalled checks the status of a missing service.
func TestServiceStatusNotInstalled(t *testing.T) {
	f := newFakeEnv("linux")
	f.env.Exists = func(string) bool { return false }
	var out bytes.Buffer

	if err := serviceStatus(f.env, &out); err != nil {
		t.Fatalf("serviceStatus: %v", err)
	}
	if !strings.Contains(out.String(), "serviço: não instalado") {
		t.Fatalf("saída = %q", out.String())
	}
}

// TestServiceStatusActive checks the status of a running service.
func TestServiceStatusActive(t *testing.T) {
	f := newFakeEnv("linux")
	f.run.replies[key("systemctl", "--user", "is-active", "llmwatch.service")] = "active"
	var out bytes.Buffer

	if err := serviceStatus(f.env, &out); err != nil {
		t.Fatalf("serviceStatus: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "serviço: instalado (active)") {
		t.Fatalf("saída = %q", text)
	}
	if !strings.Contains(text, "painel: respondendo em http://127.0.0.1:18123") {
		t.Fatalf("saída = %q", text)
	}
}

// TestServiceUnsupported checks the error of an unknown system.
func TestServiceUnsupported(t *testing.T) {
	f := newFakeEnv("plan9")
	var out bytes.Buffer

	err := serviceStart(f.env, &out)
	if !errors.Is(err, errServiceUnsupported) {
		t.Fatalf("serviceStart = %v, quer %v", err, errServiceUnsupported)
	}
	if !strings.Contains(err.Error(), "não suportado neste sistema") {
		t.Fatalf("erro = %q", err.Error())
	}
	if err := serviceStop(f.env, &out); !errors.Is(err, errServiceUnsupported) {
		t.Fatalf("serviceStop = %v, quer %v", err, errServiceUnsupported)
	}
	if err := serviceRestart(f.env, &out); !errors.Is(err, errServiceUnsupported) {
		t.Fatalf("serviceRestart = %v, quer %v", err, errServiceUnsupported)
	}
	if err := serviceStatus(f.env, &out); !errors.Is(err, errServiceUnsupported) {
		t.Fatalf("serviceStatus = %v, quer %v", err, errServiceUnsupported)
	}
}

// TestServiceCommandErrorIncludesOutput checks that the output of a
// failing command is part of the error.
func TestServiceCommandErrorIncludesOutput(t *testing.T) {
	f := newFakeEnv("linux")
	f.run.fail("systemctl", []string{"--user", "start", "llmwatch.service"},
		"Failed to start llmwatch.service: Unit not found.")
	var out bytes.Buffer

	err := serviceStart(f.env, &out)
	if err == nil {
		t.Fatal("serviceStart: quer erro")
	}
	if !strings.Contains(err.Error(), "Unit not found") {
		t.Fatalf("erro = %q", err.Error())
	}
}

// TestServiceStopWaitsForPanelDown checks that the stop waits for the
// panel to stop answering.
func TestServiceStopWaitsForPanelDown(t *testing.T) {
	f := newFakeEnv("linux")
	f.answers = []bool{true, true, false}
	var out bytes.Buffer

	if err := serviceStop(f.env, &out); err != nil {
		t.Fatalf("serviceStop: %v", err)
	}
	if len(f.probes) != 3 {
		t.Fatalf("probes = %d, quer 3", len(f.probes))
	}
}

// TestServiceStopPanelStillUp checks the error of a panel that keeps
// answering after the stop.
func TestServiceStopPanelStillUp(t *testing.T) {
	f := newFakeEnv("linux")
	f.panelUp = true
	var out bytes.Buffer

	err := serviceStop(f.env, &out)
	if err == nil {
		t.Fatal("serviceStop: quer erro")
	}
	if !strings.Contains(err.Error(), "ainda responde") {
		t.Fatalf("erro = %q", err.Error())
	}
	// The panel is probed every half second during five seconds.
	if len(f.probes) != 10 {
		t.Fatalf("probes = %d, quer 10", len(f.probes))
	}
}

// TestServiceStartWaitsForPanelUp checks that the start waits for the
// panel to answer.
func TestServiceStartWaitsForPanelUp(t *testing.T) {
	f := newFakeEnv("linux")
	f.answers = []bool{false, true}
	var out bytes.Buffer

	if err := serviceStart(f.env, &out); err != nil {
		t.Fatalf("serviceStart: %v", err)
	}
	if len(f.probes) != 2 {
		t.Fatalf("probes = %d, quer 2", len(f.probes))
	}
	if len(f.sleeps) == 0 {
		t.Fatal("nenhuma espera")
	}
	for _, d := range f.sleeps {
		if d != 500*time.Millisecond {
			t.Fatalf("espera = %v, quer 500ms", d)
		}
	}
}

// TestServiceDarwinStopNotLoaded checks that an unloaded agent is not an
// error.
func TestServiceDarwinStopNotLoaded(t *testing.T) {
	f := newFakeEnv("darwin")
	f.panelUp = false
	f.run.fail("launchctl", []string{"bootout", "gui/1000/io.github.llmwatch"},
		"Could not find service \"gui/1000/io.github.llmwatch\"")
	var out bytes.Buffer

	if err := serviceStop(f.env, &out); err != nil {
		t.Fatalf("serviceStop: %v", err)
	}
	wantCommands(t, f, []string{
		"launchctl bootout gui/1000/io.github.llmwatch",
	})
}

// TestServicePaths checks the paths of the service files.
func TestServicePaths(t *testing.T) {
	e := serviceEnv{Home: filepath.Join("/home", "ana"), UID: 1000}

	unit := serviceUnitPath(e)
	want := filepath.Join("/home", "ana", ".config", "systemd", "user", "llmwatch.service")
	if unit != want {
		t.Fatalf("unit = %q, quer %q", unit, want)
	}

	plist := servicePlistPath(e)
	want = filepath.Join("/home", "ana", "Library", "LaunchAgents", "io.github.llmwatch.plist")
	if plist != want {
		t.Fatalf("plist = %q, quer %q", plist, want)
	}

	target := serviceTarget(e)
	if target != "gui/1000/io.github.llmwatch" {
		t.Fatalf("target = %q", target)
	}
}

// TestDefaultServiceEnv checks that the production environment is
// complete.
func TestDefaultServiceEnv(t *testing.T) {
	e := defaultServiceEnv()

	if e.GOOS == "" {
		t.Fatal("GOOS vazio")
	}
	if e.Home == "" {
		t.Fatal("Home vazio")
	}
	if e.SelfPID <= 0 {
		t.Fatalf("SelfPID = %d", e.SelfPID)
	}
	if e.Addr == "" {
		t.Fatal("Addr vazio")
	}
	if e.Run == nil || e.Exists == nil || e.Probe == nil || e.Sleep == nil {
		t.Fatal("dependência nula")
	}
	if !e.Exists(e.Home) {
		t.Fatalf("Home %q não existe", e.Home)
	}
}

// TestProbeHost checks the host used by the probe.
func TestProbeHost(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:18123":   "127.0.0.1:18123",
		"[::]:18123":      "127.0.0.1:18123",
		":18123":          "127.0.0.1:18123",
		"127.0.0.1:18123": "127.0.0.1:18123",
		"192.168.0.5:80":  "192.168.0.5:80",
	}

	for addr, want := range cases {
		if got := probeHost(addr); got != want {
			t.Fatalf("probeHost(%q) = %q, quer %q", addr, got, want)
		}
	}
}

// TestLogHint checks the hint of the logs of every system.
func TestLogHint(t *testing.T) {
	cases := map[string]string{
		"linux":   "journalctl --user -u llmwatch -e",
		"darwin":  "~/Library/Logs/llmwatch.log",
		"windows": "rode llmwatch serve num terminal para ver o erro",
	}

	for goos, want := range cases {
		if got := logHint(goos); got != want {
			t.Fatalf("logHint(%q) = %q, quer %q", goos, got, want)
		}
	}
}

// TestServiceUsageMentionsCommands checks that the help lists the
// service commands.
func TestServiceUsageMentionsCommands(t *testing.T) {
	for _, name := range []string{"start", "stop", "restart", "status"} {
		if !strings.Contains(usage, "\n  "+name+" ") {
			t.Fatalf("ajuda sem o comando %q", name)
		}
	}
}

// TestFakeRunQuotes checks the formatting of the recorded commands.
func TestFakeRunQuotes(t *testing.T) {
	f := newFakeRun()
	f.run("taskkill", "/F", "/IM", "llmwatch.exe", `/FI "PID ne 4242"`)

	got := f.lines()
	want := []string{`taskkill /F /IM llmwatch.exe /FI "PID ne 4242"`}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("linhas = %v, quer %v", got, want)
	}
}
