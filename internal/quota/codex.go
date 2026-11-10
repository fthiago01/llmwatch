package quota

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"llmwatch/internal/usage"
)

// codexTimeout is the maximum time the Codex app-server may take to answer
// the rate limits request.
const codexTimeout = 20 * time.Second

// Codex reads quota information from the local Codex CLI app-server.
type Codex struct {
	bin string
}

// NewCodex creates a Codex quota provider backed by the codex CLI. An empty
// bin falls back to "codex", resolved from PATH when the quota is fetched.
func NewCodex(bin string) *Codex {
	if bin == "" {
		bin = "codex"
	}
	return &Codex{bin: bin}
}

// Name identifies the quota provider.
func (c *Codex) Name() string {
	return "codex"
}

// Interval is the frequency at which the Codex CLI is queried.
func (c *Codex) Interval() time.Duration {
	return 60 * time.Second
}

// Fetch asks the Codex app-server for the account rate limits.
func (c *Codex) Fetch(ctx context.Context) usage.QuotaStatus {
	status := usage.QuotaStatus{
		Provider: "openai",
		Source:   "codex-app-server",
		Windows:  []usage.QuotaWindow{},
	}

	if _, err := exec.LookPath(c.bin); err != nil {
		status.Error = "codex CLI não encontrado (LLMWATCH_CODEX_BIN)"
		return status
	}

	ctx, cancel := context.WithTimeout(ctx, codexTimeout)
	defer cancel()

	command := exec.CommandContext(ctx, c.bin, "app-server")
	stdin, err := command.StdinPipe()
	if err != nil {
		status.Error = fmt.Sprintf("codex: %v", err)
		return status
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		status.Error = fmt.Sprintf("codex: %v", err)
		return status
	}
	command.Stderr = io.Discard

	if err := command.Start(); err != nil {
		status.Error = fmt.Sprintf("codex: %v", err)
		return status
	}

	// The server terminates when stdin is closed, so it is only closed once
	// the answer has been received.
	defer func() {
		_ = stdin.Close()
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		// The wait error after a kill is expected and not reported.
		_ = command.Wait()
	}()

	requests := []string{
		`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"llmwatch","version":"0.1"}}}`,
		`{"method":"initialized"}`,
		`{"id":2,"method":"account/rateLimits/read"}`,
	}
	for _, request := range requests {
		if _, err := io.WriteString(stdin, request+"\n"); err != nil {
			status.Error = fmt.Sprintf("codex: %v", err)
			return status
		}
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var response codexResponse
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			continue
		}
		if response.ID != 2 {
			// Notifications and replies to other requests are ignored.
			continue
		}

		if response.Error != nil && response.Error.Message != "" {
			message := response.Error.Message
			if strings.Contains(strings.ToLower(message), "authentication") {
				message += " — rode: codex login"
			}
			status.Error = message
			return status
		}

		status.Plan = response.Result.RateLimits.PlanType
		status.Windows = codexWindows(response.Result.RateLimits, time.Now())
		status.UpdatedAt = time.Now()
		return status
	}

	if err := scanner.Err(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status.Error = "codex app-server não respondeu em 20s"
			return status
		}
		status.Error = fmt.Sprintf("codex: %v", err)
		return status
	}

	if ctx.Err() != nil {
		status.Error = "codex app-server não respondeu em 20s"
		return status
	}

	status.Error = "codex app-server não respondeu em 20s"
	return status
}

// codexWindows converts the rate limits reported by the app-server into
// quota windows.
func codexWindows(limits codexRateLimits, now time.Time) []usage.QuotaWindow {
	windows := []usage.QuotaWindow{}

	if window := codexWindow("primária", limits.Primary, now); window != nil {
		windows = append(windows, *window)
	}
	if window := codexWindow("secundária", limits.Secondary, now); window != nil {
		windows = append(windows, *window)
	}

	return windows
}

// codexWindow converts a single rate limit entry into a quota window. It
// returns nil when the entry is absent.
func codexWindow(fallback string, limit *codexLimit, now time.Time) *usage.QuotaWindow {
	if limit == nil {
		return nil
	}

	name := fallback
	if limit.WindowDurationMins > 0 {
		switch limit.WindowDurationMins {
		case 300:
			name = "5h"
		case 10080:
			name = "7d"
		default:
			name = fmt.Sprintf("%dmin", limit.WindowDurationMins)
		}
	}

	window := usage.QuotaWindow{
		Name:        name,
		UsedPercent: limit.UsedPercent,
	}
	if limit.ResetsAt != nil {
		reset := time.Unix(*limit.ResetsAt, 0)
		window.ResetsAt = &reset
	}
	return &window
}

// codexResponse is a JSON-RPC message returned by the app-server.
type codexResponse struct {
	ID     int64       `json:"id"`
	Error  *codexError `json:"error"`
	Result codexResult `json:"result"`
}

// codexError is the error member of a JSON-RPC response.
type codexError struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

// codexResult is the result member of the rate limits response.
type codexResult struct {
	RateLimits codexRateLimits `json:"rateLimits"`
}

// codexRateLimits holds the rate limits of the account.
type codexRateLimits struct {
	PlanType  string      `json:"planType"`
	Primary   *codexLimit `json:"primary"`
	Secondary *codexLimit `json:"secondary"`
}

// codexLimit describes a single rate limit window.
type codexLimit struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins int64   `json:"windowDurationMins"`
	ResetsAt           *int64  `json:"resetsAt"`
}
