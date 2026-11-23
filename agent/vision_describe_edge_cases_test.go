// ClawEh
// License: MIT

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/agentreg"
	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
)

// visionEdgeLoop is a loop whose model "v" is served by an OpenAI-style
// server answering every chat with reply, or with status 400 when reply is
// "".
func visionEdgeLoop(t *testing.T, reply string) (*AgentLoop, *config.Config) {
	t.Helper()
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if reply == "" {
			w.WriteHeader(http.StatusBadRequest)
			if _, err := w.Write([]byte(`{"error":{"message":"bad image"}}`)); err != nil {
				return
			}
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": reply}, "finish_reason": "stop",
		}}}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	t.Cleanup(ts.Close)
	cfg := ownerTestConfig(t)
	cfg.Providers = []modelspec.Provider{{Name: "p", Protocol: "openai-chat", BaseURL: ts.URL + "/v1", APIKey: "k"}}
	cfg.Models = []modelspec.Model{
		{ModelName: "alpha", Model: "alpha-wire", Provider: "p", Enabled: true},
		{ModelName: "v", Model: "v-wire", Provider: "p", Enabled: true, Vision: modelspec.VisionUserMessage},
	}
	cfg.Agents.Defaults.Models = []string{"alpha"}
	cfg.TaskModels.VisionModels = []config.TaskModelRef{{Model: "v"}}
	return mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, realClients{}), cfg
}

// The vision client calls its model and returns the description; a fresh
// temporary agent's runs isolated in its workspace; a model's failure is
// returned.
func TestProviderVisionClient_Complete(t *testing.T) {
	al, cfg := visionEdgeLoop(t, "a red square")
	agent := al.registry.Default()
	c, ok := al.buildVisionLLMClient(cfg, agent, "v")
	if !ok {
		t.Fatal("vision model v not resolved")
	}
	pc, isProvider := c.(*providerVisionClient)
	if !isProvider || pc.Model() != "v-wire" {
		t.Errorf("client = %+v, want a provider client for v-wire", c)
	}
	msgs := []spawnllm.Message{{Role: "user", Content: "describe", Media: []string{"data:image/png;base64,AAA"}}}
	if r, err := c.Complete(context.Background(), msgs); err != nil || r.Content != "a red square" {
		t.Errorf("Complete = %+v, %v", r, err)
	}

	fresh := &AgentInstance{ID: "fresh-1", Workspace: t.TempDir(), Spec: agentreg.Spec{Fresh: true}}
	fc, ok := al.buildVisionLLMClient(cfg, fresh, "v")
	fpc, isProvider := fc.(*providerVisionClient)
	if !ok || !isProvider || fpc.workspace != fresh.Workspace {
		t.Fatalf("fresh agent's vision client = %+v, %v; want its workspace", fc, ok)
	}
	if r, err := fc.Complete(context.Background(), msgs); err != nil || r.Content != "a red square" {
		t.Errorf("fresh Complete = %+v, %v", r, err)
	}

	gone := &providerVisionClient{catalog: al.catalog, alias: "nosuch", model: "nosuch"}
	if _, err := gone.Complete(context.Background(), msgs); err == nil {
		t.Error("Complete on a model not in the catalogue succeeded")
	}

	bad, _ := visionEdgeLoop(t, "")
	bc, ok := bad.buildVisionLLMClient(bad.GetConfig(), bad.registry.Default(), "v")
	if !ok {
		t.Fatal("vision model v not resolved")
	}
	if _, err := bc.Complete(context.Background(), msgs); err == nil {
		t.Error("Complete on a failing model succeeded")
	}
}

// An unknown model has no vision.
func TestModelHasVision_Unknown(t *testing.T) {
	al := &AgentLoop{cfg: &config.Config{}}
	if al.modelHasVision("ghost") {
		t.Error("an unknown model has vision")
	}
}

// A blank description moves on to the next client; when every client is
// blank or failing there is none.
func TestDescribeImages_BlankMovesOn(t *testing.T) {
	al := &AgentLoop{cfg: &config.Config{}}
	blank := &stubVisionClient{reply: "  ", model: "blank"}
	good := &stubVisionClient{reply: "a blue circle", model: "good"}
	agent := &AgentInstance{ID: "a1", VisionClients: []visionClient{blank, good}}
	if desc, ok := al.describeImages(context.Background(), agent, []string{"data:image/png;base64,AAA"}, ""); !ok || desc != "a blue circle" {
		t.Errorf("describeImages = %q, %v", desc, ok)
	}
	agent.VisionClients = []visionClient{blank, &stubVisionClient{err: errors.New("boom")}}
	if desc, ok := al.describeImages(context.Background(), agent, []string{"data:image/png;base64,AAA"}, ""); ok || desc != "" {
		t.Errorf("describeImages with no usable client = %q, %v", desc, ok)
	}
}

// An inbound image that can't be resolved, or can't be described, is left
// on the message as it came.
func TestDescribeInboundMedia_Unchanged(t *testing.T) {
	al := &AgentLoop{cfg: &config.Config{
		Models: []modelspec.Model{{ModelName: "deepseek", Enabled: true, Vision: modelspec.VisionOff}},
	}}
	failing := &stubVisionClient{err: errors.New("boom")}
	agent := &AgentInstance{ID: "a1", Model: "deepseek", VisionClients: []visionClient{failing}}

	msg := spawnllm.Message{Role: "user", Content: "what is this", Media: []string{"media://gone"}}
	al.describeInboundMedia(context.Background(), agent, &msg)
	if failing.calls != 0 || msg.Content != "what is this" || len(msg.Media) != 1 {
		t.Errorf("unresolvable image: calls %d, message %+v", failing.calls, msg)
	}

	msg = spawnllm.Message{Role: "user", Content: "what is this", Media: []string{"data:image/png;base64,AAA"}}
	al.describeInboundMedia(context.Background(), agent, &msg)
	if failing.calls != 1 || msg.Content != "what is this" || len(msg.Media) != 1 || !strings.HasPrefix(msg.Media[0], "data:") {
		t.Errorf("failed description: calls %d, message %+v", failing.calls, msg)
	}
}
