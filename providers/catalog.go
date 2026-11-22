// ClawEh
// License: MIT

package providers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/fsmode"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/internal/childenv"
	"go.bison9.com/agency/logger"
)

// CatalogBuilder builds the process's model catalogue from the
// configuration: at start, and again on each reload for Adopt. One builder
// lives as long as the process, because what it hands every catalogue must
// stay the same across builds for Adopt to keep the clients already built:
// the observer, the logger, the option functions and the human clients.
type CatalogBuilder struct {
	alerts *ModelAlerts
	clock  clock.Clock

	mu     sync.Mutex
	humans map[string]*HumanProvider // by model name
	store  models.SelectionStore
}

// SetSelectionStore sets where the catalogues built from now on persist the
// sets' selections (models.WithSelectionStore).
func (b *CatalogBuilder) SetSelectionStore(st models.SelectionStore) {
	b.mu.Lock()
	b.store = st
	b.mu.Unlock()
}

// NewCatalogBuilder returns a builder whose catalogues report model health
// to alerts and run on clk (nil: the system clock).
func NewCatalogBuilder(alerts *ModelAlerts, clk clock.Clock) *CatalogBuilder {
	if alerts == nil {
		alerts = NewModelAlerts()
	}
	return &CatalogBuilder{alerts: alerts, clock: clk, humans: make(map[string]*HumanProvider)}
}

// Alerts returns the observer every catalogue the builder builds reports to.
func (b *CatalogBuilder) Alerts() *ModelAlerts { return b.alerts }

// catalogModels are the models a catalogue is built with: every enabled
// model, with what the dispatcher used to fill in at call time filled in now.
// A model with no workspace runs its CLI in <CLAW_HOME>/cli; a model with no
// request timeout takes the agents' default.
func catalogModels(cfg *config.Config) []config.ModelConfig {
	out := make([]config.ModelConfig, 0, len(cfg.Models))
	for i := range cfg.Models {
		m := cfg.Models[i]
		if !m.Enabled {
			continue
		}
		if m.Workspace == "" && cfg.DataDir() != "" {
			m.Workspace = cfg.CLIPath()
		}
		if m.RequestTimeout == 0 && cfg.Agents.Defaults.RequestTimeout > 0 {
			m.RequestTimeout = cfg.Agents.Defaults.RequestTimeout
		}
		out = append(out, m)
	}
	return out
}

// limitDefaults are the limits a model that sets none of its own runs with.
func limitDefaults(cfg *config.Config) modelspec.LimitDefaults {
	return modelspec.LimitDefaults{
		MaxTokens:     cfg.Agents.Defaults.MaxTokens,
		ContextWindow: cfg.Agents.Defaults.ContextWindow,
	}
}

// options are the catalogue options for cfg. The functions are package
// functions, so their identity is the same in every catalogue (Adopt compares
// them).
func (b *CatalogBuilder) options(cfg *config.Config) []models.Option {
	b.mu.Lock()
	store := b.store
	b.mu.Unlock()
	return []models.Option{
		models.WithSelectionStore(store),
		models.WithBaseEnv(childenv.CLI),
		models.WithFileModesFor(responseLogFileModes),
		models.WithCooldown(modelspec.Cooldown(cfg.Cooldown)),
		models.WithObserver(b.alerts),
		models.WithLogger(logger.SDK()),
		models.WithLogContent(cfg.Logging.LogMessageContent),
		models.WithClock(b.clock),
		models.WithDeclinedMessage(declinedMessage),
		models.WithIgnoredBypassNotice(ignoredBypassNotice),
		models.WithLimitDefaults(limitDefaults(cfg)),
	}
}

// Build builds the catalogue of cfg: every provider and every enabled model,
// a human client registered for each model on the human protocol, and one
// single-model set per model (ModelSet). A provider or model the catalogue
// refuses is left out and logged; configuration validation refuses them
// before they get here.
func (b *CatalogBuilder) Build(cfg *config.Config) *models.Catalog {
	c := models.New(b.options(cfg)...)
	for i := range cfg.Providers {
		p := cfg.Providers[i]
		if err := c.AddProvider(p); err != nil {
			logger.WarnCF("providers", "provider left out of the model catalogue",
				map[string]any{"provider": p.Name, "error": err.Error()})
		}
	}
	for _, m := range catalogModels(cfg) {
		if err := c.Add(m); err != nil {
			logger.WarnCF("providers", "model left out of the model catalogue",
				map[string]any{"model": m.ModelName, "error": err.Error()})
			continue
		}
		if p, err := cfg.GetProvider(m.Provider); err == nil && p.Protocol == config.HumanProtocol {
			if err := c.RegisterClient(m.ModelName, b.human(m.ModelName, time.Duration(m.RequestTimeout)*time.Second)); err != nil {
				logger.WarnCF("providers", "human model has no client",
					map[string]any{"model": m.ModelName, "error": err.Error()})
			}
		}
		if err := c.Set(modelSetID(m.ModelName)).Assign(models.Chat, m.ModelName, 0); err != nil {
			logger.WarnCF("providers", "model has no single-model set",
				map[string]any{"model": m.ModelName, "error": err.Error()})
		}
	}
	return c
}

