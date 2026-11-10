package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Names used by the installers to register the service.
const (
	// serviceUnit is the name of the systemd unit of the user.
	serviceUnit = "llmwatch.service"
	// serviceLabel is the label of the macOS LaunchAgent.
	serviceLabel = "io.github.llmwatch"
	// serviceTask is the name of the Windows scheduled task.
	serviceTask = "llmwatch"
)

// Timings used when waiting for the panel to answer.
const (
	// serviceStartTimeout bounds the wait for the panel after a start.
	serviceStartTimeout = 15 * time.Second
	// serviceStopTimeout bounds the wait for the panel to stop answering.
	serviceStopTimeout = 5 * time.Second
	// servicePollInterval is the interval between two probes.
	servicePollInterval = 500 * time.Millisecond
	// serviceProbeTimeout bounds a single probe request.
	serviceProbeTimeout = 2 * time.Second
)

// errServiceNotInstalled is returned when the service is missing.
var errServiceNotInstalled = errors.New("serviço do llmwatch não está instalado; " +
	"rode o instalador ou use llmwatch serve")

// errServiceUnsupported is returned on systems without a known service
// manager.
var errServiceUnsupported = errors.New("gerenciamento de serviço não suportado neste sistema; " +
	"use llmwatch serve")

// serviceEnv holds the dependencies of the service subcommands.
//
// The zero value is not usable: use defaultServiceEnv to build a
// production environment.
type serviceEnv struct {
	// GOOS is the operating system the service is managed on.
	GOOS string
	// Home is the home directory of the user.
	Home string
	// UID is the numeric identifier of the user.
	UID int
	// SelfPID is the identifier of the running process, used to avoid
	// killing it on Windows.
	SelfPID int
	// Addr is the address the server listens on.
	Addr string
	// Run runs an external command and returns its combined output.
	Run func(name string, args ...string) (output string, err error)
	// Exists reports whether a path exists.
	Exists func(path string) bool
	// Probe reports whether the panel answers on the given URL.
	Probe func(url string) bool
	// Sleep waits for the given duration.
	Sleep func(time.Duration)
}

// defaultServiceEnv returns the environment of the running program.
func defaultServiceEnv() serviceEnv {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}

	addr := os.Getenv(envAddr)
	if addr == "" {
		addr = defaultAddr
	}

	return serviceEnv{
		GOOS:    runtime.GOOS,
		Home:    home,
		UID:     os.Getuid(),
		SelfPID: os.Getpid(),
		Addr:    addr,
		Run: func(name string, args ...string) (string, error) {
			out, err := exec.Command(name, args...).CombinedOutput()
			return string(out), err
		},
		Exists: func(path string) bool {
			_, err := os.Stat(path)
			return err == nil
		},
		Probe: probePanel,
		Sleep: time.Sleep,
	}
}

// runCommand executes an external command and fails with its output.
//
// The output of the command is part of the error, because the messages of
// the service managers are the only useful clue when they fail.
func runCommand(e serviceEnv, action string, name string, args ...string) error {
	output, err := e.Run(name, args...)
	if err == nil {
		return nil
	}
	message := strings.TrimSpace(output)
	if message == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %w: %s", action, err, message)
}

// serviceStart starts the service and waits for the panel to answer.
func serviceStart(e serviceEnv, out io.Writer) error {
	switch e.GOOS {
	case "linux":
		return serviceStartLinux(e, out)
	case "darwin":
		return serviceStartDarwin(e, out)
	case "windows":
		return serviceStartWindows(e, out)
	default:
		return errServiceUnsupported
	}
}

// serviceStop stops the service and waits for the panel to stop answering.
func serviceStop(e serviceEnv, out io.Writer) error {
	switch e.GOOS {
	case "linux":
		return serviceStopLinux(e, out)
	case "darwin":
		return serviceStopDarwin(e, out)
	case "windows":
		return serviceStopWindows(e, out)
	default:
		return errServiceUnsupported
	}
}

// serviceRestart restarts the service and waits for the panel to answer.
func serviceRestart(e serviceEnv, out io.Writer) error {
	switch e.GOOS {
	case "linux":
		return serviceRestartLinux(e, out)
	case "darwin":
		return serviceRestartDarwin(e, out)
	case "windows":
		return serviceRestartWindows(e, out)
	default:
		return errServiceUnsupported
	}
}

