package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// openTest opens a database in a temporary directory and closes it when the
// test ends.
func openTest(t *testing.T) (*DB, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "llmwatch.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	return db, path
}

func TestOpenCreatesFileWithRestrictedPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the POSIX file modes do not apply on Windows")
	}

	// The parent directory does not exist yet: it must be created.
	dir := filepath.Join(t.TempDir(), "state", "nested")
	path := filepath.Join(dir, "llmwatch.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("permissions = %o, want 600", perm)
	}

	// The parent directory must have been created and be restricted.
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Fatalf("directory permissions = %o, want 700", perm)
	}

	// The database must be usable.
	if err := db.SetSetting(context.Background(), "k", "v"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llmwatch.db")

	ctx := context.Background()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.SetSetting(ctx, "theme", "dark"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Opening the same file again must not fail and must keep the data.
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("Open again: %v", err)
	}
	defer func() {
		if err := db2.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	value, ok, err := db2.GetSetting(ctx, "theme")
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if !ok || value != "dark" {
		t.Fatalf("GetSetting = (%q, %t), want (%q, true)", value, ok, "dark")
	}

	var version int
	if err := db2.priv.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != 1 {
		t.Fatalf("user_version = %d, want 1", version)
	}
}

func TestGetSettingMissing(t *testing.T) {
	db, _ := openTest(t)

	value, ok, err := db.GetSetting(context.Background(), "absent")
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if ok {
		t.Fatalf("GetSetting returned ok = true, want false")
	}
	if value != "" {
		t.Fatalf("GetSetting returned %q, want an empty string", value)
	}
}

func TestSetSettingUpserts(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()

	if err := db.SetSetting(ctx, "theme", "dark"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	value, ok, err := db.GetSetting(ctx, "theme")
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if !ok || value != "dark" {
		t.Fatalf("GetSetting = (%q, %t), want (%q, true)", value, ok, "dark")
	}

	// A second write must replace the value, not fail.
	if err := db.SetSetting(ctx, "theme", "light"); err != nil {
		t.Fatalf("SetSetting again: %v", err)
	}

	value, ok, err = db.GetSetting(ctx, "theme")
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if !ok || value != "light" {
		t.Fatalf("GetSetting = (%q, %t), want (%q, true)", value, ok, "light")
	}
}

func TestSessionLifecycle(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()

	now := time.Now()
	if err := db.CreateSession(ctx, "hash-valid", now.Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := db.CreateSession(ctx, "hash-expired", now.Add(-time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	valid, err := db.SessionValid(ctx, "hash-valid", now)
	if err != nil {
		t.Fatalf("SessionValid: %v", err)
	}
	if !valid {
		t.Fatal("SessionValid = false, want true for a live session")
	}

	valid, err = db.SessionValid(ctx, "hash-expired", now)
	if err != nil {
		t.Fatalf("SessionValid: %v", err)
	}
	if valid {
		t.Fatal("SessionValid = true, want false for an expired session")
	}

	valid, err = db.SessionValid(ctx, "hash-unknown", now)
	if err != nil {
		t.Fatalf("SessionValid: %v", err)
	}
	if valid {
		t.Fatal("SessionValid = true, want false for an unknown session")
	}
}

func TestDeleteSession(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()

	now := time.Now()
	if err := db.CreateSession(ctx, "hash-1", now.Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := db.DeleteSession(ctx, "hash-1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	valid, err := db.SessionValid(ctx, "hash-1", now)
	if err != nil {
		t.Fatalf("SessionValid: %v", err)
	}
	if valid {
		t.Fatal("SessionValid = true after the deletion, want false")
	}

	// Deleting an unknown session is not an error.
	if err := db.DeleteSession(ctx, "hash-unknown"); err != nil {
		t.Fatalf("DeleteSession of an unknown session: %v", err)
	}
}

func TestPurgeExpiredSessions(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()

	now := time.Now()
	if err := db.CreateSession(ctx, "hash-live", now.Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := db.CreateSession(ctx, "hash-dead", now.Add(-time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := db.PurgeExpiredSessions(ctx, now); err != nil {
		t.Fatalf("PurgeExpiredSessions: %v", err)
	}

	valid, err := db.SessionValid(ctx, "hash-live", now)
	if err != nil {
		t.Fatalf("SessionValid: %v", err)
	}
	if !valid {
		t.Fatal("the live session was purged")
	}

	valid, err = db.SessionValid(ctx, "hash-dead", now)
	if err != nil {
		t.Fatalf("SessionValid: %v", err)
	}
	if valid {
		t.Fatal("the expired session survived the purge")
	}
}

func TestDeleteAllSessions(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()

	now := time.Now()
	for _, hash := range []string{"hash-1", "hash-2", "hash-3"} {
		if err := db.CreateSession(ctx, hash, now.Add(time.Hour)); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}

	if err := db.DeleteAllSessions(ctx); err != nil {
		t.Fatalf("DeleteAllSessions: %v", err)
	}

	for _, hash := range []string{"hash-1", "hash-2", "hash-3"} {
		valid, err := db.SessionValid(ctx, hash, now)
		if err != nil {
			t.Fatalf("SessionValid: %v", err)
		}
		if valid {
			t.Fatalf("the session %s survived the deletion", hash)
		}
	}
}

func TestCreateProvider(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()

	p, err := db.CreateProvider(ctx, Provider{
		Type:    "anthropic",
		Name:    "claude",
		Enabled: true,
		Config:  map[string]string{"api_key": "secret"},
	})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if p.ID == 0 {
		t.Fatal("CreateProvider did not assign an id")
	}
	if p.Type != "anthropic" || p.Name != "claude" || !p.Enabled {
		t.Fatalf("unexpected provider: %+v", p)
	}
	if p.Config["api_key"] != "secret" {
		t.Fatalf("unexpected config: %+v", p.Config)
	}
	if p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() {
		t.Fatalf("unexpected timestamps: %+v", p)
	}

	count, err := db.CountProviders(ctx)
	if err != nil {
		t.Fatalf("CountProviders: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountProviders = %d, want 1", count)
	}
}

func TestCreateProviderIgnoresInputIDAndDates(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()

	p, err := db.CreateProvider(ctx, Provider{
		ID:        999,
		Name:      "claude",
		CreatedAt: time.Unix(1, 0),
		UpdatedAt: time.Unix(1, 0),
	})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if p.ID == 999 {
		t.Fatalf("CreateProvider kept the input id: %d", p.ID)
	}
	if p.CreatedAt.Unix() == 1 {
		t.Fatalf("CreateProvider kept the input created_at: %s", p.CreatedAt)
	}
	if p.Config == nil {
		t.Fatal("CreateProvider returned a nil config, want an empty map")
	}
}

func TestGetProvider(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()

	created, err := db.CreateProvider(ctx, Provider{
		Type:   "openai",
		Name:   "gpt",
		Config: map[string]string{"base_url": "https://api.openai.com"},
	})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	got, err := db.GetProvider(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if got.ID != created.ID || got.Name != "gpt" || got.Type != "openai" {
		t.Fatalf("unexpected provider: %+v", got)
	}
	if got.Config["base_url"] != "https://api.openai.com" {
		t.Fatalf("unexpected config: %+v", got.Config)
	}
}

func TestGetProviderNotFound(t *testing.T) {
	db, _ := openTest(t)

	_, err := db.GetProvider(context.Background(), 42)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetProvider error = %v, want ErrNotFound", err)
	}
}

func TestListProvidersOrderedAndNotNil(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()

	// The empty list must not be nil.
	list, err := db.ListProviders(ctx)
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	if list == nil {
		t.Fatal("ListProviders returned nil, want an empty slice")
	}
	if len(list) != 0 {
		t.Fatalf("ListProviders returned %d providers, want 0", len(list))
	}

	for _, name := range []string{"zeta", "alpha", "mid"} {
		if _, err := db.CreateProvider(ctx, Provider{Type: "t", Name: name}); err != nil {
			t.Fatalf("CreateProvider: %v", err)
		}
	}

	list, err = db.ListProviders(ctx)
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("ListProviders returned %d providers, want 3", len(list))
	}
	want := []string{"alpha", "mid", "zeta"}
	for i, name := range want {
		if list[i].Name != name {
			t.Fatalf("provider %d = %q, want %q", i, list[i].Name, name)
		}
	}
}

func TestUpdateProvider(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()

	created, err := db.CreateProvider(ctx, Provider{
		Type:    "anthropic",
		Name:    "claude",
		Enabled: true,
		Config:  map[string]string{"api_key": "old"},
	})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	updated, err := db.UpdateProvider(ctx, Provider{
		ID:      created.ID,
		Type:    "openai",
		Name:    "gpt",
		Enabled: false,
		Config:  map[string]string{"api_key": "new"},
	})
	if err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}
	if updated.ID != created.ID {
		t.Fatalf("UpdateProvider changed the id: %d", updated.ID)
	}
	if updated.Type != "openai" || updated.Name != "gpt" || updated.Enabled {
		t.Fatalf("unexpected provider: %+v", updated)
	}
	if updated.Config["api_key"] != "new" {
		t.Fatalf("unexpected config: %+v", updated.Config)
	}
	if !updated.UpdatedAt.After(updated.CreatedAt) &&
		!updated.UpdatedAt.Equal(updated.CreatedAt) {
		t.Fatalf("updated_at = %s, want at least created_at = %s",
			updated.UpdatedAt, updated.CreatedAt)
	}

	// The change must be persisted.
	got, err := db.GetProvider(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if got.Name != "gpt" || got.Type != "openai" || got.Enabled {
		t.Fatalf("unexpected stored provider: %+v", got)
	}
	if got.Config["api_key"] != "new" {
		t.Fatalf("unexpected stored config: %+v", got.Config)
	}
}

func TestUpdateProviderNotFound(t *testing.T) {
	db, _ := openTest(t)

	_, err := db.UpdateProvider(context.Background(), Provider{ID: 42, Name: "ghost"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateProvider error = %v, want ErrNotFound", err)
	}
}

func TestDuplicateProviderName(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()

	if _, err := db.CreateProvider(ctx, Provider{Type: "t", Name: "claude"}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	// The second provider with the same name must be rejected.
	_, err := db.CreateProvider(ctx, Provider{Type: "t", Name: "claude"})
	if !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("CreateProvider error = %v, want ErrDuplicateName", err)
	}

	count, err := db.CountProviders(ctx)
	if err != nil {
		t.Fatalf("CountProviders: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountProviders = %d, want 1", count)
	}
}

func TestUpdateProviderDuplicateName(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()

	if _, err := db.CreateProvider(ctx, Provider{Type: "t", Name: "claude"}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	other, err := db.CreateProvider(ctx, Provider{Type: "t", Name: "gpt"})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	_, err = db.UpdateProvider(ctx, Provider{ID: other.ID, Name: "claude"})
	if !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("UpdateProvider error = %v, want ErrDuplicateName", err)
	}

	// The provider must keep its original name.
	got, err := db.GetProvider(ctx, other.ID)
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if got.Name != "gpt" {
		t.Fatalf("name = %q, want %q", got.Name, "gpt")
	}
}

func TestDeleteProvider(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()

	created, err := db.CreateProvider(ctx, Provider{Type: "t", Name: "claude"})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	if err := db.DeleteProvider(ctx, created.ID); err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}

	_, err = db.GetProvider(ctx, created.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetProvider error = %v, want ErrNotFound", err)
	}

	count, err := db.CountProviders(ctx)
	if err != nil {
		t.Fatalf("CountProviders: %v", err)
	}
	if count != 0 {
		t.Fatalf("CountProviders = %d, want 0", count)
	}
}

func TestDeleteProviderNotFound(t *testing.T) {
	db, _ := openTest(t)

	err := db.DeleteProvider(context.Background(), 42)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteProvider error = %v, want ErrNotFound", err)
	}
}

func TestProviderConfigRoundTrip(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()

	want := map[string]string{
		"api_key":  "sk-123",
		"base_url": "https://example.com",
		"model":    "claude-sonnet-4",
	}

	created, err := db.CreateProvider(ctx, Provider{
		Type:   "anthropic",
		Name:   "claude",
		Config: want,
	})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	got, err := db.GetProvider(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if len(got.Config) != len(want) {
		t.Fatalf("config = %+v, want %+v", got.Config, want)
	}
	for k, v := range want {
		if got.Config[k] != v {
			t.Fatalf("config[%q] = %q, want %q", k, got.Config[k], v)
		}
	}

	// The configuration must be stored as a JSON object.
	var raw string
	if err := db.priv.QueryRowContext(ctx,
		`SELECT config FROM providers WHERE id = ?`, created.ID).Scan(&raw); err != nil {
		t.Fatalf("query config: %v", err)
	}
	if raw == "" || raw[0] != '{' {
		t.Fatalf("stored config = %q, want a JSON object", raw)
	}
}
