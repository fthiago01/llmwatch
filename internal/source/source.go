// Package source reads raw LLM usage records from local tool transcript files.
//
// A Source knows how to enumerate the transcript files of one tool, how to
// turn them into usage records and how to cheaply detect that the underlying
// files changed.
package source

import (
	"context"

	"llmwatch/internal/usage"
)

// Source is a provider of raw usage records.
//
// Load returns every record known by the source (the accumulated state of the
// source, not just the records added since the previous call). Fingerprint
// returns a cheap-to-compute token that changes whenever the source files
// change; it may be used to skip a Load when nothing moved.
type Source interface {
	// Name returns the stable identifier of the source, e.g. "claude".
	Name() string
	// Load returns all known records of the source.
	Load(ctx context.Context) ([]usage.Record, error)
	// Fingerprint returns a token that changes when the source files change.
	Fingerprint() (string, error)
}