// serviceStatus prints the state of the service and of the panel.
//
// It never fails because the service is stopped: the state is printed
// instead.
func serviceStatus(e serviceEnv, out io.Writer) error {
	switch e.GOOS {
	case "linux":
		return serviceStatusLinux(e, out)
	case "darwin":
		return serviceStatusDarwin(e, out)
	case "windows":
		return serviceStatusWindows(e, out)
	default:
		return errServiceUnsupported
	}
}

// --- Linux -----------------------------------------------------------------

// serviceUnitPath returns the path of the systemd unit of the user.
func serviceUnitPath(e serviceEnv) string {
	return filepath.Join(e.Home, ".config", "systemd", "user", serviceUnit)
}

// serviceInstalledLinux reports whether the systemd unit exists.
func serviceInstalledLinux(e serviceEnv) bool {
	return e.Exists(serviceUnitPath(e))
}

// serviceStartLinux starts the systemd unit.
func serviceStartLinux(e serviceEnv, out io.Writer) error {
	if !serviceInstalledLinux(e) {
		return errServiceNotInstalled
	}
	if err := runCommand(e, "falha ao iniciar o serviço", "systemctl", "--user", "start", serviceUnit); err != nil {
		return err
	}
	return waitPanelUp(e, out)
}

// serviceStopLinux stops the systemd unit.
func serviceStopLinux(e serviceEnv, out io.Writer) error {
	if !serviceInstalledLinux(e) {
		return errServiceNotInstalled
	}
	if err := runCommand(e, "falha ao parar o serviço", "systemctl", "--user", "stop", serviceUnit); err != nil {
		return err
	}
	return waitPanelDown(e, out)
}

// serviceRestartLinux restarts the systemd unit.
func serviceRestartLinux(e serviceEnv, out io.Writer) error {
	if !serviceInstalledLinux(e) {
		return errServiceNotInstalled
	}
	if err := runCommand(e, "falha ao reiniciar o serviço", "systemctl", "--user", "restart", serviceUnit); err != nil {
		return err
	}
	return waitPanelUp(e, out)
}

// serviceStatusLinux prints the state of the systemd unit.
func serviceStatusLinux(e serviceEnv, out io.Writer) error {
	if !serviceInstalledLinux(e) {
		fmt.Fprintln(out, "serviço: não instalado")
		printPanelStatus(e, out)
		printVersion(out)
		return nil
	}

	state := "inativo"
	if output, err := e.Run("systemctl", "--user", "is-active", serviceUnit); err == nil {
		if value := strings.TrimSpace(output); value != "" {
			state = value
		}
	}
	fmt.Fprintf(out, "serviço: instalado (%s)\n", state)
	printPanelStatus(e, out)
	printVersion(out)
	return nil
}

// --- macOS -----------------------------------------------------------------

// servicePlistPath returns the path of the LaunchAgent.
func servicePlistPath(e serviceEnv) string {
	return filepath.Join(e.Home, "Library", "LaunchAgents", serviceLabel+".plist")
}

// serviceTarget returns the launchd target of the agent.
func serviceTarget(e serviceEnv) string {
	return serviceDomain(e) + "/" + serviceLabel
}

// serviceDomain returns the launchd domain of the session of the user.
func serviceDomain(e serviceEnv) string {
	return "gui/" + strconv.Itoa(e.UID)
}

// serviceInstalledDarwin reports whether the LaunchAgent exists.
func serviceInstalledDarwin(e serviceEnv) bool {
	return e.Exists(servicePlistPath(e))
}

// serviceLoadedDarwin reports whether the agent is already loaded.
func serviceLoadedDarwin(e serviceEnv) bool {
	_, err := e.Run("launchctl", "print", serviceTarget(e))
	return err == nil
}

// serviceStartDarwin loads the agent or kicks it when it is already loaded.
func serviceStartDarwin(e serviceEnv, out io.Writer) error {
	if !serviceInstalledDarwin(e) {
		return errServiceNotInstalled
	}
	if serviceLoadedDarwin(e) {
		if err := runCommand(e, "falha ao iniciar o serviço", "launchctl", "kickstart", serviceTarget(e)); err != nil {
			return err
		}
	} else {
		if err := runCommand(e, "falha ao iniciar o serviço", "launchctl", "bootstrap",
			serviceDomain(e), servicePlistPath(e)); err != nil {
			return err
		}
	}
	return waitPanelUp(e, out)
}

