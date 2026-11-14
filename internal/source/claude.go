package source

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"llmwatch/internal/usage"
)

const (
	// claudeName is the source identifier.
	claudeName = "claude"
	// claudeProvider is the provider attributed to every claude record.
	claudeProvider = "anthropic"
	// syntheticModel marks locally generated (non API) messages.
	syntheticModel = "<synthetic>"
	// claudeWebSearchPrice is the price, in US dollars, of a single web
	// search request performed by the server side web search tool.
	claudeWebSearchPrice = 0.01
	// claudeHaikuLongContextTokens is the context size above which the
	// long context pricing of claude-haiku-5-5 applies.
	claudeHaikuLongContextTokens = 100000
)

// claudePricing holds the price, in US dollars per million tokens, of the
// token classes of a model.
//
// The order of the fields is [input, cacheWrite5m, cacheWrite1h,
// cacheRead, output].
type claudePricing struct {
	Input        float64
	CacheWrite5m float64
	CacheWrite1h float64
	CacheRead    float64
	Output       float64
}

// claudePrices maps a normalized model id to its pricing.
//
// A model that is not listed here has no known price: its cost is reported
// as zero instead of being guessed.
var claudePrices = map[string]claudePricing{
	"claude-fable-5-1":  {Input: 10, CacheWrite5m: 12.5, CacheWrite1h: 20, CacheRead: 0.25, Output: 50},
	"claude-fable-5":    {Input: 10, CacheWrite5m: 12.5, CacheWrite1h: 20, CacheRead: 1, Output: 50},
	"claude-opus-5-5":   {Input: 4, CacheWrite5m: 5, CacheWrite1h: 8, CacheRead: 0.2, Output: 20},
	"claude-opus-5":     {Input: 5, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.5, Output: 25},
	"claude-opus-4-8":   {Input: 5, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.5, Output: 25},
	"claude-opus-4-7":   {Input: 5, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.5, Output: 25},
	"claude-opus-4-6":   {Input: 5, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.5, Output: 25},
	"claude-opus-4-5":   {Input: 5, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.5, Output: 25},
	"claude-sonnet-5-5": {Input: 2, CacheWrite5m: 2.5, CacheWrite1h: 4, CacheRead: 0.1, Output: 10},
	"claude-sonnet-5":   {Input: 2, CacheWrite5m: 2.5, CacheWrite1h: 4, CacheRead: 0.2, Output: 10},
	"claude-sonnet-4-6": {Input: 3, CacheWrite5m: 3.75, CacheWrite1h: 6, CacheRead: 0.3, Output: 15},
	"claude-sonnet-4-5": {Input: 3, CacheWrite5m: 3.75, CacheWrite1h: 6, CacheRead: 0.3, Output: 15},
	"claude-haiku-5-5":  {Input: 0.1, CacheWrite5m: 0.125, CacheWrite1h: 0.2, CacheRead: 0.01, Output: 0.5},
	"claude-haiku-4-5":  {Input: 1, CacheWrite5m: 1.25, CacheWrite1h: 2, CacheRead: 0.1, Output: 5},
}

// claudeHaikuLongContextPricing is the pricing of claude-haiku-5-5 when the
// context of the request exceeds claudeHaikuLongContextTokens.
var claudeHaikuLongContextPricing = claudePricing{
	Input: 0.5, CacheWrite5m: 0.625, CacheWrite1h: 1, CacheRead: 0.05, Output: 2.5,
}

// claudeUsage is the usage payload of a transcript message.
//
// The cache creation tokens are split by TTL when the transcript provides
// the breakdown; when it does not, the whole cache creation count is
// considered a 5 minute write.
type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	// CacheCreation splits the cache creation tokens by TTL.
	CacheCreation *struct {
		Ephemeral5mInputTokens *int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1hInputTokens *int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	// Speed describes the serving tier of the request, e.g. "fast".
	Speed string `json:"speed"`
	// ServerToolUse holds the usage of the server side tools.
	ServerToolUse *struct {
		WebSearchRequests int64 `json:"web_search_requests"`
	} `json:"server_tool_use"`
}

// claudeEntry is the subset of a transcript line that carries usage data.
type claudeEntry struct {
	Timestamp string `json:"timestamp"`
	// SessionID identifies the session the line belongs to. It is empty
	// when the line does not carry one.
	SessionID string `json:"sessionId"`
	// Cwd is the working directory of the session. It is empty when the
	// line does not carry one.
	Cwd     string `json:"cwd"`
	Message struct {
		ID    string       `json:"id"`
		Model string       `json:"model"`
		Usage *claudeUsage `json:"usage"`
	} `json:"message"`
}

// claudeFileState tracks the incremental read progress of one transcript file.
type claudeFileState struct {
	offset int64
	// records holds the deduplicated records of the file, keyed by message id.
	// Records without an id are stored under a synthetic unique key.
	records map[string]usage.Record
	// order keeps the insertion order of keys so the file state can be
	// rebuilt deterministically when a file is re-read from scratch.
	order []string
	// nextSeq generates unique keys for records without a message id.
	nextSeq int64
}

