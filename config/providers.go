package config

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/app"
	"go.bison9.com/agency/logger"
)

// rrCounter is a global counter for round-robin load balancing across models.
var rrCounter atomic.Uint64

// CooldownConfig sets, per HTTP-status category, how long a model that keeps
// failing is taken out of rotation (the "settled" cooldown reached after the
// short 1/3/5-minute escalation on the first three consecutive failures). Each
// value is in MINUTES: 0 uses the built-in default; a negative value disables
// cooldown for that category (the model is never taken out for it). 413
// (context-too-large) and errors with no HTTP status never cool — they are
// per-request or transient.
type CooldownConfig struct {
	// BillingAuthMinutes covers HTTP 401, 402, 403 (auth / out-of-credits).
	BillingAuthMinutes int `json:"billing_auth_minutes,omitempty" env:"CLAW_COOLDOWN_BILLING_AUTH_MINUTES"`
	// RateLimitMinutes covers HTTP 429.
	RateLimitMinutes int `json:"rate_limit_minutes,omitempty" env:"CLAW_COOLDOWN_RATE_LIMIT_MINUTES"`
	// BadRequestMinutes covers HTTP 400.
	BadRequestMinutes int `json:"bad_request_minutes,omitempty" env:"CLAW_COOLDOWN_BAD_REQUEST_MINUTES"`
	// ClientErrorMinutes covers other 4xx (404, 408, …; not 400/401/402/403/429/413).
	ClientErrorMinutes int `json:"client_error_minutes,omitempty" env:"CLAW_COOLDOWN_CLIENT_ERROR_MINUTES"`
	// ServerErrorMinutes covers 5xx.
	ServerErrorMinutes int `json:"server_error_minutes,omitempty" env:"CLAW_COOLDOWN_SERVER_ERROR_MINUTES"`
}

// BillingAuth is the cooldown for HTTP 401, 402 and 403; 0 means never.
func (c CooldownConfig) BillingAuth() time.Duration { return modelspec.Cooldown(c).BillingAuth() }

// RateLimit is the cooldown for HTTP 429; 0 means never.
func (c CooldownConfig) RateLimit() time.Duration { return modelspec.Cooldown(c).RateLimit() }

// BadRequest is the cooldown for HTTP 400; 0 means never.
func (c CooldownConfig) BadRequest() time.Duration { return modelspec.Cooldown(c).BadRequest() }

// ClientError is the cooldown for the other 4xx statuses; 0 means never.
func (c CooldownConfig) ClientError() time.Duration { return modelspec.Cooldown(c).ClientError() }

// ServerError is the cooldown for 5xx statuses; 0 means never.
func (c CooldownConfig) ServerError() time.Duration { return modelspec.Cooldown(c).ServerError() }

// ValidateModel checks one model's own fields (modelspec's rules). It is the
// one place model validation reaches the operator: every path that validates
// a model (the WebUI's model endpoints, a config save or reload, and the
// startup prune) goes through it, so the reserved extra_body key error names
// the product as it always has.
func ValidateModel(m *modelspec.Model) error {
	err := m.Validate()
	if rk, ok := errors.AsType[*modelspec.ReservedBodyKeyError](err); ok {
		return fmt.Errorf("model %q: extra_body key %q collides with a request field %s manages",
			rk.Model, rk.Key, app.ShortName())
	}
	return err
}

// HasCLIProvider reports whether any enabled model in ModelList uses a
// *-cli protocol (claude-cli, codex-cli, gemini-cli). Those CLIs rely on
// the MCP host to call claw's tools natively.
func (c *Config) HasCLIProvider() bool {
	for i := range c.Models {
		if !c.Models[i].Enabled {
			continue
		}
		prov, err := c.GetProvider(c.Models[i].Provider)
		if err != nil {
			continue
		}
		if modelspec.IsCLIProtocol(prov.Protocol) {
			return true
		}
	}
	return false
}

