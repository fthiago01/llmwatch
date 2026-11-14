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

// dailyDays is the number of daily buckets kept in a snapshot.
const dailyDays = 84

// sessionDays is the number of days a session is kept in a snapshot.
const sessionDays = 30

// maxSessions is the maximum number of sessions reported in a snapshot.
const maxSessions = 200

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
	Session    string    `json:"session,omitempty"` // session/thread id, "" when unknown
	Project    string    `json:"project,omitempty"` // working directory of the session, "" when unknown
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

// Day holds the usage of one calendar day, split by provider.
type Day struct {
	// Date is the day in the "2006-01-02" format, in the timezone of the
	// snapshot.
	Date string `json:"date"`
	// Totals holds the aggregated counters of the day.
	Totals
	// ByProvider holds the tokens spent per provider; it is never nil.
	ByProvider map[string]int64 `json:"by_provider"`
	// ByProject holds the tokens spent per project; it is never nil. Records
	// without a known project are not counted.
	ByProject map[string]int64 `json:"by_project"`
}

// Session summarizes the usage of a single session/thread.
type Session struct {
	// ID is the session identifier, unique per source.
	ID string `json:"id"`
	// Project is the working directory of the session, taken from the first
	// record with a known project; "" when unknown.
	Project string `json:"project"`
	// Provider is the provider that spent the most tokens in the session.
	Provider string `json:"provider"`
	// Model is the model that spent the most tokens in the session; ties are
	// broken alphabetically.
	Model string `json:"model"`
	// Start is the time of the oldest record of the session.
	Start time.Time `json:"start"`
	// End is the time of the newest record of the session.
	End time.Time `json:"end"`
	// Totals holds the aggregated counters of the session; Requests counts
	// the number of records of the session.
	Totals
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

// UpdateInfo describes the update status of the running binary.
type UpdateInfo struct {
	// Current is the version in use, e.g. "v0.2.2" or "dev".
	Current string `json:"current"`
	// Latest is the tag of the most recent release, e.g. "v0.2.3".
	Latest string `json:"latest,omitempty"`
	// Available tells whether Latest is newer than Current.
	Available bool `json:"available"`
	// URL is the html_url of the release.
	URL string `json:"url,omitempty"`
	// CheckedAt is the moment of the last check.
	CheckedAt time.Time `json:"checked_at"`
	// Error describes a failure during the check, when any.
	Error string `json:"error,omitempty"`
}

// Snapshot is the aggregated view of a set of usage records.
type Snapshot struct {
	GeneratedAt time.Time     `json:"generated_at"`
	Windows     []Window      `json:"windows"`
	Blocks      []Block       `json:"blocks"`
	Hourly      []Bucket      `json:"hourly"`
	Daily       []Day         `json:"daily"`
	Sessions    []Session     `json:"sessions"`
	Quotas      []QuotaStatus `json:"quotas"`
	Errors      []string      `json:"errors"`
	// Update holds the update status, filled by the server and not by the
	// aggregator.
	Update *UpdateInfo `json:"update,omitempty"`
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
		Daily:       buildDaily(valid, now),
		Sessions:    buildSessions(valid, now),
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

// buildDaily splits the records into the last dailyDays calendar days, from
// the oldest to the current one.
//
// The days are computed from the local midnight of now, so the timezone and
// the daylight saving changes of the local calendar are respected.
func buildDaily(recs []Record, now time.Time) []Day {
	loc := now.Location()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	days := make([]Day, dailyDays)
	byDate := make(map[string]int, dailyDays)
	for i := range days {
		day := today.AddDate(0, 0, i-(dailyDays-1))
		days[i] = Day{
			Date:       day.Format("2006-01-02"),
			ByProvider: map[string]int64{},
			ByProject:  map[string]int64{},
		}
		byDate[days[i].Date] = i
	}

	for _, r := range recs {
		local := r.Time.In(loc)
		date := local.Format("2006-01-02")
		i, ok := byDate[date]
		if !ok {
			// The record is in the future or older than the oldest day.
			continue
		}
		days[i].Totals.Add(r)
		days[i].ByProvider[r.Provider] += r.tokens()
		if r.Project != "" {
			days[i].ByProject[r.Project] += r.tokens()
		}
	}
	return days
}

// sessionKey identifies a session within a source.
type sessionKey struct {
	source  string
	session string
}

// sessionAcc accumulates the data of a session while it is being built.
type sessionAcc struct {
	session  Session
	byModel  map[string]int64
	byProv   map[string]int64
	hasStart bool
}

// buildSessions groups the records by (source, session) and summarizes the
// sessions that are still recent.
//
// Records without a known session are ignored. Sessions whose last record is
// older than sessionDays are dropped. The result is sorted by the end of the
// session, most recent first, and truncated to maxSessions entries; the
// returned slice is never nil.
func buildSessions(recs []Record, now time.Time) []Session {
	accs := map[sessionKey]*sessionAcc{}

	for _, r := range recs {
		if r.Session == "" {
			// The session is unknown: it cannot be grouped.
			continue
		}
		key := sessionKey{source: r.Source, session: r.Session}
		acc, ok := accs[key]
		if !ok {
			acc = &sessionAcc{
				session: Session{ID: r.Session},
				byModel: map[string]int64{},
				byProv:  map[string]int64{},
			}
			accs[key] = acc
		}

		if !acc.hasStart || r.Time.Before(acc.session.Start) {
			acc.session.Start = r.Time
			acc.hasStart = true
		}
		if acc.session.End.IsZero() || r.Time.After(acc.session.End) {
			acc.session.End = r.Time
		}
		if acc.session.Project == "" && r.Project != "" {
			acc.session.Project = r.Project
		}

		tokens := r.tokens()
		acc.byModel[r.Model] += tokens
		acc.byProv[r.Provider] += tokens
		acc.session.Totals.Add(r)
	}

	cutoff := now.AddDate(0, 0, -sessionDays)
	sessions := make([]Session, 0, len(accs))
	for _, acc := range accs {
		if acc.session.End.Before(cutoff) {
			// The session is too old to be reported.
			continue
		}
		s := acc.session
		s.Provider = dominant(acc.byProv)
		s.Model = dominant(acc.byModel)
		sessions = append(sessions, s)
	}

	sort.Slice(sessions, func(i, j int) bool {
		a, b := sessions[i], sessions[j]
		if !a.End.Equal(b.End) {
			return a.End.After(b.End)
		}
		return a.ID < b.ID
	})

	if len(sessions) > maxSessions {
		sessions = sessions[:maxSessions]
	}
	return sessions
}

// dominant returns the key with the highest counter; ties are broken
// alphabetically.
func dominant(counts map[string]int64) string {
	best := ""
	bestTokens := int64(0)
	found := false
	for key, tokens := range counts {
		switch {
		case !found || tokens > bestTokens:
			best, bestTokens, found = key, tokens, true
		case tokens == bestTokens && key < best:
			best = key
		}
	}
	return best
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
