package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/config"
)

func TestHandleListCLIs(t *testing.T) {
	h := NewHandler("")
	req := httptest.NewRequest(http.MethodGet, "/api/system/clis", nil)
	rec := httptest.NewRecorder()
	h.handleListCLIs(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var got []cliInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != len(modelspec.CLIAgents) {
		t.Fatalf("got %d entries, want %d", len(got), len(modelspec.CLIAgents))
	}
	for i, c := range got {
		if c.Protocol != modelspec.CLIAgents[i].Protocol || c.Label != modelspec.CLIAgents[i].Label || c.Binary != modelspec.CLIAgents[i].Binary {
			t.Errorf("entry %d = %+v, want protocol/label/binary %q/%q/%q",
				i, c, modelspec.CLIAgents[i].Protocol, modelspec.CLIAgents[i].Label, modelspec.CLIAgents[i].Binary)
		}
		// An installed CLI must report its resolved path; a missing one must not
		// claim a path. (Version is best-effort, so it's not asserted.)
		if c.Installed && c.Path == "" {
			t.Errorf("entry %d installed but has no path", i)
		}
		if !c.Installed && c.Path != "" {
			t.Errorf("entry %d not installed but has path %q", i, c.Path)
		}
	}
}

// A provider's configured command is the binary the list looks for: a CLI
// that is not on the PATH but has its path set is installed, and a set path
// that does not exist is not, whatever the PATH holds.
func TestHandleListCLIs_UsesProviderCommand(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "opencode-here")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 1.2.3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())

	list := func(command string) cliInfo {
		t.Helper()
		cfg := config.DefaultConfig()
		cfg.Providers = []modelspec.Provider{{Name: "OpenCode", Protocol: "opencode-cli", Command: command}}
		cfg.Models = nil
		h, _ := writeConfig(t, cfg)
		rec := httptest.NewRecorder()
		h.handleListCLIs(rec, httptest.NewRequest(http.MethodGet, "/api/system/clis", nil))
		var got []cliInfo
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, c := range got {
			if c.Protocol == "opencode-cli" {
				return c
			}
		}
		t.Fatal("no opencode-cli entry")
		return cliInfo{}
	}

	if c := list(bin); !c.Installed || c.Path != bin || c.Command != bin {
		t.Errorf("with the path set: installed, path, command = %v, %q, %q, want true, %q, %q", c.Installed, c.Path, c.Command, bin, bin)
	}
	missing := filepath.Join(dir, "nothing")
	if c := list(missing); c.Installed || c.Path != "" || c.Command != missing {
		t.Errorf("with a missing path set: installed, path, command = %v, %q, %q, want false, \"\", %q", c.Installed, c.Path, c.Command, missing)
	}
	if c := list(""); c.Installed || c.Command != "" {
		t.Errorf("with no command and an empty PATH: installed, command = %v, %q, want false, \"\"", c.Installed, c.Command)
	}
}