// GetModelConfig returns the ModelConfig for the given model name.
// If multiple configs exist with the same model_name, it uses round-robin
// selection for load balancing. Returns an error if the model is not found.
func (c *Config) GetModelConfig(modelName string) (*modelspec.Model, error) {
	matches := c.findMatches(modelName)
	if len(matches) == 0 {
		return nil, fmt.Errorf("model %q not found in models or providers", modelName)
	}
	if len(matches) == 1 {
		return &matches[0], nil
	}

	// Multiple configs - use round-robin for load balancing
	idx := rrCounter.Add(1) % uint64(len(matches))
	return &matches[idx], nil
}

// findMatches finds all ModelConfig entries with the given model_name.
func (c *Config) findMatches(modelName string) []modelspec.Model {
	var matches []modelspec.Model
	for i := range c.Models {
		if c.Models[i].ModelName == modelName && c.Models[i].Enabled {
			matches = append(matches, c.Models[i])
		}
	}
	return matches
}

// dropOtherProtocolFields clears the fields a provider's protocol never
// uses, so a value left over from another protocol is not written back on
// save: base_url on a CLI provider (the factory builds a CLI from its
// command alone), and command and bypass_restrictions, which only a CLI
// reads, on any other.
func (c *Config) dropOtherProtocolFields() {
	for i := range c.Providers {
		p := &c.Providers[i]
		if modelspec.IsCLIProtocol(p.Protocol) {
			p.BaseURL = ""
		} else {
			p.Command = ""
			p.BypassRestrictions = false
		}
	}
}

// GetProvider resolves a provider by name. The lookup is case-sensitive on the
// configured Name.
func (c *Config) GetProvider(name string) (*modelspec.Provider, error) {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i], nil
		}
	}
	return nil, fmt.Errorf("provider %q not found", name)
}

// ValidateProviders checks that provider names are unique and non-empty, each
// protocol is recognised, and HTTP protocols carry a base_url.
func (c *Config) ValidateProviders() error {
	seen := make(map[string]struct{}, len(c.Providers))
	for i := range c.Providers {
		p := &c.Providers[i]
		if strings.TrimSpace(p.Name) == "" {
			return fmt.Errorf("providers[%d]: name is required", i)
		}
		if _, dup := seen[p.Name]; dup {
			return fmt.Errorf("providers[%d]: duplicate provider name %q", i, p.Name)
		}
		seen[p.Name] = struct{}{}
		if err := p.ValidateProtocol(); err != nil {
			return fmt.Errorf("provider %q: %w", p.Name, err)
		}
	}
	return nil
}

// ValidateModels validates all ModelConfig entries in the models,
// including that each model's provider reference resolves to a configured
// provider. Multiple entries with the same model_name are allowed for load
// balancing.
func (c *Config) ValidateModels() error {
	for i := range c.Models {
		if err := ValidateModel(&c.Models[i]); err != nil {
			return fmt.Errorf("models[%d]: %w", i, err)
		}
		if _, err := c.GetProvider(c.Models[i].Provider); err != nil {
			return fmt.Errorf("models[%d] (%q): %w", i, c.Models[i].ModelName, err)
		}
	}
	return nil
}

// RenameModelReferences repoints every reference to a model alias from oldName
// to newName across agent defaults, per-agent model chains, vision models, and
// the global summarization chain. Used when a model's
// model_name changes via the WebUI so existing references are not orphaned.
// No-op when oldName is empty or unchanged. Mutates in-memory config only.
func (c *Config) RenameModelReferences(oldName, newName string) {
	if oldName == "" || oldName == newName {
		return
	}
	for _, site := range c.modelRefSites() {
		if site.slice != nil {
			renameInSlice(*site.slice, oldName, newName)
		} else {
			*site.scalar = renameScalar(*site.scalar, oldName, newName)
		}
	}
}

