// Package quota collects subscription quota information from providers.
package quota

import (
	"context"
	"strings"
	"time"

	"llmwatch/internal/usage"
)

// Provider is a source of quota information for a provider.
type Provider interface {
	// Name identifies the quota provider.
	Name() string
	// Interval is how often the provider should be queried.
	Interval() time.Duration
	// Fetch reads the current quota. Failures are reported in
	// QuotaStatus.Error rather than returned as an error.
	Fetch(ctx context.Context) usage.QuotaStatus
}

// windowName converts a provider-specific window key into a display name.
func windowName(key string) string {
	switch key {
	case "five_hour":
		return "5h"
	case "seven_day":
		return "7d"
	case "seven_day_opus":
		return "7d opus"
	case "seven_day_sonnet":
		return "7d sonnet"
	default:
		return strings.ReplaceAll(key, "_", " ")
	}
}
