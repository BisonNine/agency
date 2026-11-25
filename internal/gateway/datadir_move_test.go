// Agency
// License: MIT

package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.bison9.com/agency/global"
	"go.bison9.com/agency/servicetoken"
	"go.bison9.com/agency/tools/shell"
)

// TestDataDir_MovedByHand builds a data directory through the normal code
// paths (agents, a conversation, a cron job, the self-signed certificate
// pair, a sub-agent task record, an own knowledge base, Maestro data, a
// background shell job and a WebUI config save), stops, renames the directory to another name in
// another place, and starts against the new location: everything is found
// and works, and no file in the directory names the old location.
//
// The one exception is logs/: log lines record what happened where, at the
// time, and nothing reads a path back from them.
func TestDataDir_MovedByHand(t *testing.T) {
	// Each start here is a new process for the shell jobs.
	shell.Host.Reopen()
	tlsPort := freeLoopbackPort(t)
	mcpAddr := freeAddr(t)
	extra := fmt.Sprintf(`, "mcp_host": {"enabled": true, "listen": %q, "external_tools": ["*"]}`, mcpAddr)
	b := bootGatewayWith(t, extra, func(s string) string {
		s = strings.Replace(s, `"tls": {"mode": "off"}`, fmt.Sprintf(`"tls": {"mode": "localhost"}, "tls_port": %d`, tlsPort), 1)
		s = strings.Replace(s, `"list": [{"id": "main", "default": true}]`,
			`"list": [{"id": "main", "default": true, "tools": ["cron_schedule", "agent_spawn", "agent_status", "file_write", "file_read_lines", "shell_exec"], "knowledge": {"enabled": true}, "maestro": {"enabled": true}}]`, 1)
		// A cron job needs the agent's default chat.
		return strings.Replace(s, `"agents": {`, `"bindings": [{"agent_id": "main", "default": true, "match": {"channel": "webui"}, "deliver_to": "alice"}], "agents": {`, 1)
	})
	oldHome := b.g.home
	const svc = "SVC-main-move"
	if err := servicetoken.Save(servicetoken.Path(oldHome), map[string]string{"main": svc}); err != nil {
		t.Fatal(err)
	}
	b.tokens <- struct{}{}
	waitFor(t, "the service token", func() bool {
		_, ok := b.services.SessionTokens.Resolve(svc)
		return ok
	})
	call := func(name string, args map[string]any) string {
		t.Helper()
		var out struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		raw := mcpRPC(t, "http://"+mcpAddr+"/mcp", svc, "tools/call", map[string]any{"name": name, "arguments": args})
		if err := json.Unmarshal(raw, &out); err != nil || out.IsError || len(out.Content) == 0 {
			t.Fatalf("%s: %s (%v)", name, raw, err)
		}
		return out.Content[0].Text
	}

	// Build the data directory.
	call("cron_schedule", map[string]any{"action": "add", "message": "Water the plants.", "every_seconds": 3600})
	call("kb_put", map[string]any{
		"kb": "notes", "concept_id": "alpha",
		"frontmatter": map[string]any{"type": "note", "title": "Alpha"}, "body": "Alpha body.",
	})
	call("maestro_project_create", map[string]any{"name": "garden", "title": "Garden", "disclaimer_template": "none"})
	spawned := call("agent_spawn", map[string]any{"task": "Say hi.", "mode": "wait"})
	taskID := regexp.MustCompile(`uuid ([0-9a-f-]{36})`).FindStringSubmatch(spawned)
	if taskID == nil {
		t.Fatalf("no task uuid in %q", spawned)
	}
	call("file_write", map[string]any{"path": "files/a.txt", "content": "kept"})
	jobID := regexp.MustCompile(`job (j[0-9a-f]{8})`).FindStringSubmatch(
		call("shell_exec", map[string]any{"command": "echo job-done", "background": true, "notify": false, "working_dir": "files"}))
	if jobID == nil {
		t.Fatal("no shell job id")
	}
	if got := call("shell_ctrl", map[string]any{"action": "wait", "id": jobID[1]}); !strings.Contains(got, "exited with code 0") {
		t.Fatalf("shell job: %s", got)
	}
	if _, err := b.al.ProcessDirect(t.Context(), "Remember the marigolds.", "agent:main:main"); err != nil {
		t.Fatal(err)
	}
	certBefore := b.services.TLSCerts.Info()
	// A WebUI save: the config is written through the normal save path, and
	// the base_dir this config names in full is written relative.
	b.g.login(t)
	if code := b.g.patch(t, "/api/config", `{"operator": {"name": "Alice"}, "bindings": []}`); code != http.StatusOK {
		t.Fatalf("config save = %d", code)
	}
	b.fatal.requestRestart()
	if err := b.stop(t); exitCode(err) != exitCodeServiceDied {
		t.Fatalf("first run = %v", err)
	}

	// Move the directory by hand to another name in another place.
	newHome := filepath.Join(t.TempDir(), "moved-data")
	if err := os.Rename(oldHome, newHome); err != nil {
		t.Fatal(err)
	}
	t.Setenv(global.EnvVarHome, newHome)
	b.g.home = newHome
	shell.Host.Reopen()
	b = bootGatewayIn(t, b.g)
	b.g.waitReady(t)

	if got, want := b.cfg.BaseDir(), filepath.Join(newHome, "agents"); got != want {
		t.Errorf("base dir = %s, want %s", got, want)
	}
	if got := b.services.TLSCerts.Info(); got.Fingerprint != certBefore.Fingerprint || !strings.HasPrefix(got.CertFile, newHome+string(filepath.Separator)) {
		t.Errorf("certificate after the move = %s (%s), want the same one (%s) from the new location", got.CertFile, got.Fingerprint, certBefore.Fingerprint)
	}
	waitFor(t, "the service token", func() bool {
		_, ok := b.services.SessionTokens.Resolve(svc)
		return ok
	})
	if got := call("cron_schedule", map[string]any{"action": "list"}); !strings.Contains(got, "Water the plants.") {
		t.Errorf("cron jobs after the move: %s", got)
	}
	if got := call("kb_get", map[string]any{"kb": "notes", "concept_id": "alpha"}); !strings.Contains(got, "Alpha body.") {
		t.Errorf("knowledge base after the move: %s", got)
	}
	if got := call("maestro_project_get", map[string]any{"name": "garden"}); !strings.Contains(got, `"Garden"`) {
		t.Errorf("Maestro project after the move: %s", got)
	}
	if got := call("agent_status", map[string]any{"uuid": taskID[1]}); !strings.Contains(got, "done") {
		t.Errorf("task record after the move: %s", got)
	}
	if got := call("file_read_lines", map[string]any{"path": "files/a.txt"}); !strings.Contains(got, "kept") {
		t.Errorf("workspace file after the move: %s", got)
	}
	if got := call("shell_ctrl", map[string]any{"action": "list"}); !strings.Contains(got, jobID[1]+" exited") {
		t.Errorf("shell job record after the move: %s", got)
	}
	// The conversation is found: the model sees the earlier message, and
	// the workspace the prompt names is the new one.
	if _, err := b.al.ProcessDirect(t.Context(), "What did I ask?", "agent:main:main"); err != nil {
		t.Fatal(err)
	}
	reqs := b.g.stub.Requests()
	last := reqs[len(reqs)-1].Text()
	if !strings.Contains(last, "Remember the marigolds.") {
		t.Error("the conversation was not found after the move")
	}
	if !strings.Contains(last, filepath.Join(newHome, "agents", "default")) || strings.Contains(last, oldHome) {
		t.Error("the prompt does not name the moved workspace")
	}

	b.fatal.requestRestart()
	if err := b.stop(t); exitCode(err) != exitCodeServiceDied {
		t.Fatalf("second run = %v", err)
	}
	for _, f := range filesNaming(t, newHome, oldHome) {
		t.Errorf("%s names the old location %s", f, oldHome)
	}
}

// filesNaming lists the files under dir, logs/ left out, whose bytes hold
// text.
func filesNaming(t *testing.T, dir, text string) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		if d.IsDir() {
			if rel == "logs" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if bytes.Contains(data, []byte(text)) {
			hits = append(hits, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

// patch sends a PATCH with a JSON body and returns the status.
func (g *e2eGateway) patch(t *testing.T, path, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, g.url(path), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Error(cerr)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		msg, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			t.Error(rerr)
		}
		t.Logf("PATCH %s: %s", path, msg)
	}
	return resp.StatusCode
}
