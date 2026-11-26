// Agency
// License: MIT

package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"runtime"
	"strings"
	"sync"
	"time"

	"go.bison9.com/agency/app"
	"go.bison9.com/agency/channels/matrix"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/internal/mcpreg"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/report"
)

func (h *Handler) registerReportRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/report/pdf", h.handleReportPDF)
	mux.HandleFunc("GET /api/report/assessment", h.handleReportAssessment)
}

// handleReportAssessment returns the identity line and the security
// assessment table as JSON — the same rows the PDF renders, for the Report
// page to show inline. Never cached, for the same reason as the PDF.
func (h *Handler) handleReportAssessment(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.currentConfig()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Failed to load config: "+err.Error())
		return
	}
	env := h.runningEnvironment(cfg.DataDir(), time.Now())
	env.CLIRegistrations = h.enabledCLIRegistrations(r.Context(), cfg)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, report.Assess(r.Context(), cfg, env))
}

// handleReportPDF streams the configuration report for this install: the
// configuration as loaded plus facts about the running process. Served inline
// so the browser renders it, and never cached.
func (h *Handler) handleReportPDF(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.currentConfig()
	if err != nil {
		http.Error(w, "Failed to load config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now()
	env := h.runningEnvironment(cfg.DataDir(), now)
	env.CLIRegistrations = h.enabledCLIRegistrations(r.Context(), cfg)
	rep := report.Collect(r.Context(), cfg, env)

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`inline; filename="%s-report-%s.pdf"`, strings.ToLower(app.Name()), now.Format("2006-01-02-1504")))
	w.Header().Set("Cache-Control", "no-store")
	if err := report.RenderPDF(rep, w); err != nil {
		// Headers may already be out; all that is left is to record it.
		logger.ErrorCF("api", "configuration report render failed", map[string]any{"error": err.Error()})
	}
}

// enabledCLIRegistrations reads whether the tool server is registered in
// each CLI an enabled model runs on, the CLIs Check Up has a row for. Nothing
// is read while the MCP host is off: the rows say so from the config.
func (h *Handler) enabledCLIRegistrations(ctx context.Context, cfg *config.Config) []mcpreg.Status {
	url := mcpreg.InternalURL(cfg)
	if url == "" {
		return nil
	}
	protocols := report.EnabledCLIProtocols(cfg)
	out := make([]mcpreg.Status, len(protocols))
	var wg sync.WaitGroup
	for i, proto := range protocols {
		wg.Go(func() {
			st, err := h.cliRegistrar().Check(ctx, proto, cliCommand(cfg, proto), url)
			if err != nil { // a catalogue CLI the registrar does not know
				st = mcpreg.Status{Protocol: proto, State: mcpreg.NotChecked}
			}
			out[i] = st
		})
	}
	wg.Wait()
	return out
}

// runningEnvironment is reportEnvironment plus what the running listener has
// observed: the last forwarded request from a proxy that is not trusted, and
// the Matrix rooms seen to be encrypted.
func (h *Handler) runningEnvironment(dataDir string, now time.Time) report.Environment {
	env := reportEnvironment(h.configPath, dataDir, now)
	env.ForwardedPeer, env.ForwardedSeen = h.untrustedProxySeen()
	rooms := matrix.EncryptedRooms
	if h.encryptedMatrixRooms != nil {
		rooms = h.encryptedMatrixRooms
	}
	env.EncryptedMatrixRooms = rooms()
	return env
}

// reportEnvironment gathers what the report needs to know about the process.
// Every lookup degrades to "unknown" rather than failing the report.
func reportEnvironment(configPath, dataDir string, now time.Time) report.Environment {
	env := report.Environment{
		ConfigPath: configPath,
		DataDir:    dataDir,
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		Version:    app.Version(),
		Now:        now,
	}
	env.BuildTime, env.GoVersion = app.BuildInfo()
	if exe, err := os.Executable(); err == nil {
		env.Executable = exe
	} else {
		env.Executable = "unknown"
	}
	if host, err := os.Hostname(); err == nil {
		env.Hostname = host
	} else {
		env.Hostname = "unknown"
	}
	env.User, env.Group = "unknown", "unknown"
	if u, err := user.Current(); err == nil {
		env.User = u.Username
		if g, err := user.LookupGroupId(u.Gid); err == nil {
			env.Group = g.Name
		} else {
			env.Group = u.Gid
		}
	}
	return env
}