// serviceStopDarwin removes the agent from the launchd session.
//
// Only bootout really stops the service, because the agent is kept alive.
func serviceStopDarwin(e serviceEnv, out io.Writer) error {
	if !serviceInstalledDarwin(e) {
		return errServiceNotInstalled
	}
	if output, err := e.Run("launchctl", "bootout", serviceTarget(e)); err != nil {
		// An agent that is not loaded is already stopped.
		if !isNotLoaded(err, output) {
			return fmt.Errorf("falha ao parar o serviço: %w: %s",
				err, strings.TrimSpace(output))
		}
	}
	return waitPanelDown(e, out)
}

// serviceRestartDarwin restarts the agent.
func serviceRestartDarwin(e serviceEnv, out io.Writer) error {
	if !serviceInstalledDarwin(e) {
		return errServiceNotInstalled
	}
	if serviceLoadedDarwin(e) {
		if err := runCommand(e, "falha ao reiniciar o serviço", "launchctl", "kickstart", "-k", serviceTarget(e)); err != nil {
			return err
		}
	} else {
		if err := runCommand(e, "falha ao reiniciar o serviço", "launchctl", "bootstrap",
			serviceDomain(e), servicePlistPath(e)); err != nil {
			return err
		}
	}
	return waitPanelUp(e, out)
}

// serviceStatusDarwin prints the state of the LaunchAgent.
func serviceStatusDarwin(e serviceEnv, out io.Writer) error {
	if !serviceInstalledDarwin(e) {
		fmt.Fprintln(out, "serviço: não instalado")
		printPanelStatus(e, out)
		printVersion(out)
		return nil
	}

	if serviceLoadedDarwin(e) {
		fmt.Fprintln(out, "serviço: instalado (carregado)")
	} else {
		fmt.Fprintln(out, "serviço: instalado (parado)")
	}
	printPanelStatus(e, out)
	printVersion(out)
	return nil
}

// isNotLoaded reports whether the error means that the agent was not
// loaded.
func isNotLoaded(err error, output string) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error() + " " + output)
	return strings.Contains(text, "not loaded") ||
		strings.Contains(text, "não carregado") ||
		strings.Contains(text, "could not find service") ||
		strings.Contains(text, "no such process")
}

// --- Windows ---------------------------------------------------------------

// serviceTaskInstalledWindows reports whether the scheduled task exists.
func serviceTaskInstalledWindows(e serviceEnv) bool {
	_, err := e.Run("schtasks", "/Query", "/TN", serviceTask)
	return err == nil
}

// serviceStartWindows runs the scheduled task.
func serviceStartWindows(e serviceEnv, out io.Writer) error {
	if !serviceTaskInstalledWindows(e) {
		return errServiceNotInstalled
	}
	return startWindows(e, out)
}

// startWindows runs the scheduled task of an installed service.
func startWindows(e serviceEnv, out io.Writer) error {
	if err := runCommand(e, "falha ao iniciar o serviço", "schtasks", "/Run", "/TN", serviceTask); err != nil {
		return err
	}
	return waitPanelUp(e, out)
}

// serviceStopWindows kills the running server.
//
// The scheduled task ends as soon as it starts the hidden server, so the
// process itself must be killed. The taskkill command fails when no
// process matches, which means the service was already stopped.
func serviceStopWindows(e serviceEnv, out io.Writer) error {
	if !serviceTaskInstalledWindows(e) {
		return errServiceNotInstalled
	}
	return stopWindows(e, out)
}

// stopWindows kills the running server of an installed service.
func stopWindows(e serviceEnv, out io.Writer) error {
	filter := fmt.Sprintf("/FI \"PID ne %d\"", e.SelfPID)
	output, err := e.Run("taskkill", "/F", "/IM", "llmwatch.exe", filter)
	if err != nil {
		// No matching process means the service was already stopped.
		if !isNoProcessFound(err, output) {
			return fmt.Errorf("falha ao parar o serviço: %w: %s",
				err, strings.TrimSpace(output))
		}
	}
	return waitPanelDown(e, out)
}