// Claude is a Source that reads Claude Code transcript files (.jsonl) stored
// under a root directory such as ~/.claude/projects.
//
// Loading is incremental: every file keeps the byte offset that was already
// consumed, so subsequent loads only read the newly appended data. Records are
// deduplicated by message id, keeping the last value seen for each id.
type Claude struct {
	root string

	mu    sync.Mutex
	files map[string]*claudeFileState
	// order preserves the order in which files were first seen.
	fileOrder []string
}

// NewClaude returns a Claude source reading transcripts under root.
//
// The typical root is ~/.claude/projects.
func NewClaude(root string) *Claude {
	return &Claude{
		root:  root,
		files: map[string]*claudeFileState{},
	}
}

// Name returns the source identifier.
func (c *Claude) Name() string { return claudeName }

// claudeGlobs returns the glob patterns matching every transcript file.
func claudeGlobs(root string) []string {
	return []string{
		filepath.Join(root, "*", "*.jsonl"),
		filepath.Join(root, "*", "*", "subagents", "*.jsonl"),
	}
}

// listFiles returns the sorted list of transcript files under root.
func (c *Claude) listFiles() ([]string, error) {
	if c.root == "" {
		return nil, nil
	}
	if fi, err := os.Stat(c.root); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat %s: %w", c.root, err)
	} else if !fi.IsDir() {
		return nil, nil
	}

	seen := map[string]bool{}
	var files []string
	for _, pattern := range claudeGlobs(c.root) {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("glob %s: %w", pattern, err)
		}
		for _, m := range matches {
			if seen[m] {
				continue
			}
			seen[m] = true
			files = append(files, m)
		}
	}
	sort.Strings(files)
	return files, nil
}

// Fingerprint returns a token that changes whenever any transcript file
// changes. It only stats the files, so it is cheap.
func (c *Claude) Fingerprint() (string, error) {
	files, err := c.listFiles()
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", nil
	}

	var (
		count     int
		totalSize int64
		maxMtime  int64
	)
	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", fmt.Errorf("stat %s: %w", f, err)
		}
		if fi.IsDir() {
			continue
		}
		count++
		totalSize += fi.Size()
		if mt := fi.ModTime().UnixNano(); mt > maxMtime {
			maxMtime = mt
		}
	}

	if count == 0 {
		return "", nil
	}

	return strconv.Itoa(count) + ":" + strconv.FormatInt(totalSize, 10) + ":" + strconv.FormatInt(maxMtime, 10), nil
}

// Load returns all records known by the source, reading only the data that
// was appended since the last call.
func (c *Claude) Load(ctx context.Context) ([]usage.Record, error) {
	files, err := c.listFiles()
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, path := range files {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("load %s: %w", c.root, err)
		}
		if err := c.loadFile(path); err != nil {
			return nil, err
		}
	}

	return c.records()
}

// loadFile reads the new content of one transcript file into its state.
func (c *Claude) loadFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	st := c.files[path]
	if st == nil {
		st = &claudeFileState{records: map[string]usage.Record{}}
		c.files[path] = st
		c.fileOrder = append(c.fileOrder, path)
	} else if fi.Size() < st.offset {
		// The file was truncated or recreated: drop everything we knew
		// about it and read it again from the beginning.
		st.offset = 0
		st.records = map[string]usage.Record{}
		st.order = nil
	}

	if _, err := f.Seek(st.offset, io.SeekStart); err != nil {
		return fmt.Errorf("seek %s: %w", path, err)
	}

	reader := bufio.NewReaderSize(f, 64*1024)
	var consumed int64
	for {
		line, err := readLine(reader)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if line == nil {
			// The last line is incomplete: it is not consumed and will be
			// read again on the next load.
			break
		}
		consumed += int64(len(line)) + 1 // +1 for the newline
		rec, id := parseClaudeLine(line)
		st.add(rec, id)
	}
	st.offset += consumed

	return nil
}

// readLine reads a single line from reader, without the trailing newline.
//
// It returns (nil, io.EOF) when the stream ends without a complete line, so
// that partial writes are left for a later read. Lines of any size are
// supported.
func readLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			// The trailing content has no terminating newline: it is an
			// incomplete line and must not be consumed.
			return nil, io.EOF
		}
		return nil, err
	}
	line = line[:len(line)-1] // strip the '\n'
	return []byte(line), nil
}

