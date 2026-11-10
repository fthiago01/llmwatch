// Package update checks whether a newer release of llmwatch is available.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llmwatch/internal/usage"
)

// DefaultAPIURL is the endpoint returning the latest GitHub release.
const DefaultAPIURL = "https://api.github.com/repos/fthiago01/llmwatch/releases/latest"

// maxBodySize limits how much of the response body is read.
const maxBodySize = 1 << 20

// devVersion marks a development build, which never reports updates.
const devVersion = "dev"

// Checker queries the release API of the project.
type Checker struct {
	current string
	apiURL  string
	client  *http.Client
}

// New creates a Checker for the given current version. An empty apiURL falls
// back to DefaultAPIURL and a nil client falls back to a default HTTP client.
func New(current, apiURL string, client *http.Client) *Checker {
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &Checker{current: current, apiURL: apiURL, client: client}
}

// release is the subset of the release payload used by the checker.
type release struct {
	TagName    string `json:"tag_name"`
	HTMLURL    string `json:"html_url"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// Check queries the release API and reports the update status. Failures are
// reported in usage.UpdateInfo.Error instead of being returned as an error.
func (c *Checker) Check(ctx context.Context) usage.UpdateInfo {
	info := usage.UpdateInfo{
		Current:   c.current,
		CheckedAt: time.Now(),
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL, nil)
	if err != nil {
		info.Error = fmt.Sprintf("github: %v", err)
		return info
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "llmwatch/"+c.current)

	response, err := c.client.Do(request)
	if err != nil {
		info.Error = fmt.Sprintf("github: %v", err)
		return info
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxBodySize))
	if err != nil {
		info.Error = fmt.Sprintf("github: %v", err)
		return info
	}

	if response.StatusCode != http.StatusOK {
		info.Error = fmt.Sprintf("github: HTTP %d", response.StatusCode)
		if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests {
			info.Error = "github: limite de requisições do GitHub, tentando depois"
		}
		return info
	}

	var payload release
	if err := json.Unmarshal(body, &payload); err != nil {
		info.Error = fmt.Sprintf("github: resposta inválida: %v", err)
		return info
	}

	if payload.Draft || payload.Prerelease {
		return info
	}

	info.Latest = payload.TagName
	info.URL = payload.HTMLURL
	info.Available = Newer(payload.TagName, c.current)
	return info
}

// Newer reports whether latest is a newer version than current.
//
// Versions are simple MAJOR.MINOR.PATCH numbers with an optional "v" prefix
// and an optional pre-release suffix. A version with a suffix is older than
// the same version without one; suffixes are compared as plain strings. An
// invalid version, or a development build as current, never reports an
// update.
func Newer(latest, current string) bool {
	latestVersion, ok := parseVersion(latest)
	if !ok {
		return false
	}
	currentVersion, ok := parseVersion(current)
	if !ok {
		return false
	}
	return compareVersions(latestVersion, currentVersion) > 0
}

// version is a parsed semantic version.
type version struct {
	major  int
	minor  int
	patch  int
	pre    string
	hasPre bool
}

// parseVersion parses a MAJOR.MINOR.PATCH version, with an optional "v"
// prefix and an optional "-suffix" pre-release part.
func parseVersion(text string) (version, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return version{}, false
	}
	text = strings.TrimPrefix(text, "v")

	var pre string
	hasPre := false
	if index := strings.Index(text, "-"); index >= 0 {
		pre = text[index+1:]
		text = text[:index]
		hasPre = true
	}

	parts := strings.Split(text, ".")
	if len(parts) != 3 {
		return version{}, false
	}

	numbers := make([]int, 3)
	for i, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return version{}, false
		}
		numbers[i] = number
	}

	return version{
		major:  numbers[0],
		minor:  numbers[1],
		patch:  numbers[2],
		pre:    pre,
		hasPre: hasPre,
	}, true
}

// compareVersions compares two versions, returning a positive number when a
// is newer than b, zero when they are equal and a negative number otherwise.
func compareVersions(a, b version) int {
	if c := compareInt(a.major, b.major); c != 0 {
		return c
	}
	if c := compareInt(a.minor, b.minor); c != 0 {
		return c
	}
	if c := compareInt(a.patch, b.patch); c != 0 {
		return c
	}
	switch {
	case a.hasPre && b.hasPre:
		return strings.Compare(a.pre, b.pre)
	case a.hasPre:
		// A pre-release is older than the final release.
		return -1
	case b.hasPre:
		return 1
	default:
		return 0
	}
}

// compareInt compares two integers.
func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// Run checks for updates every interval, waiting initialDelay before the
// first check, and reports each result to notify. It returns when the context
// is cancelled.
//
// A development build never queries the API: notify is called once with the
// current version and no update available.
func (c *Checker) Run(ctx context.Context, interval time.Duration, initialDelay time.Duration, notify func(usage.UpdateInfo)) {
	if c.current == devVersion {
		notify(usage.UpdateInfo{
			Current:   devVersion,
			Available: false,
			CheckedAt: time.Now(),
		})
		return
	}

	if !wait(ctx, initialDelay) {
		return
	}

	for {
		notify(c.Check(ctx))

		if !wait(ctx, interval) {
			return
		}
	}
}

// wait sleeps for the given duration, returning false when the context is
// cancelled first.
func wait(ctx context.Context, duration time.Duration) bool {
	if duration <= 0 {
		return ctx.Err() == nil
	}

	timer := time.NewTimer(duration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