func renameScalar(s, oldName, newName string) string {
	if s == oldName {
		return newName
	}
	return s
}

func renameInSlice(ss []string, oldName, newName string) {
	for i := range ss {
		if ss[i] == oldName {
			ss[i] = newName
		}
	}
}

// ValidateProvider validates a single provider (the one at idx) without
// rejecting the whole config because OTHER providers are invalid. It checks the
// provider's own name, protocol, and base_url, plus that its name does not
// collide with another provider. The per-provider WebUI endpoints use this so an
// operator can repair one entry at a time (e.g. during a protocol migration)
// even while other entries remain invalid.
func (c *Config) ValidateProvider(idx int) error {
	if idx < 0 || idx >= len(c.Providers) {
		return fmt.Errorf("provider index %d out of range", idx)
	}
	p := &c.Providers[idx]
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("provider name is required")
	}
	for i := range c.Providers {
		if i != idx && c.Providers[i].Name == p.Name {
			return fmt.Errorf("duplicate provider name %q", p.Name)
		}
	}
	if err := p.ValidateProtocol(); err != nil {
		return fmt.Errorf("provider %q: %w", p.Name, err)
	}
	return nil
}

// PruneInvalid removes providers and models that fail validation, logging a WARN
// for each dropped entry, and returns how many of each were removed. It is the
// lenient counterpart to ValidateProviders/ValidateModels: the gateway calls it
// at startup so a single bad entry (e.g. a stale/unknown protocol, or a model
// pointing at a missing provider) degrades gracefully instead of aborting the
// whole process. It mutates the in-memory config only — it never writes to disk.
func (c *Config) PruneInvalid() (droppedProviders, droppedModels int) {
	seen := make(map[string]struct{}, len(c.Providers))
	valid := make(map[string]struct{}, len(c.Providers))
	keptProviders := make([]modelspec.Provider, 0, len(c.Providers))
	for i := range c.Providers {
		p := c.Providers[i]
		var reason string
		if strings.TrimSpace(p.Name) == "" {
			reason = "name is required"
		} else if _, dup := seen[p.Name]; dup {
			reason = "duplicate provider name"
		} else if err := p.ValidateProtocol(); err != nil {
			reason = err.Error()
		}
		if reason != "" {
			logger.WarnCF("config", "ignoring invalid provider", map[string]any{
				"provider": p.Name,
				"reason":   reason,
			})
			droppedProviders++
			continue
		}
		seen[p.Name] = struct{}{}
		valid[p.Name] = struct{}{}
		keptProviders = append(keptProviders, p)
	}
	c.Providers = keptProviders

	keptModels := make([]modelspec.Model, 0, len(c.Models))
	for i := range c.Models {
		m := c.Models[i]
		if err := ValidateModel(&m); err != nil {
			logger.WarnCF("config", "ignoring invalid model", map[string]any{
				"model":  m.ModelName,
				"reason": err.Error(),
			})
			droppedModels++
			continue
		}
		if _, ok := valid[m.Provider]; !ok {
			logger.WarnCF("config", "ignoring model with unknown provider", map[string]any{
				"model":    m.ModelName,
				"provider": m.Provider,
			})
			droppedModels++
			continue
		}
		keptModels = append(keptModels, m)
	}
	c.Models = keptModels
	return droppedProviders, droppedModels
}

func MergeAPIKeys(apiKey string, apiKeys []string) []string {
	seen := make(map[string]struct{})
	var all []string

	if k := strings.TrimSpace(apiKey); k != "" {
		if _, exists := seen[k]; !exists {
			seen[k] = struct{}{}
			all = append(all, k)
		}
	}

	for _, k := range apiKeys {
		if trimmed := strings.TrimSpace(k); trimmed != "" {
			if _, exists := seen[trimmed]; !exists {
				seen[trimmed] = struct{}{}
				all = append(all, trimmed)
			}
		}
	}

	return all
}
