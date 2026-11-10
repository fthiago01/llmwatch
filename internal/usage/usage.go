// Package usage aggregates raw LLM usage records into windowed summaries,
// hourly buckets and subscription-block usage information.
package usage

import (
	"sort"
	"time"
)

// blockDuration is the duration of a subscription usage block (the 5h block
// used by subscription plans).
const blockDuration = 5 * time.Hour

// hourlyBuckets is the number of hourly buckets kept in a snapshot.
const hourlyBuckets = 24

// Record is a single usage observation attributed to a provider model.
type Record struct {
	Source     string    `json:"source"`
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	Time       time.Time `json:"time"`
	Input      int64     `json:"input"`
	Output     int64     `json:"output"`
	CacheRead  int64     `json:"cache_read"`
	CacheWrite int64     `json:"cache_write"`
	Reasoning  int64     `json:"reasoning"`
	Cost       float64   `json:"cost"`
}

// tokens returns the total number of tokens accounted for by the record.
func (r Record) tokens() int64 {
	return r.Input + r.Output + r.CacheRead + r.CacheWrite + r.Reasoning
}

// Totals accumulates token counters, request count and cost.
type Totals struct {
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheRead  int64   `json:"cache_read"`
	CacheWrite int64   `json:"cache_write"`
	Reasoning  int64   `json:"reasoning"`
	Total      int64   `json:"total"`
	Requests   int64   `json:"requests"`
	Cost       float64 `json:"cost"`
}

// Add accumulates a single record into the totals.
func (t *Totals) Add(r Record) {
	t.Input += r.Input
	t.Output += r.Output
	t.CacheRead += r.CacheRead
	t.CacheWrite += r.CacheWrite
	t.Reasoning += r.Reasoning
	t.Total += r.tokens()
	t.Requests++
	t.Cost += r.Cost
}

// Row holds the totals of one (provider, model) pair.
type Row struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Totals
}

// Window is a named time window with per provider/model breakdown.
type Window struct {
	Name       string `json:"name"`
	ByProvider []Row  `json:"by_provider"`
	Sum        Totals `json:"sum"`
}

// Bucket holds the tokens of one hour, split by provider.
type Bucket struct {
	Hour       time.Time        `json:"hour"`
	ByProvider map[string]int64 `json:"by_provider"`
}

// Block describes the active usage block of a provider.
type Block struct {
	Provider string    `json:"provider"`
	Start    time.Time `json:"start"`
	ResetAt  time.Time `json:"reset_at"`
	Totals
	TokensPerMin float64 `json:"tokens_per_min"`
	Limit        int64   `json:"limit"`
	PercentUsed  float64 `json:"percent_used"`
}

// QuotaWindow describes the consumption of a single quota window of a
// provider subscription, e.g. "5h", "7d" or "5h · MiniMax-M3".
type QuotaWindow struct {
	// Name identifies the window, e.g. "5h" or "7d".
	Name string `json:"name"`
	// UsedPercent is the percentage of the window already consumed (0..100).
	UsedPercent float64 `json:"used_percent"`
	// ResetsAt is the moment the window resets, when known.
	ResetsAt *time.Time `json:"resets_at,omitempty"`
	// Used is the absolute consumption of the window, 0 when unknown.
	Used int64 `json:"used"`
	// Limit is the absolute ceiling of the window, 0 when unknown.
	Limit int64 `json:"limit"`
}

// Remaining returns the percentage of the window still available. The result
// is clamped to the [0,100] range.
func (w QuotaWindow) Remaining() float64 {
	remaining := 100 - w.UsedPercent
	switch {
	case remaining < 0:
		return 0
	case remaining > 100:
		return 100
	default:
		return remaining
	}
}

// QuotaStatus describes the quota consumption reported by an external source
// for a given provider.
type QuotaStatus struct {
	// Provider is the quota owner, e.g. "anthropic", "minimax-coding-plan"
	// or "openai".
	Provider string `json:"provider"`
	// Name is the registered instance name (e.g. "MiniMax pessoal").
	Name string `json:"name"`
	// Source is where the data came from, e.g. "claude-statusline",
	// "minimax-api" or "codex-app-server".
	Source string `json:"source"`
	// Plan is the subscription plan, when known.
	Plan string `json:"plan,omitempty"`
	// Windows holds the quota windows; it is never nil.
	Windows []QuotaWindow `json:"windows"`
	// UpdatedAt is the moment the quota was last refreshed.
	UpdatedAt time.Time `json:"updated_at"`
	// Error describes a failure while collecting the quota, when any.
	Error string `json:"error,omitempty"`
}

// Snapshot is the aggregated view of a set of usage records.
type Snapshot struct {
	GeneratedAt time.Time     `json:"generated_at"`
	Windows     []Window      `json:"windows"`
	Blocks      []Block       `json:"blocks"`
	Hourly      []Bucket      `json:"hourly"`
	Quotas      []QuotaStatus `json:"quotas"`
	Errors      []string      `json:"errors"`
}

// windowDef describes a named window and the inclusive/exclusive bounds used
// to select records: records are kept when start < Time <= now.
type windowDef struct {
	name  string
	start time.Time
}

