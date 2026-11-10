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
)

// claudeEntry is the subset of a transcript line that carries usage data.
type claudeEntry struct {
	Timestamp string `json:"timestamp"`
	Message   struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
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
		Cost:       0,
	}, entry.Message.ID
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
