package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"llmwatch/internal/usage"
)

// DefaultMiniMaxURL is the endpoint reporting the remaining quota of the
// MiniMax M Plan (token plan).
const DefaultMiniMaxURL = "https://www.minimax.io/v1/token_plan/remains"

// MiniMax reads quota information from the MiniMax token plan API.
type MiniMax struct {
	key    string
	url    string
	client *http.Client
}

// NewMiniMax creates a MiniMax quota provider. An empty url falls back to
// DefaultMiniMaxURL and a nil client falls back to a default HTTP client.
func NewMiniMax(key, url string, client *http.Client) *MiniMax {
	if url == "" {
		url = DefaultMiniMaxURL
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &MiniMax{key: key, url: url, client: client}
}

// Name identifies the quota provider.
func (m *MiniMax) Name() string {
	return "minimax"
}

// Interval is the frequency at which the MiniMax API is queried.
func (m *MiniMax) Interval() time.Duration {
	return 60 * time.Second
}

// Fetch reads the remaining quota of the MiniMax M Plan.
func (m *MiniMax) Fetch(ctx context.Context) usage.QuotaStatus {
	status := usage.QuotaStatus{
		Provider: "minimax-coding-plan",
		Source:   "minimax-api",
		Windows:  []usage.QuotaWindow{},
	}

	if m.key == "" {
		status.Error = "defina LLMWATCH_MINIMAX_KEY com a chave do M Plan (Plan Details em platform.minimax.io)"
		return status
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.url, nil)
	if err != nil {
		status.Error = fmt.Sprintf("minimax: %v", err)
		return status
	}
	request.Header.Set("Authorization", "Bearer "+m.key)
	request.Header.Set("Content-Type", "application/json")

	response, err := m.client.Do(request)
	if err != nil {
		status.Error = fmt.Sprintf("minimax: %v", err)
		return status
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		status.Error = fmt.Sprintf("minimax: %v", err)
		return status
	}

	if response.StatusCode != http.StatusOK {
		status.Error = fmt.Sprintf("minimax: HTTP %d: %s", response.StatusCode, snippet(body, 200))
		return status
	}

	var payload minimaxResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		status.Error = fmt.Sprintf("minimax: resposta inválida: %v", err)
		return status
	}

	if payload.BaseResp.StatusCode != 0 {
		message := fmt.Sprintf("minimax: %s (código %d)", payload.BaseResp.StatusMsg, payload.BaseResp.StatusCode)
		if payload.BaseResp.StatusCode == 2049 {
			message += " — use a chave do M Plan, não a de pay-as-you-go"
		}
		status.Error = message
		return status
	}

	now := time.Now()
	windows := make([]usage.QuotaWindow, 0, len(payload.ModelRemains)*2)
	for _, model := range payload.ModelRemains {
		windows = append(windows, minimaxWindow("5h", model, now)...)
		windows = append(windows, minimaxWindow("semana", model, now)...)
	}

	sort.SliceStable(windows, func(i, j int) bool {
		return windows[i].Name < windows[j].Name
	})

	status.Windows = windows
	status.UpdatedAt = now
	return status
}

// minimaxWindow builds the quota window for a MiniMax model. The label
// selects which pair of counters is used: the 5h one or the weekly one.
//
// The API may report the window in two ways: with absolute counters
// (current_interval_total_count and friends) or only with the remaining
// percentage. Windows without any information are skipped.
func minimaxWindow(label string, model minimaxModel, now time.Time) []usage.QuotaWindow {
	total := model.IntervalTotal
	usageCount := model.IntervalUsage
	status := model.IntervalStatus
	remainingPercent := model.IntervalRemainingPercent
	endTime := model.EndTime
	remains := model.RemainsTime
	if label == "semana" {
		total = model.WeeklyTotal
		usageCount = model.WeeklyUsage
		status = model.WeeklyStatus
		remainingPercent = model.WeeklyRemainingPercent
		endTime = model.WeeklyEndTime
		remains = model.WeeklyRemainsTime
	}

	// A status of 3 means the window does not apply to this model.
	if int64(status) == 3 {
		return nil
	}

	var used, limit int64
	var usedPercent float64
	switch {
	case remainingPercent != nil:
		// The API reports how much of the window is left; the consumed
		// percentage is its complement.
		usedPercent = 100 - float64(*remainingPercent)
		if usedPercent < 0 {
			usedPercent = 0
		}
		if usedPercent > 100 {
			usedPercent = 100
		}
		// The absolute counters are only filled when the API sends them.
		if int64(total) > 0 {
			used = int64(total) - int64(usageCount)
			if used < 0 {
				used = 0
			}
			limit = int64(total)
		}
	case int64(total) > 0:
		// The API reports the remaining usage in the *_usage_count fields;
		// the consumed amount is the difference against the total.
		used = int64(total) - int64(usageCount)
		if used < 0 {
			used = 0
		}
		limit = int64(total)
		usedPercent = float64(used) * 100 / float64(total)
	default:
		return nil
	}

	var resetsAt *time.Time
	switch {
	case int64(endTime) > 0:
		reset := time.UnixMilli(int64(endTime))
		resetsAt = &reset
	case int64(remains) > 0:
		reset := now.Add(time.Duration(int64(remains)) * time.Millisecond)
		resetsAt = &reset
	}

	return []usage.QuotaWindow{{
		Name:        minimaxWindowName(label, model.ModelName),
		UsedPercent: usedPercent,
		ResetsAt:    resetsAt,
		Used:        used,
		Limit:       limit,
	}}
}

