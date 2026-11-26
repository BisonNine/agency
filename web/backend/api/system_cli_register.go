// Agency
// License: MIT

package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"

	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/internal/audit"
	"go.bison9.com/agency/internal/mcpreg"
	"go.bison9.com/agency/logger"
)

// Registering the tool server in each CLI (what set-mcp.sh does), from the
// CLI providers list. The command run is fixed per CLI (internal/mcpreg) and
// the URL is this instance's own internal MCP endpoint; the request only
// names which CLI.

// cliRegistrations is the answer of GET /api/system/clis/registration.
type cliRegistrations struct {
	// URL is the endpoint each CLI is registered with; "" when the MCP host
	// is off.
	URL  string          `json:"url"`
	CLIs []mcpreg.Status `json:"clis"`
}

// cliRegisterError is a refused or failed registration: one sentence, and
// the CLI's state when it is known.
type cliRegisterError struct {
	Error  string         `json:"error"`
	Status *mcpreg.Status `json:"status,omitempty"`
}

var (
	defaultRegistrarOnce sync.Once
	defaultRegistrar     *mcpreg.Registrar
)

// cliRegistrar is the handler's Registrar: the one tests set, else one for
// the service user's CLIs, shared so registrations of one CLI are serialised.
func (h *Handler) cliRegistrar() *mcpreg.Registrar {
	if h.registrar != nil {
		return h.registrar
	}
	defaultRegistrarOnce.Do(func() { defaultRegistrar = mcpreg.New() })
	return defaultRegistrar
}

// handleCLIRegistrations reports, for every CLI of the catalogue, whether the
// tool server is registered in it.
//
//	GET /api/system/clis/registration
func (h *Handler) handleCLIRegistrations(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.currentConfig()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Failed to load config: "+err.Error())
		return
	}
	url := mcpreg.InternalURL(cfg)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, cliRegistrations{URL: url, CLIs: h.cliRegistrar().CheckAll(r.Context(), cliCommands(cfg), url)})
}

// handleCLIRegister registers the tool server in one CLI, and answers with
// the CLI's state read back.
//
//	POST /api/system/clis/{protocol}/register   {"replace": true}
//
// An entry pointing at another address is replaced only with replace: the
// state is read again here, so a page showing an old state cannot replace one
// silently.
func (h *Handler) handleCLIRegister(w http.ResponseWriter, r *http.Request) {
	agent := modelspec.CLIAgentByProtocol(r.PathValue("protocol"))
	if agent == nil {
		writeJSONError(w, http.StatusNotFound, "Unknown CLI.")
		return
	}
	var body struct {
		Replace bool `json:"replace"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "Invalid request.")
		return
	}
	cfg, err := h.currentConfig()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Failed to load config: "+err.Error())
		return
	}
	url := mcpreg.InternalURL(cfg)

	st, err := h.cliRegistrar().Register(r.Context(), agent.Protocol, cliCommand(cfg, agent.Protocol), url, body.Replace)
	switch {
	case err == nil:
		logger.InfoCF("api", "Tool server registered in CLI", map[string]any{
			"cli": agent.Protocol, "url": url, "replace": body.Replace, "state": string(st.State),
		})
		recordCLIRegister(r, agent.Protocol, url, body.Replace, audit.OutcomeOK)
		writeJSON(w, http.StatusOK, st)
	case errors.Is(err, mcpreg.ErrNotInstalled):
		writeJSON(w, http.StatusConflict, cliRegisterError{Error: agent.Label + " is not installed.", Status: &st})
	case errors.Is(err, mcpreg.ErrHostOff):
		writeJSON(w, http.StatusConflict, cliRegisterError{Error: "Could not register " + agent.Label + ": the tool server is off.", Status: &st})
	case errors.Is(err, mcpreg.ErrReplaceRequired):
		writeJSON(w, http.StatusConflict, cliRegisterError{Error: agent.Label + " is registered to a different address: " + st.Address + ".", Status: &st})
	default:
		logger.WarnCF("api", "Tool server registration in CLI failed", map[string]any{
			"cli": agent.Protocol, "url": url, "error": err.Error(),
		})
		recordCLIRegister(r, agent.Protocol, url, body.Replace, audit.OutcomeError)
		writeJSON(w, http.StatusInternalServerError, cliRegisterError{Error: "Could not register " + agent.Label + "."})
	}
}

// recordCLIRegister writes the audit row of a registration that ran: who,
// from where, which CLI and the address.
func recordCLIRegister(r *http.Request, protocol, url string, replace bool, outcome string) {
	store := audit.Default()
	if store == nil {
		return
	}
	details, err := json.Marshal(map[string]any{"url": url, "replace": replace})
	if err != nil {
		details = []byte("{}")
	}
	store.Record(audit.Event{
		Kind:    audit.KindCLIRegister,
		Actor:   audit.ActorFromContext(r.Context()),
		Sender:  clientIP(r),
		Summary: protocol,
		Details: string(details),
		Outcome: outcome,
	})
}
