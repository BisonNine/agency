package fusion

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/internal/testalerts"
)

// freshEngine lets the test build the process's shared engine anew, and
// forgets it again at the end. Not for parallel tests.
func freshEngine(t *testing.T) {
	t.Helper()
	engineOnce, engine = sync.Once{}, nil
	t.Cleanup(func() { engineOnce, engine = sync.Once{}, nil })
}

func fusionConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv(global.EnvVarHome, t.TempDir())
	cfg := config.DefaultConfig()
	cfg.Agents.List = []config.AgentConfig{{ID: "alice", Fusion: true, MCPTools: []string{"wxca"}}}
	return cfg
}

// With no services configured the engine builds, offers no tools and no
// services, and serves the OAuth routes; the token store is created.
func TestSharedEngine_NoServices(t *testing.T) {
	freshEngine(t)
	cfg := fusionConfig(t)
	if defs := GlobalProvider.RegisterTools(toolspec.Deps{Cfg: cfg, AgentID: "alice"}); len(defs) != 0 {
		t.Errorf("tools without services: %d", len(defs))
	}
	if names := ServiceNames(cfg); len(names) != 0 {
		t.Errorf("services = %v", names)
	}
	if OAuthHandler(cfg) == nil {
		t.Error("no OAuth handler")
	}
	if GlobalProvider.Description() != "Config-driven REST-API tools (MCPFusion): external APIs exposed as tools" {
		t.Errorf("Description = %q", GlobalProvider.Description())
	}
	if _, err := os.Stat(cfg.FusionTokensPath()); err != nil {
		t.Errorf("token store: %v", err)
	}
}

// A token store that cannot be opened disables every Fusion tool, raises
// one alert naming the store, and mounts no OAuth routes.
func TestSharedEngine_StoreUnavailable(t *testing.T) {
	freshEngine(t)
	rec := testalerts.Install(t)
	cfg := fusionConfig(t)
	// The store's folder cannot be created: a file is in its place.
	if err := os.WriteFile(filepath.Dir(cfg.FusionTokensPath()), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if defs := GlobalProvider.RegisterTools(toolspec.Deps{Cfg: cfg, AgentID: "alice"}); defs != nil {
		t.Errorf("tools with no store: %d", len(defs))
	}
	if OAuthHandler(cfg) != nil || ServiceNames(cfg) != nil {
		t.Error("OAuth routes or services without an engine")
	}
	got := rec.Alerts()
	if len(got) != 1 || got[0].Title != "Fusion token store unavailable" || got[0].EventID != "fusion" ||
		!strings.HasPrefix(got[0].Description, cfg.FusionTokensPath()+": ") {
		t.Errorf("alerts = %+v", got)
	}
}

func TestDataStore_Failures(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSQLiteDataStore(filepath.Join(blocker, "tokens.db")); err == nil || !strings.Contains(err.Error(), "fusion datastore: create dir: ") {
		t.Errorf("folder under a file: %v", err)
	}
	if _, err := NewSQLiteDataStore(dir); err == nil || !strings.HasPrefix(err.Error(), "fusion datastore: ") {
		t.Errorf("store is a folder: %v", err)
	}
	notDB := filepath.Join(dir, "not.db")
	if err := os.WriteFile(notDB, []byte(strings.Repeat("not a database ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSQLiteDataStore(notDB); err == nil || !strings.Contains(err.Error(), "fusion datastore: schema: ") {
		t.Errorf("not a database: %v", err)
	}

	ds, err := NewSQLiteDataStore(filepath.Join(dir, "ok.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ds.(*sqliteDataStore).db.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := ds.Get(ctx, "c", "k"); err == nil || !strings.HasPrefix(err.Error(), "fusion datastore: get: ") {
		t.Errorf("get: %v", err)
	}
	if err := ds.Set(ctx, "c", "k", []byte("v")); err == nil || !strings.HasPrefix(err.Error(), "fusion datastore: set: ") {
		t.Errorf("set: %v", err)
	}
	if err := ds.Delete(ctx, "c", "k"); err == nil || !strings.HasPrefix(err.Error(), "fusion datastore: delete: ") {
		t.Errorf("delete: %v", err)
	}
}
