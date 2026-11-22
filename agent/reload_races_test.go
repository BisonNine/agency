// ClawEh
// License: MIT

package agent

import (
	"context"
	"sync"
	"testing"

	"go.bison9.com/agency/commands"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/dump"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
)

// The cooldown commands read the catalogue while a reload hands it a new
// one (race detector).
func TestCooldownHooks_RaceWithReload(t *testing.T) {
	cfg := compressTestConfig()
	builder := providers.NewCatalogBuilder(nil, nil)
	al := &AgentLoop{catalog: builder.Build(cfg), catalogs: builder}
	rt := &commands.Runtime{}
	al.addCooldownHooks(rt)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			al.catalog.Adopt(builder.Build(cfg))
		}
	})
	for range 200 {
		rt.ListCooldowns()
		rt.ClearCooldown("openai", "some-model")
		rt.ResetCooldown()
	}
	close(stop)
	wg.Wait()
}

// A turn reads the configuration a reload swaps in under the loop's lock
// when it records a response and takes a direct answer (race detector).
func TestTurnConfigReads_RaceWithReload(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al := &AgentLoop{cfg: &config.Config{}, dumpsDir: dump.Dir{Path: t.TempDir()}}
	agent := &AgentInstance{ID: "alice"}
	al.exposeReasoningCache = map[string]bool{activeModelCacheKey(agent.ID, "s"): false}
	turn := &llmTurn{al: al, agent: agent, opts: processOptions{SessionKey: "s"}}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			al.mu.Lock()
			al.cfg = &config.Config{}
			al.mu.Unlock()
		}
	})
	for range 200 {
		resp := &providers.LLMResponse{Content: "hi"}
		turn.recordResponse(context.Background(), resp)
		turn.directAnswer(resp)
	}
	close(stop)
	wg.Wait()
}

// The MCP setup and the transcription echo read the configuration a reload
// swaps in under the loop's lock (race detector).
func TestMCPAndTranscriptionConfigReads_RaceWithReload(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al := &AgentLoop{cfg: &config.Config{}}
	stop, started := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		close(started)
		for {
			select {
			case <-stop:
				return
			default:
			}
			al.mu.Lock()
			al.cfg = &config.Config{}
			al.mu.Unlock()
		}
	})
	<-started
	for range 2000 {
		if mgr := al.connectAndRegisterMCP(context.Background()); mgr != nil {
			t.Fatal("MCP connected with it disabled")
		}
		al.sendTranscriptionFeedback(context.Background(), "telegram", "1", "m", "Transcript: hi")
	}
	close(stop)
	wg.Wait()
}
