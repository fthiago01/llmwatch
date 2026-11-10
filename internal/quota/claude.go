package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"llmwatch/internal/usage"
)

// Claude reads quota information from the status-line JSON written by Claude
// Code.
type Claude struct {
	path string
}

// NewClaude creates a Claude quota provider backed by path.
func NewClaude(path string) *Claude {
	return &Claude{path: path}
}

// Name identifies the quota provider.
func (c *Claude) Name() string {
	return "claude"
}

// Interval is the frequency at which Claude Code updates its status line.
func (c *Claude) Interval() time.Duration {
	return 3 * time.Second
}

// Fetch reads and parses the Claude Code status-line file.
func (c *Claude) Fetch(ctx context.Context) usage.QuotaStatus {
	status := usage.QuotaStatus{
		Provider: "anthropic",
		Source:   "claude-statusline",
		Windows:  []usage.QuotaWindow{},
	}

	data, err := os.ReadFile(c.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			status.Error = "status line do Claude Code ainda não gravou dados (rode o Claude Code)"
		} else {
			status.Error = fmt.Sprintf("não foi possível ler status line do Claude Code: %v", err)
		}
		return status
	}

	info, err := os.Stat(c.path)
	if err == nil {
		status.UpdatedAt = info.ModTime()
	}

	var document struct {
		RateLimits json.RawMessage `json:"rate_limits"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		status.Error = fmt.Sprintf("JSON inválido no status line do Claude Code: %v", err)
		return status
	}
	if len(document.RateLimits) == 0 || string(document.RateLimits) == "null" {
		status.Error = "rate_limits ausente: plano sem limites ou Claude Code desatualizado"
		return status
	}

	var limits map[string]json.RawMessage
	if err := json.Unmarshal(document.RateLimits, &limits); err != nil {
		status.Error = fmt.Sprintf("rate_limits inválido no status line do Claude Code: %v", err)
		return status
	}

	now := time.Now()
	for key, raw := range limits {
		var window struct {
			UsedPercentage *float64 `json:"used_percentage"`
			ResetsAt       *int64   `json:"resets_at"`
		}
		if err := json.Unmarshal(raw, &window); err != nil || window.UsedPercentage == nil {
			continue
		}

		used := *window.UsedPercentage
		var resetsAt *time.Time
		if window.ResetsAt != nil && *window.ResetsAt != 0 {
			reset := time.Unix(*window.ResetsAt, 0)
			resetsAt = &reset
			if reset.Before(now) {
				used = 0
			}
		}

		status.Windows = append(status.Windows, usage.QuotaWindow{
			Name:        windowName(key),
			UsedPercent: used,
			ResetsAt:    resetsAt,
		})
	}

	sort.SliceStable(status.Windows, func(i, j int) bool {
		left, right := quotaWindowRank(status.Windows[i].Name), quotaWindowRank(status.Windows[j].Name)
		if left != right {
			return left < right
		}
		return status.Windows[i].Name < status.Windows[j].Name
	})
	return status
}

func quotaWindowRank(name string) int {
	switch name {
	case "5h":
		return 0
	case "7d":
		return 1
	default:
		return 2
	}
}