// parseClaudeLine converts a transcript line into a record.
//
// The second return value is the message id used for deduplication; it is
// empty when the line has no message id. The record is the zero Record when
// the line carries no usage information.
func parseClaudeLine(line []byte) (usage.Record, string) {
	line = []byte(strings.TrimSpace(string(line)))
	if len(line) == 0 {
		return usage.Record{}, ""
	}

	var entry claudeEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		return usage.Record{}, ""
	}
	if entry.Message.Usage == nil {
		return usage.Record{}, ""
	}
	if entry.Message.Model == syntheticModel {
		return usage.Record{}, ""
	}

	ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
	if err != nil {
		ts = time.Time{}
	}

	return usage.Record{
		Source:     claudeName,
		Provider:   claudeProvider,
		Model:      entry.Message.Model,
		Time:       ts,
		Input:      entry.Message.Usage.InputTokens,
		Output:     entry.Message.Usage.OutputTokens,
		CacheWrite: entry.Message.Usage.CacheCreationInputTokens,
		CacheRead:  entry.Message.Usage.CacheReadInputTokens,
		Cost:       claudeCost(entry.Message.Model, entry.Message.Usage),
		Session:    entry.SessionID,
		Project:    entry.Cwd,
	}, entry.Message.ID
}

// claudeCost estimates, in US dollars, the cost of one Claude Code
// response from its usage counters.
//
// The model id is normalized before the lookup, so "claude-opus-5-5[1m]"
// and "claude-opus-5-5-20260101" are priced as "claude-opus-5-5". A model
// with no known price, or a request served with the "fast" speed, has no
// estimable cost and yields zero.
func claudeCost(model string, u *claudeUsage) float64 {
	if u == nil {
		return 0
	}
	if strings.EqualFold(strings.TrimSpace(u.Speed), "fast") {
		// The fast speed is not covered by the price table.
		return 0
	}

	name := claudeNormalizeModel(model)
	pricing, ok := claudePrices[name]
	if !ok {
		// The model is unknown: the cost is not guessed.
		return 0
	}

	write5m, write1h := claudeCacheWrites(u)
	if name == "claude-haiku-5-5" &&
		u.InputTokens+u.CacheReadInputTokens+write5m+write1h > claudeHaikuLongContextTokens {
		pricing = claudeHaikuLongContextPricing
	}

	cost := float64(u.InputTokens)*pricing.Input +
		float64(write5m)*pricing.CacheWrite5m +
		float64(write1h)*pricing.CacheWrite1h +
		float64(u.CacheReadInputTokens)*pricing.CacheRead +
		float64(u.OutputTokens)*pricing.Output
	cost /= 1e6

	if u.ServerToolUse != nil {
		cost += float64(u.ServerToolUse.WebSearchRequests) * claudeWebSearchPrice
	}
	return cost
}

// claudeCacheWrites returns the number of tokens written to the cache with
// a 5 minute and with a 1 hour TTL.
//
// When the transcript carries the TTL breakdown it is used as is;
// otherwise the whole cache creation count is billed as a 5 minute write.
func claudeCacheWrites(u *claudeUsage) (int64, int64) {
	if u.CacheCreation == nil {
		return u.CacheCreationInputTokens, 0
	}
	if u.CacheCreation.Ephemeral5mInputTokens == nil && u.CacheCreation.Ephemeral1hInputTokens == nil {
		return u.CacheCreationInputTokens, 0
	}
	var write5m, write1h int64
	if u.CacheCreation.Ephemeral5mInputTokens != nil {
		write5m = *u.CacheCreation.Ephemeral5mInputTokens
	}
	if u.CacheCreation.Ephemeral1hInputTokens != nil {
		write1h = *u.CacheCreation.Ephemeral1hInputTokens
	}
	return write5m, write1h
}

// claudeNormalizeModel normalizes a model id so it can be looked up in the
// price table.
//
// It removes the bracketed suffix used by the long context variants
// ("claude-opus-5-5[1m]") and the date suffix of the snapshots
// ("claude-opus-5-5-20260101").
func claudeNormalizeModel(model string) string {
	model = strings.TrimSpace(model)
	if i := strings.IndexByte(model, '['); i >= 0 {
		model = model[:i]
	}
	// The date suffix is the last dash separated group, made of 8 digits.
	if i := strings.LastIndexByte(model, '-'); i >= 0 && i+1 < len(model) {
		if isAllDigits(model[i+1:]) && len(model)-i-1 == 8 {
			model = model[:i]
		}
	}
	return model
}

// isAllDigits reports whether s is made of decimal digits only.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// add stores a record in the file state.
//
// Records carrying a message id are deduplicated by that id, keeping the last
// value seen. Records without an id are always kept.
func (s *claudeFileState) add(rec usage.Record, id string) {
	if rec.Source == "" {
		return
	}
	key := id
	if key == "" {
		key = "\x00" + strconv.FormatInt(s.nextSeq, 10)
		s.nextSeq++
	}
	if _, ok := s.records[key]; !ok {
		s.order = append(s.order, key)
	}
	s.records[key] = rec
}

// records returns every known record, sorted by time.
func (c *Claude) records() ([]usage.Record, error) {
	out := []usage.Record{}
	for _, path := range c.fileOrder {
		st := c.files[path]
		if st == nil {
			continue
		}
		for _, id := range st.order {
			if rec, ok := st.records[id]; ok {
				out = append(out, rec)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Time.Equal(out[j].Time) {
			return out[i].Time.Before(out[j].Time)
		}
		return out[i].Model < out[j].Model
	})
	return out, nil
}