// serviceRestartWindows stops the service and starts it again.
func serviceRestartWindows(e serviceEnv, out io.Writer) error {
	if !serviceTaskInstalledWindows(e) {
		return errServiceNotInstalled
	}
	if err := stopWindows(e, out); err != nil {
		return err
	}
	e.Sleep(time.Second)
	return startWindows(e, out)
}

// serviceStatusWindows prints the state of the scheduled task.
func serviceStatusWindows(e serviceEnv, out io.Writer) error {
	if !serviceTaskInstalledWindows(e) {
		fmt.Fprintln(out, "serviço: não instalado")
		printPanelStatus(e, out)
		printVersion(out)
		return nil
	}

	// The task ends as soon as it starts the hidden server, so its own
	// state says nothing about the running panel.
	fmt.Fprintln(out, "serviço: instalado")
	printPanelStatus(e, out)
	printVersion(out)
	return nil
}

// isNoProcessFound reports whether the error means that no process was
// found.
func isNoProcessFound(err error, output string) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error() + " " + output)
	return strings.Contains(text, "não encontrado") ||
		strings.Contains(text, "nao encontrado") ||
		strings.Contains(text, "not found") ||
		strings.Contains(text, "no running instance")
}

// --- Probe and messages ----------------------------------------------------

// probeURL returns the URL used to check whether the panel answers.
func probeURL(addr string) string {
	return "http://" + probeHost(addr) + "/login"
}

// probeHost returns the host to use in the probe URL.
//
// A wildcard host is replaced by the loopback address, because the probe
// runs on the machine itself.
func probeHost(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = addr, "18123"
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	if port == "" {
		port = "18123"
	}
	return net.JoinHostPort(host, port)
}

// probePanel reports whether the panel answers on the given URL.
func probePanel(url string) bool {
	client := &http.Client{Timeout: serviceProbeTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	return resp.StatusCode < 500
}

// waitPanelUp waits for the panel to answer and reports the address.
//
// It gives up after 15 seconds and points to the logs of the system.
func waitPanelUp(e serviceEnv, out io.Writer) error {
	if !waitPanel(e, serviceStartTimeout, true) {
		return fmt.Errorf("o serviço foi iniciado mas o painel não respondeu em 15s; "+
			"veja os logs (%s)", logHint(e.GOOS))
	}
	fmt.Fprintf(out, "llmwatch rodando em http://%s\n", e.Addr)
	return nil
}

// waitPanelDown waits for the panel to stop answering.
func waitPanelDown(e serviceEnv, out io.Writer) error {
	if !waitPanel(e, serviceStopTimeout, false) {
		return fmt.Errorf("o serviço foi parado mas o painel ainda responde em %s", e.Addr)
	}
	fmt.Fprintln(out, "llmwatch parado")
	return nil
}

// waitPanel probes the panel until it reaches the wanted state.
//
// It reports whether the panel reached the state before the timeout. The
// panel must answer when up is true and must stay silent when it is
// false.
//
// The number of probes is bounded by the timeout, so the wait never
// depends on the clock of the machine.
func waitPanel(e serviceEnv, timeout time.Duration, up bool) bool {
	attempts := int(timeout / servicePollInterval)
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if e.Probe(probeURL(e.Addr)) == up {
			return true
		}
		if attempt < attempts-1 {
			e.Sleep(servicePollInterval)
		}
	}
	return false
}

// logHint returns the hint of where to look for the logs of the service.
func logHint(goos string) string {
	switch goos {
	case "linux":
		return "journalctl --user -u llmwatch -e"
	case "darwin":
		return "~/Library/Logs/llmwatch.log"
	default:
		return "rode llmwatch serve num terminal para ver o erro"
	}
}

// printPanelStatus prints whether the panel answers.
func printPanelStatus(e serviceEnv, out io.Writer) {
	url := probeURL(e.Addr)
	if e.Probe(url) {
		fmt.Fprintf(out, "painel: respondendo em http://%s\n", e.Addr)
		return
	}
	fmt.Fprintf(out, "painel: não responde\n")
}

// printVersion prints the version of the program.
func printVersion(out io.Writer) {
	fmt.Fprintf(out, "versão: %s\n", version)
}
