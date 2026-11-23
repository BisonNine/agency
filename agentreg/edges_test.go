// ClawEh
// License: MIT

package agentreg

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/routing"
)

// humanConfig is testConfig with a human provider and bob as a human agent
// (his model is a person).
func humanConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.Providers = append(cfg.Providers, modelspec.Provider{Name: "People", Protocol: config.HumanProtocol})
	cfg.Models = append(cfg.Models, modelspec.Model{ModelName: "Bob (human)", Model: "bob", Provider: "People", Enabled: true})
	cfg.Agents.List[1].Models = []string{"Bob (human)"}
	return cfg
}

func TestShortID(t *testing.T) {
	tests := []struct{ in, want string }{
		{"0123456789abcdef", "01234567"},
		{"012345678", "01234567"},
		{"01234567", "01234567"},
		{"abc", "abc"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := ShortID(tt.in); got != tt.want {
			t.Errorf("ShortID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNew_RequiresBuild(t *testing.T) {
	if _, err := New[*fakeInst](t.Context(), testConfig(t), Hooks[*fakeInst]{}); err == nil {
		t.Fatal("New without a Build hook must fail")
	}
	r := mustNew(t, testConfig(t), newFakeHost())
	if err := r.Reload(t.Context(), testConfig(t), nil, commitOK); err == nil {
		t.Fatal("Reload without a Build hook must fail")
	}
}

// A config agent that fails to build fails New, and the agents already built
// are closed rather than leaked.
func TestNew_BuildFailureClosesBuilt(t *testing.T) {
	h := newFakeHost()
	h.failID = "bob"
	_, err := New(t.Context(), testConfig(t), h.hooks())
	if err == nil || !strings.Contains(err.Error(), `"bob"`) {
		t.Fatalf("New = %v, want an error naming bob", err)
	}
	built := h.instances()
	if len(built) != 1 || built[0].spec.ID != "alice" || !built[0].closed() {
		t.Fatalf("built = %+v, want alice built and closed", built)
	}
}

func TestLookups_ConfigAndTemp(t *testing.T) {
	r := mustNew(t, testConfig(t), newFakeHost())
	temp := mustFresh(t, r, config.AgentConfig{})

	if _, ok := r.GetConfigured("alice"); !ok {
		t.Error("GetConfigured(alice) not found")
	}
	if _, ok := r.GetConfigured(temp); ok {
		t.Error("GetConfigured must not find a temporary agent")
	}
	if _, ok := r.GetConfigured("nobody"); ok {
		t.Error("GetConfigured(nobody) found something")
	}
	if _, ok := r.Info("nobody"); ok {
		t.Error("Info(nobody) found something")
	}
	if !r.IsTemp(temp) {
		t.Error("IsTemp(temp) = false")
	}
	if r.IsTemp("alice") || r.IsTemp("nobody") {
		t.Error("IsTemp is true for a config or unknown agent")
	}
	if got := r.Default(); got == nil || got.spec.ID != "alice" {
		t.Errorf("Default() = %+v, want alice", got)
	}
}

func TestDefault_NoneWhenNoEnabledAgent(t *testing.T) {
	cfg := testConfig(t)
	off := false
	for i := range cfg.Agents.List {
		cfg.Agents.List[i].Enabled = &off
	}
	r := mustNew(t, cfg, newFakeHost())
	if got := r.Default(); got != nil {
		t.Fatalf("Default() = %+v, want none", got)
	}
	if id := r.DefaultID(); id != "" {
		t.Fatalf("DefaultID() = %q, want empty", id)
	}
}

// A human agent is never the default, even listed first.
func TestHumanAgent_NeverDefault(t *testing.T) {
	cfg := humanConfig(t)
	cfg.Agents.List[0].Default = false
	cfg.Agents.List[0], cfg.Agents.List[1] = cfg.Agents.List[1], cfg.Agents.List[0]
	r := mustNew(t, cfg, newFakeHost())
	if id := r.DefaultID(); id != "alice" {
		t.Fatalf("DefaultID() = %q, want alice (bob is a person)", id)
	}
}

func TestCreate_RefusesHuman(t *testing.T) {
	r := mustNew(t, humanConfig(t), newFakeHost())
	if _, err := r.CreateClone("bob"); !errors.Is(err, ErrHuman) {
		t.Errorf("clone of a human agent: err = %v, want ErrHuman", err)
	}
	if _, err := r.CreateFresh(config.AgentConfig{Models: []string{"Bob (human)"}}); !errors.Is(err, ErrHuman) {
		t.Errorf("fresh agent on a human model: err = %v, want ErrHuman", err)
	}
	if n := len(r.ListTemp()); n != 0 {
		t.Fatalf("%d temporary agents registered after refusals", n)
	}
}

// A reload that makes a fresh agent's model a person deletes the agent.
func TestReload_DeletesTempAgentOnHumanModel(t *testing.T) {
	cfg := testConfig(t)
	h := newFakeHost()
	r := mustNew(t, cfg, h)
	fresh := mustFresh(t, r, config.AgentConfig{Models: []string{"m1"}})

	next := testConfig(t)
	next.Providers = append(next.Providers, modelspec.Provider{Name: "People", Protocol: config.HumanProtocol})
	next.Models[0].Provider = "People"
	if err := r.Reload(t.Context(), next, h.build, commitOK); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, ok := r.Get(fresh); ok {
		t.Fatal("a temporary agent on a human model survived the reload")
	}
}

// A clone of a model-less source keeps its clone model while it is one of
// the default models.
func TestReload_CloneModelFromDefaults(t *testing.T) {
	cfg := testConfig(t)
	cfg.Agents.Defaults.Models = []string{"m1"}
	h := newFakeHost()
	r := mustNew(t, cfg, h)
	clone := mustClone(t, r, "bob", CloneModel("m1"))

	next := testConfig(t)
	next.Agents.Defaults.Models = []string{"m1"}
	if err := r.Reload(t.Context(), next, h.build, commitOK); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, ok := r.Get(clone); !ok {
		t.Fatal("a clone on one of the default models was deleted")
	}

	next = testConfig(t)
	if err := r.Reload(t.Context(), next, h.build, commitOK); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, ok := r.Get(clone); ok {
		t.Fatal("a clone whose model left the defaults survived the reload")
	}
}

func TestResolveRoute_FollowsReload(t *testing.T) {
	cfg := testConfig(t)
	cfg.Bindings = []config.AgentBinding{
		{AgentID: "bob", Match: config.BindingMatch{Channel: "telegram"}},
	}
	h := newFakeHost()
	r := mustNew(t, cfg, h)
	if got := r.ResolveRoute(routing.RouteInput{Channel: "telegram"}); got.AgentID != "bob" {
		t.Fatalf("route = %+v, want bob", got)
	}
	if got := r.ResolveRoute(routing.RouteInput{Channel: "signal"}); got.AgentID != "alice" {
		t.Fatalf("unbound route = %+v, want the default alice", got)
	}

	next := testConfig(t)
	if err := r.Reload(t.Context(), next, h.build, commitOK); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := r.ResolveRoute(routing.RouteInput{Channel: "telegram"}); got.AgentID != "alice" {
		t.Fatalf("route after the binding was removed = %+v, want alice", got)
	}
}

func TestAfterClose_Refusals(t *testing.T) {
	h := newFakeHost()
	r := mustNew(t, testConfig(t), h)
	temp := mustFresh(t, r, config.AgentConfig{})
	r.Close()
	if err := r.DeleteWhenIdle(temp); !errors.Is(err, ErrClosed) {
		t.Errorf("DeleteWhenIdle after Close = %v, want ErrClosed", err)
	}
	if err := r.Delete(temp); !errors.Is(err, ErrClosed) {
		t.Errorf("Delete after Close = %v, want ErrClosed", err)
	}
	if err := r.Reload(t.Context(), testConfig(t), h.build, commitOK); !errors.Is(err, ErrClosed) {
		t.Errorf("Reload after Close = %v, want ErrClosed", err)
	}
	for _, b := range h.instances() {
		if !b.closed() {
			t.Errorf("%s (built by the refused reload or before) is not closed", b.spec.ID)
		}
	}
}

func TestDeleteWhenIdle_Refusals(t *testing.T) {
	r := mustNew(t, testConfig(t), newFakeHost())
	if err := r.DeleteWhenIdle("alice"); !errors.Is(err, ErrNotTemp) {
		t.Errorf("DeleteWhenIdle(config agent) = %v, want ErrNotTemp", err)
	}
	if err := r.DeleteWhenIdle("nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteWhenIdle(unknown) = %v, want ErrNotFound", err)
	}
	if _, ok := r.Get("alice"); !ok {
		t.Error("alice was removed")
	}
}

// A build failure leaves no agent and no state directory behind.
func TestCreate_BuildFailureRemovesStateDir(t *testing.T) {
	cfg := testConfig(t)
	h := newFakeHost()
	r := mustNew(t, cfg, h)
	h.onBuild = func(spec Spec) {
		if spec.Origin == OriginTemp {
			h.mu.Lock()
			h.failID = spec.ID
			h.mu.Unlock()
		}
	}
	if _, err := r.CreateFresh(config.AgentConfig{}); err == nil || !strings.Contains(err.Error(), "build refused") {
		t.Fatalf("CreateFresh = %v, want the build error", err)
	}
	if n := len(r.ListTemp()); n != 0 {
		t.Fatalf("%d temporary agents registered after a failed build", n)
	}
	dirs, err := os.ReadDir(r.TempRoot())
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 0 {
		t.Fatalf("temp root holds %v after a failed build", dirs)
	}
}

// A temp root that cannot hold a directory refuses the creation.
func TestCreate_StateDirFailure(t *testing.T) {
	cfg := testConfig(t)
	r := mustNew(t, cfg, newFakeHost())
	writeFile(t, r.TempRoot(), []byte("not a directory"))
	if _, err := r.CreateFresh(config.AgentConfig{}); err == nil {
		t.Fatal("CreateFresh with an unusable temp root must fail")
	}
	if n := len(r.ListTemp()); n != 0 {
		t.Fatalf("%d temporary agents registered", n)
	}
}

// An ephemeral agent is never saved, whichever agent triggers the save.
func TestPersist_SkipsEphemeral(t *testing.T) {
	cfg := testConfig(t)
	r := mustNew(t, cfg, newFakeHost())
	ephemeral := mustFresh(t, r, config.AgentConfig{}, EphemeralMemory())
	kept := mustFresh(t, r, config.AgentConfig{})

	data, err := os.ReadFile(filepath.Join(cfg.DataDir(), global.InternalDir, StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	var file stateFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Agents) != 1 || file.Agents[0].ID != kept {
		t.Fatalf("saved %+v, want only %s (not the ephemeral %s)", file.Agents, kept, ephemeral)
	}
}

// A state file that cannot be written does not stop the agent working.
func TestPersist_WriteFailureKeepsAgent(t *testing.T) {
	cfg := testConfig(t)
	r := mustNew(t, cfg, newFakeHost())
	statePath := filepath.Join(cfg.DataDir(), global.InternalDir, StateFileName)
	if err := os.MkdirAll(filepath.Join(statePath, "blocker"), 0o700); err != nil {
		t.Fatal(err)
	}
	id := mustFresh(t, r, config.AgentConfig{})
	if _, ok := r.Get(id); !ok {
		t.Fatal("the agent is gone after a failed save")
	}
	if info, err := os.Stat(statePath); err != nil || !info.IsDir() {
		t.Fatalf("the blocking directory was replaced: %v %v", info, err)
	}
}

// A sweep that cannot release an idle agent keeps it and does not count it.
func TestSweep_RetireFailureKeepsAgent(t *testing.T) {
	fc := clock.NewFake(time.Now())
	h := newFakeHost()
	hooks := h.hooks()
	hooks.Clock = fc
	r, err := New(t.Context(), testConfig(t), hooks)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(r.Close)
	idle := mustFresh(t, r, config.AgentConfig{}, Temp(time.Hour))
	h.mu.Lock()
	h.failRetire = true
	h.mu.Unlock()
	if n := r.Sweep(fc.Now().Add(2 * time.Hour)); n != 0 {
		t.Fatalf("Sweep deleted %d, want 0", n)
	}
	if _, ok := r.Get(idle); !ok {
		t.Fatal("an agent that could not be released was dropped")
	}
}

func TestTempPaths_WithoutDataDir(t *testing.T) {
	cfg := &config.Config{}
	base := t.TempDir()
	cfg.Agents.BaseDir = base
	root, state := tempPaths(cfg)
	if root != filepath.Join(base, ".temp-agents") || state != "" {
		t.Errorf("tempPaths(absolute base) = %q, %q", root, state)
	}
	cfg.Agents.BaseDir = "relative"
	root, state = tempPaths(cfg)
	if root != filepath.Join(os.TempDir(), "claw-temp-agents") || state != "" {
		t.Errorf("tempPaths(relative base) = %q, %q", root, state)
	}
}
