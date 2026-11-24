// Agency
// License: MIT

package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/fsmode"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/internal/childenv"
	"go.bison9.com/agency/internal/perms"
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
	// quiet builds catalogues that log nothing: for a check that builds one
	// beside the running catalogue, whose build has logged it all.
	quiet bool
}

// newQuietCatalogBuilder is a builder whose catalogues log nothing (the
// models and providers they leave out, ignored bypass flags, client logs):
// for checking a configuration without repeating what the running
// catalogue's build logged.
func newQuietCatalogBuilder() *CatalogBuilder {
	b := NewCatalogBuilder(nil, nil)
	b.quiet = true
	return b
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
func catalogModels(cfg *config.Config) []modelspec.Model {
	out := make([]modelspec.Model, 0, len(cfg.Models))
	for i := range cfg.Models {
		m := cfg.Models[i]
		if !m.Enabled {
			continue
		}
		if m.Workspace == "" && cfg.DataDir() != "" {
			m.Workspace = cfg.CLIPath()
		} else {
			m.Workspace = config.ResolvePath(cfg.DataDir(), m.Workspace)
		}
		m.ResponseLogFile = config.ResolvePath(cfg.DataDir(), m.ResponseLogFile)
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
	if b.quiet {
		return []models.Option{
			models.WithBaseEnv(childenv.CLI),
			models.WithFileModesFor(responseLogFileModes),
			models.WithIgnoredBypassNotice(func(models.IgnoredBypass) {}),
		}
	}
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
			b.warn("provider left out of the model catalogue",
				map[string]any{"provider": p.Name, "error": err.Error()})
		}
	}
	for _, m := range catalogModels(cfg) {
		if err := c.Add(m); err != nil {
			b.warn("model left out of the model catalogue",
				map[string]any{"model": m.ModelName, "error": err.Error()})
			continue
		}
		if p, err := cfg.GetProvider(m.Provider); err == nil && p.Protocol == config.HumanProtocol {
			if err := c.RegisterClient(m.ModelName, b.human(m.ModelName, time.Duration(m.RequestTimeout)*time.Second)); err != nil {
				b.warn("human model has no client",
					map[string]any{"model": m.ModelName, "error": err.Error()})
			}
		}
		if err := c.Set(modelSetID(m.ModelName)).Assign(models.Chat, m.ModelName, 0); err != nil {
			b.warn("model has no single-model set",
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
	Providers     []modelspec.Provider    `json:"providers"`
	Models        []modelspec.Model       `json:"models"`
	Modes         map[string]fsmode.Modes `json:"modes"` // response log file → its modes
	Cooldown      config.CooldownConfig   `json:"cooldown"`
	LogContent    bool                    `json:"log_content"`
	LimitDefaults modelspec.LimitDefaults `json:"limit_defaults"`
	BaseEnv       []string                `json:"base_env"`
}

// Digest summarizes everything the catalogue built from cfg depends on: the
// providers, the models as the catalogue gets them, the modes of their
// response log files, the cooldown policy and the client options. Two
// configurations with the same digest build the same catalogue, so a reload
// whose digest is unchanged keeps the running one, and with it the
// cooldowns; the agents' lists are replaced in it (Set.ReplaceAll).
func Digest(cfg *config.Config) string {
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
// folders created for it, take: those of the security.permissions area it lies
// in (the logs area for a file under <CLAW_HOME>/logs), from the policy the
// current config installed. They are read when a client is built, and the
// digest compares them, so a changed mode rebuilds the client on the next
// reload.
func responseLogFileModes(path string) fsmode.Modes {
	m := perms.CurrentPolicy().ModesFor(path)
	return fsmode.Modes{Dir: m.Dir, File: m.File}
}

// LogBypassEnabled writes one INFO line per CLI provider running with its
// permission-bypass flag, so a startup log states what the CLI may do. A CLI
// running without it is not reported here: it applies its own permission
// settings, as directed, and a refusal reaches the user through the
// declined-tools error.
func LogBypassEnabled(cfg *config.Config) {
	if cfg == nil {
		return
	}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if p.BypassRestrictions && modelspec.IsCLIProtocol(p.Protocol) {
			logger.InfoCF("provider", "Allow CLI to bypass restrictions is on: the CLI can run commands and edit files anywhere the service user can, without asking",
				map[string]any{"provider": p.Name, "protocol": p.Protocol})
		}
	}
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

// defaultModelProblem says why the agents' default model (the first of
// agents.defaults.models) cannot be called, or nil when it can: no model
// configured, the model not among the enabled ones, its provider missing, or
// its client refused by c.
func defaultModelProblem(cfg *config.Config, c *models.Catalog) error {
	model := cfg.Agents.Defaults.DefaultModelName()
	if len(cfg.Models) == 0 {
		return errors.New("no models configured. Please add entries to models in your config")
	}
	modelCfg, err := cfg.GetModelConfig(model)
	if err != nil {
		return fmt.Errorf("model %q not found in models: %w", model, err)
	}
	if _, provErr := cfg.GetProvider(modelCfg.Provider); provErr != nil {
		return fmt.Errorf("model %q: %w", model, provErr)
	}
	_, release, clientErr := c.Client(modelCfg.ModelName)
	if clientErr != nil {
		return fmt.Errorf("failed to create provider for model %q: %w", model, clientProblem(clientErr))
	}
	release()
	return nil
}

// CheckDefaultModel is defaultModelProblem on a catalogue built quietly for
// cfg and closed afterwards: for a check made beside (or before) the running
// catalogue, which logs nothing the running one's build does. The check
// makes no call, so the close has nothing to wait for.
func CheckDefaultModel(ctx context.Context, cfg *config.Config) error {
	c := newQuietCatalogBuilder().Build(cfg)
	return errors.Join(defaultModelProblem(cfg, c), c.Close(ctx))
}

// warn logs a build problem, unless the builder is quiet.
func (b *CatalogBuilder) warn(msg string, fields map[string]any) {
	if !b.quiet {
		logger.WarnCF("providers", msg, fields)
	}
}

// clientProblem is the text operators have always read for a model whose
// client cannot be built: the cause, named by its provider, without the
// catalogue's wording around it.
func clientProblem(err error) error {
	ce, ok := errors.AsType[*models.ClientError](err)
	if !ok {
		return err
	}
	switch {
	case errors.Is(err, models.ErrNoClient) && modelspec.TaskOnlyProtocol(ce.Protocol):
		return fmt.Errorf("provider %q: protocol %q serves task models only (speech), not chat", ce.Provider, ce.Protocol)
	case errors.Is(err, models.ErrNoClient):
		return fmt.Errorf("provider %q: unknown protocol %q", ce.Provider, ce.Protocol)
	case errors.Is(ce.Err, modelspec.ErrModelRequired):
		return ce.Err
	default:
		return fmt.Errorf("provider %q: %w", ce.Provider, ce.Err)
	}
}