// windowDefs returns the windows of a snapshot, in presentation order.
func windowDefs(now time.Time) []windowDef {
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return []windowDef{
		{name: "5h", start: now.Add(-5 * time.Hour)},
		{name: "hoje", start: midnight},
		{name: "7d", start: now.Add(-7 * 24 * time.Hour)},
		{name: "30d", start: now.Add(-30 * 24 * time.Hour)},
	}
}

// Aggregate builds a snapshot from the given records.
//
// Records newer than now are ignored. The snapshot always contains non-nil
// slices and maps, even when there is nothing to aggregate.
func Aggregate(recs []Record, now time.Time, limits map[string]int64) Snapshot {
	valid := make([]Record, 0, len(recs))
	for _, r := range recs {
		if r.Time.After(now) {
			continue
		}
		valid = append(valid, r)
	}

	defs := windowDefs(now)
	windows := make([]Window, 0, len(defs))
	for _, def := range defs {
		windows = append(windows, buildWindow(def, valid))
	}

	return Snapshot{
		GeneratedAt: now,
		Windows:     windows,
		Blocks:      buildBlocks(valid, now, limits),
		Hourly:      buildHourly(valid, now),
		// Quotas are filled by the server, not by the aggregator.
		Quotas: []QuotaStatus{},
		Errors: []string{},
	}
}

// rowKey identifies a (provider, model) pair.
type rowKey struct {
	provider string
	model    string
}

// buildWindow aggregates the records of a single window.
func buildWindow(def windowDef, recs []Record) Window {
	win := Window{Name: def.name, ByProvider: []Row{}}
	rows := map[rowKey]*Row{}

	for _, r := range recs {
		if !r.Time.After(def.start) {
			// The window is open on the left: start < Time.
			continue
		}
		win.Sum.Add(r)

		key := rowKey{provider: r.Provider, model: r.Model}
		row, ok := rows[key]
		if !ok {
			row = &Row{Provider: r.Provider, Model: r.Model}
			rows[key] = row
		}
		row.Totals.Add(r)
	}

	for _, row := range rows {
		win.ByProvider = append(win.ByProvider, *row)
	}
	sort.Slice(win.ByProvider, func(i, j int) bool {
		a, b := win.ByProvider[i], win.ByProvider[j]
		if a.Total != b.Total {
			return a.Total > b.Total
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Model < b.Model
	})
	return win
}

// buildHourly splits the records into the last 24 hourly buckets, from the
// oldest to the current one.
func buildHourly(recs []Record, now time.Time) []Bucket {
	loc := now.Location()
	nowTrunc := now.In(loc).Truncate(time.Hour)

	buckets := make([]Bucket, hourlyBuckets)
	for i := range buckets {
		buckets[i] = Bucket{
			Hour:       nowTrunc.Add(-time.Duration(hourlyBuckets-1-i) * time.Hour),
			ByProvider: map[string]int64{},
		}
	}

	for _, r := range recs {
		hour := r.Time.In(loc).Truncate(time.Hour)
		age := int(nowTrunc.Sub(hour) / time.Hour)
		if age < 0 || age >= len(buckets) {
			continue
		}
		buckets[len(buckets)-1-age].ByProvider[r.Provider] += r.tokens()
	}
	return buckets
}

// buildBlocks computes the active usage block of every provider.
func buildBlocks(recs []Record, now time.Time, limits map[string]int64) []Block {
	byProvider := map[string][]Record{}
	for _, r := range recs {
		byProvider[r.Provider] = append(byProvider[r.Provider], r)
	}

	providers := make([]string, 0, len(byProvider))
	for p := range byProvider {
		providers = append(providers, p)
	}
	sort.Strings(providers)

	blocks := []Block{}
	for _, provider := range providers {
		prs := byProvider[provider]
		sort.SliceStable(prs, func(i, j int) bool {
			return prs[i].Time.Before(prs[j].Time)
		})

		var (
			hasBlock bool
			start    time.Time
			totals   Totals
		)
		for _, r := range prs {
			switch {
			case !hasBlock:
				start = r.Time.UTC().Truncate(time.Hour)
				hasBlock = true
			case !r.Time.Before(start.Add(blockDuration)):
				// The record falls outside the current block: a new one starts.
				start = r.Time.UTC().Truncate(time.Hour)
				totals = Totals{}
			}
			totals.Add(r)
		}
		if !hasBlock {
			continue
		}

		resetAt := start.Add(blockDuration)
		if !now.Before(resetAt) {
			// The block expired: only the active block is reported.
			continue
		}

		limit := limits[provider]
		block := Block{
			Provider: provider,
			Start:    start,
			ResetAt:  resetAt,
			Totals:   totals,
			Limit:    limit,
		}

		minutes := now.Sub(start).Minutes()
		if minutes < 1 {
			minutes = 1
		}
		block.TokensPerMin = float64(totals.Total) / minutes
		if limit > 0 {
			block.PercentUsed = float64(totals.Total) * 100 / float64(limit)
		}

		blocks = append(blocks, block)
	}
	return blocks
}
