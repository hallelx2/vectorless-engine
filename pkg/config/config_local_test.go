package config

import "testing"

// TestLocalModeDefaults: with VLE_LOCAL_MODE set, Load with no file and no
// other env boots a complete, valid config — :7654, a localhost Postgres
// URL, local storage, river queue — with nothing else required.
func TestLocalModeDefaults(t *testing.T) {
	t.Setenv("VLE_LOCAL_MODE", "true")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("local-mode Load() with no other config should succeed, got: %v", err)
	}
	if cfg.Server.Addr != ":7654" {
		t.Errorf("local mode server.addr = %q, want :7654", cfg.Server.Addr)
	}
	if cfg.Database.URL != defaultLocalDatabaseURL {
		t.Errorf("local mode database.url = %q, want %q", cfg.Database.URL, defaultLocalDatabaseURL)
	}
	if cfg.Storage.Driver != "local" {
		t.Errorf("local mode storage.driver = %q, want local", cfg.Storage.Driver)
	}
	if cfg.Queue.Driver != "river" {
		t.Errorf("local mode queue.driver = %q, want river", cfg.Queue.Driver)
	}
	if cfg.Storage.Local.Root == "" {
		t.Error("local mode storage.local.root must be set")
	}
}

// TestLocalModeTruthyForms: the env flag accepts the usual truthy spellings
// and ignores everything else.
func TestLocalModeTruthyForms(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Setenv("VLE_LOCAL_MODE", v)
		if !LocalModeEnabled() {
			t.Errorf("VLE_LOCAL_MODE=%q should enable local mode", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off", "nope"} {
		t.Setenv("VLE_LOCAL_MODE", v)
		if LocalModeEnabled() {
			t.Errorf("VLE_LOCAL_MODE=%q should NOT enable local mode", v)
		}
	}
}

// TestLocalModeEnvOverridesWin: local mode only moves the starting point —
// explicit env values still override the local defaults.
func TestLocalModeEnvOverridesWin(t *testing.T) {
	t.Setenv("VLE_LOCAL_MODE", "true")
	t.Setenv("VLE_SERVER_ADDR", ":9999")
	t.Setenv("VLE_DATABASE_URL", "postgres://custom:custom@db:5432/custom?sslmode=disable")
	t.Setenv("VLE_STORAGE_LOCAL_ROOT", "/srv/docs")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.Server.Addr != ":9999" {
		t.Errorf("env should override local addr: got %q, want :9999", cfg.Server.Addr)
	}
	if cfg.Database.URL != "postgres://custom:custom@db:5432/custom?sslmode=disable" {
		t.Errorf("env should override local db url, got %q", cfg.Database.URL)
	}
	if cfg.Storage.Local.Root != "/srv/docs" {
		t.Errorf("VLE_STORAGE_LOCAL_ROOT should set storage root, got %q", cfg.Storage.Local.Root)
	}
}

// TestNonLocalModeUnchanged: without the flag the historical defaults hold
// (:8080), and the engine still requires a database URL for the river
// queue — i.e. local mode is the ONLY thing that injects one.
func TestNonLocalModeUnchanged(t *testing.T) {
	t.Setenv("VLE_LOCAL_MODE", "")
	// Provide a DB URL so validation passes for the river default.
	t.Setenv("VLE_DATABASE_URL", "postgres://x:x@localhost:5432/x?sslmode=disable")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.Server.Addr != ":8080" {
		t.Errorf("non-local addr = %q, want :8080", cfg.Server.Addr)
	}
}

// TestNonLocalModeMissingDBURLFails proves the local-mode injection is what
// removes the "no required config" gap: without it and without a DB URL,
// the river queue fails validation.
func TestNonLocalModeMissingDBURLFails(t *testing.T) {
	t.Setenv("VLE_LOCAL_MODE", "")
	t.Setenv("VLE_DATABASE_URL", "")

	if _, err := Load(""); err == nil {
		t.Fatal("expected validation error for river queue with no database.url, got nil")
	}
}
