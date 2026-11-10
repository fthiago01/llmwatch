package catalog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"llmwatch/internal/quota"
	"llmwatch/internal/source"
	"llmwatch/internal/store"
	"llmwatch/internal/usage"
)

// Built holds the sources and quota providers of the enabled providers.
//
// The slices are never nil.
type Built struct {
	Sources []source.Source
	Quotas  []quota.Provider
}

// namedSource is a source that reports the registered name of the provider
// instead of the name of its implementation.
type namedSource struct {
	source.Source
	name string
}

// Name returns the registered name of the provider.
func (s *namedSource) Name() string { return s.name }

// namedQuota is a quota provider that reports the registered name of the
// provider instead of the name of its implementation.
type namedQuota struct {
	quota.Provider
	name string
}

// Name returns the registered name of the provider.
func (q *namedQuota) Name() string { return q.name }

// Fetch reads the quota and stamps the registered name on the status.
func (q *namedQuota) Fetch(ctx context.Context) usage.QuotaStatus {
	status := q.Provider.Fetch(ctx)
	status.Name = q.name
	return status
}

// Build turns the stored providers into the sources and quota providers they
// describe.
//
// Only the enabled providers are built. A provider of an unknown type, or
// with an invalid configuration, is skipped and reported in the returned
// slice of error messages, formatted as "<nome>: <motivo>".
func Build(env Env, ps []store.Provider) (Built, []string) {
	built := Built{
		Sources: []source.Source{},
		Quotas:  []quota.Provider{},
	}
	var errs []string

	for _, p := range ps {
		if !p.Enabled {
			continue
		}

		t, ok := Lookup(env, p.Type)
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: tipo desconhecido %q", p.Name, p.Type))
			continue
		}

		if err := Validate(t, p.Config); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", p.Name, err))
			continue
		}

		cfg := Normalize(t, p.Config)

		switch t.ID {
		case TypeClaudeCodeLogs:
			built.Sources = append(built.Sources, &namedSource{
				Source: source.NewClaude(cfg["path"]),
				name:   p.Name,
			})
		case TypeOpencodeDB:
			built.Sources = append(built.Sources, &namedSource{
				Source: source.NewOpencode(cfg["path"]),
				name:   p.Name,
			})
		case TypeCodexDB:
			built.Sources = append(built.Sources, &namedSource{
				Source: source.NewCodex(cfg["path"]),
				name:   p.Name,
			})
		case TypeClaudeStatus:
			built.Quotas = append(built.Quotas, &namedQuota{
				Provider: quota.NewClaude(cfg["path"]),
				name:     p.Name,
			})
		case TypeMiniMaxPlan:
			built.Quotas = append(built.Quotas, &namedQuota{
				Provider: quota.NewMiniMax(cfg["api_key"], cfg["url"], nil),
				name:     p.Name,
			})
		case TypeCodexAppServer:
			built.Quotas = append(built.Quotas, &namedQuota{
				Provider: quota.NewCodex(cfg["bin"]),
				name:     p.Name,
			})
		default:
			errs = append(errs, fmt.Sprintf("%s: tipo desconhecido %q", p.Name, p.Type))
		}
	}

	return built, errs
}

// Detect suggests the providers available on the machine.
//
// The suggestions are meant for the first execution: every detected provider
// is enabled and configured with the default value of its fields.
//
// lookPath resolves an executable name and getenv reads an environment
// variable; both may be replaced to make the detection testable.
func Detect(env Env, lookPath func(string) (string, error), getenv func(string) string) []store.Provider {
	types := Types(env)
	byID := make(map[string]Type, len(types))
	for _, t := range types {
		byID[t.ID] = t
	}

	var out []store.Provider

	// The usage sources are suggested when their default path exists.
	for _, id := range []string{TypeClaudeCodeLogs, TypeOpencodeDB, TypeCodexDB} {
		t := byID[id]
		path := t.Fields[0].Default
		if !exists(path) {
			continue
		}
		out = append(out, store.Provider{
			Type:    t.ID,
			Name:    t.Label,
			Enabled: true,
			Config:  map[string]string{"path": path},
		})
	}

	// The Claude status line is suggested when Claude Code is installed.
	if exists(filepath.Join(env.Home, ".claude")) {
		t := byID[TypeClaudeStatus]
		out = append(out, store.Provider{
			Type:    t.ID,
			Name:    t.Label,
			Enabled: true,
			Config:  map[string]string{"path": t.Fields[0].Default},
		})
	}

	// The Codex app-server is suggested when the codex CLI is on the PATH.
	if lookPath != nil {
		if bin, err := lookPath("codex"); err == nil && bin != "" {
			t := byID[TypeCodexAppServer]
			out = append(out, store.Provider{
				Type:    t.ID,
				Name:    t.Label,
				Enabled: true,
				Config:  map[string]string{"bin": bin},
			})
		}
	}

	// The MiniMax plan is suggested when the API key is exported.
	if getenv != nil {
		if key := getenv("LLMWATCH_MINIMAX_KEY"); key != "" {
			t := byID[TypeMiniMaxPlan]
			url := getenv("LLMWATCH_MINIMAX_URL")
			if url == "" {
				url = t.Fields[1].Default
			}
			out = append(out, store.Provider{
				Type:    t.ID,
				Name:    t.Label,
				Enabled: true,
				Config: map[string]string{
					"api_key": key,
					"url":     url,
				},
			})
		}
	}

	return out
}

// exists reports whether the given path exists.
func exists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}