// human returns the client of the human model name. The same instance is
// handed to every catalogue while the model's settings stay the same, so
// Adopt keeps it; the builder owns it (the catalogue never closes a
// registered client, and a human client holds nothing to close).
func (b *CatalogBuilder) human(name string, timeout time.Duration) *HumanProvider {
	b.mu.Lock()
	defer b.mu.Unlock()
	if hp, ok := b.humans[name]; ok && hp.label == name && hp.timeout == timeout {
		return hp
	}
	hp := NewHumanProvider(name, timeout)
	b.humans[name] = hp
	return hp
}

// HumanClient returns the client of the human model name, as the last
// catalogue built registered it.
func (b *CatalogBuilder) HumanClient(name string) (*HumanProvider, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	hp, ok := b.humans[name]
	return hp, ok
}

// modelSetPrefix marks the single-model sets; no agent id contains a NUL.
const modelSetPrefix = "\x00model\x00"

func modelSetID(name string) string { return modelSetPrefix + name }

// ModelSet is the set that holds model name alone, for Chat: a call to one
// named model (a classifier, a summarization model) with the catalogue's
// health, cooldown and client. Its chain is empty when the catalogue does not
// have the model.
func ModelSet(c *models.Catalog, name string) *models.Set {
	return c.Set(modelSetID(strings.TrimSpace(name)))
}

// catalogDigest is what a catalogue built from a configuration depends on.
type catalogDigest struct {
	Providers     []config.Provider       `json:"providers"`
	Models        []config.ModelConfig    `json:"models"`
	Modes         map[string]fsmode.Modes `json:"modes"` // response log file → its modes
	Cooldown      config.CooldownConfig   `json:"cooldown"`
	LogContent    bool                    `json:"log_content"`
	LimitDefaults modelspec.LimitDefaults `json:"limit_defaults"`
	BaseEnv       []string                `json:"base_env"`
	Assignments   any                     `json:"assignments"`
}

// Digest summarizes everything the catalogue built from cfg depends on: the
// providers, the models as the catalogue gets them, the modes of their
// response log files, the cooldown policy, the client options and the
// assignments (what assignments returns). Two configurations with the same
// digest build the same catalogue, so a reload whose digest is unchanged
// keeps the running one, and with it the cooldowns.
func Digest(cfg *config.Config, assignments any) string {
	ms := catalogModels(cfg)
	modes := make(map[string]fsmode.Modes)
	for i := range ms {
		if f := ms[i].ResponseLogFile; f != "" {
			modes[f] = responseLogFileModes(f)
		}
	}
	d := catalogDigest{
		Providers:     cfg.Providers,
		Models:        ms,
		Modes:         modes,
		Cooldown:      cfg.Cooldown,
		LogContent:    cfg.Logging.LogMessageContent,
		LimitDefaults: limitDefaults(cfg),
		BaseEnv:       childenv.CLI(),
		Assignments:   assignments,
	}
	data, err := json.Marshal(d)
	if err != nil {
		// Every field marshals; should one ever not, the catalogue is
		// rebuilt rather than kept stale.
		return "unmarshalable:" + err.Error()
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// responseLogFileModes are the modes a model's response_log_file, and any
// folders created for it, take: those of the security.permissions area it
// lies in (the logs area for a file under <CLAW_HOME>/logs), from the policy
// the current config installed.
func responseLogFileModes(path string) fsmode.Modes {
	m := responseLogModes(path)
	return fsmode.Modes{Dir: m.Dir, File: m.File}
}

// declinedMessage is the text of the error a CLI running without its
// permission bypass returns when it declined to use tools. The reply that
// carries it names the agent and the model (renderTurnErrorFor, attemptText).
func declinedMessage(d models.Declined) string {
	msg := "The " + d.Label + " declined to use tools"
	if len(d.Denied) > 0 {
		msg += " (" + strings.Join(d.Denied, ", ") + ")"
	}
	return msg + ". Tick *Allow CLI to bypass restrictions* for this CLI on the Providers page, or allow the tools in the CLI's own settings."
}

// ignoredBypassNotice logs the permission-bypass flags a CLI model's
// extra_args carry that are not passed.
func ignoredBypassNotice(n models.IgnoredBypass) {
	config.WarnIgnoredBypassArgs(n.Protocol, n.Flags)
}