// minimaxWindowName labels a window. The "general" entry carries the quota
// of the plan itself, so it is named without a model suffix.
func minimaxWindowName(label, model string) string {
	if model == "" || model == "general" {
		return label
	}
	return label + " · " + model
}

// minimaxResponse is the payload returned by the token plan endpoint.
type minimaxResponse struct {
	BaseResp struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
	ModelRemains []minimaxModel `json:"model_remains"`
}

// minimaxModel describes the quota of a single model. The counters may be
// encoded either as numbers or as strings, so they are decoded tolerantly.
type minimaxModel struct {
	ModelName string `json:"model_name"`

	IntervalTotal  minimaxNumber `json:"current_interval_total_count"`
	IntervalUsage  minimaxNumber `json:"current_interval_usage_count"`
	IntervalStatus minimaxNumber `json:"current_interval_status"`
	// IntervalRemainingPercent is a pointer so that an absent field can be
	// told apart from a zero value.
	IntervalRemainingPercent *minimaxPercent `json:"current_interval_remaining_percent"`

	WeeklyTotal            minimaxNumber   `json:"current_weekly_total_count"`
	WeeklyUsage            minimaxNumber   `json:"current_weekly_usage_count"`
	WeeklyStatus           minimaxNumber   `json:"current_weekly_status"`
	WeeklyRemainingPercent *minimaxPercent `json:"current_weekly_remaining_percent"`

	EndTime           minimaxNumber `json:"end_time"`
	RemainsTime       minimaxNumber `json:"remains_time"`
	WeeklyEndTime     minimaxNumber `json:"weekly_end_time"`
	WeeklyRemainsTime minimaxNumber `json:"weekly_remains_time"`
}

// minimaxNumber decodes a JSON number that may also be sent as a string.
type minimaxNumber int64

// UnmarshalJSON accepts both numbers and quoted numbers.
func (n *minimaxNumber) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "" || text == "null" {
		*n = 0
		return nil
	}
	if strings.HasPrefix(text, `"`) {
		var raw string
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			*n = 0
			return nil
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			// Tolerate fractional values by truncating them.
			float, ferr := strconv.ParseFloat(raw, 64)
			if ferr != nil {
				return err
			}
			*n = minimaxNumber(int64(float))
			return nil
		}
		*n = minimaxNumber(value)
		return nil
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return err
	}
	*n = minimaxNumber(int64(value))
	return nil
}

// minimaxPercent decodes a JSON percentage that may also be sent as a
// string.
type minimaxPercent float64

// UnmarshalJSON accepts both numbers and quoted numbers.
func (p *minimaxPercent) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "" || text == "null" {
		*p = 0
		return nil
	}
	if strings.HasPrefix(text, `"`) {
		var raw string
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			*p = 0
			return nil
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return err
		}
		*p = minimaxPercent(value)
		return nil
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return err
	}
	*p = minimaxPercent(value)
	return nil
}

// snippet truncates body to at most limit bytes, for error messages.
func snippet(body []byte, limit int) string {
	text := strings.TrimSpace(string(body))
	if len(text) > limit {
		return text[:limit] + "…"
	}
	return text
}
