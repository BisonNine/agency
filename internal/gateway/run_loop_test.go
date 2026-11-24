// Agency
// License: MIT

package gateway

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/cogmem/store"

	"go.bison9.com/agency/agent"
	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/cogmemhost"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/internal/test/stubprovider"
	"go.bison9.com/agency/servicetoken"
)

// bootedGateway is the gateway's services started the way gatewayCmd starts
// them, with the main loop's channels in the test's hands.
type bootedGateway struct {
	g        *e2eGateway
	cfg      *config.Config
	al       *agent.AgentLoop
	services *gatewayServices
	run      *gatewayRun
	fatal    *fatalNotifier
	alerts   *alertRecorder
	tokens   chan struct{}
	done     chan error
}

// bootGateway starts the services on a temp AGENCY_HOME (extra is spliced into
// the config) and runs the main loop in the background.
func bootGateway(t *testing.T, extra string) *bootedGateway {
	t.Helper()
	return bootGatewayWith(t, extra, nil)
}

// bootGatewayWith is bootGateway with the config text passed through mutate
// first, when it is not nil.
func bootGatewayWith(t *testing.T, extra string, mutate func(string) string) *bootedGateway {
	t.Helper()
	g := newE2EHome(t, extra)
	if mutate != nil {
		g.rewriteConfig(t, mutate)
	}
	return bootGatewayIn(t, g)
}

// bootGatewayIn starts the services on g's AGENCY_HOME as it is and runs the
// main loop in the background.
func bootGatewayIn(t *testing.T, g *e2eGateway) *bootedGateway {
	t.Helper()
	cfgPath := filepath.Join(g.home, "config.json")
	store, cfg, _, err := openBootConfig(g.home, cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	registerToolProviders()
	b := &bootedGateway{g: g, cfg: cfg, alerts: &alertRecorder{}, tokens: make(chan struct{}), done: make(chan error, 1)}
	b.fatal = newFatalNotifier(b.alerts)
	msgBus := bus.NewMessageBus()
	forumHost := agent.NewForumHost()
	classifierHost := agent.NewClassifierHost()
	knowledgeStore, err := newKnowledgeStore()
	if err != nil {
		t.Fatal(err)
	}
	b.al, err = newGatewayAgentLoop(cfg, msgBus, forumHost, classifierHost, b.alerts)
	if err != nil {
		t.Fatal(err)
	}
	b.services, err = setupAndStartServices(cfg, b.al, msgBus, cfgPath, store, b.fatal)
	if err != nil {
		t.Fatal(err)
	}
	b.services.Forum = newForumService(forumHost)
	b.services.Classifiers = newClassifierService(classifierHost)
	b.services.Knowledge = knowledgeStore
	wireGatewayAPI(b.services, b.al, b.fatal, "")
	var stop func()
	b.run, stop = startGatewayRun(cfg, false, b.al, b.services, msgBus, store, &modelRefAlerts{}, b.fatal)
	t.Cleanup(stop)
	b.run.serviceTokenChanges = b.tokens
	go func() { b.done <- b.run.loop(t.Context()) }()
	// The loop subscribes to signals first; once it has, it is running.
	<-g.signals
	return b
}

func (b *bootedGateway) stop(t *testing.T) error {
	t.Helper()
	select {
	case err := <-b.done:
		return err
	case <-time.After(90 * time.Second):
		t.Fatal("main loop did not return")
		return nil
	}
}

// waitFor polls cond until it holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond) // polling a condition, not timing
	}
}

func (r *alertRecorder) titles() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.alerts))
	for _, a := range r.alerts {
		out = append(out, a.Title)
	}
	return out
}

func (r *alertRecorder) has(title string) bool {
	return slices.Contains(r.titles(), title)
}

// TestGatewayRun_ReloadsAndFatal drives the main loop: a config the watcher
// validated is applied, a reload that cannot restart the MCP host alerts, a
// service-token file change reaches the MCP host, and a core service dying
// shuts the gateway down with that failure as the exit reason.
func TestGatewayRun_ReloadsAndFatal(t *testing.T) {
	// A speech model makes the gateway set up transcription at start and on
	// every reload.
	b := bootGateway(t, `, "mcp_host": {"enabled": true, "listen": "127.0.0.1:0"}, "task_models": {"stt": [{"model": "m"}]}`)

	// A watcher reload with a new allowlist is applied to the running loop.
	newCfg := *b.cfg
	newCfg.Gateway.AllowedCIDRs = []string{"10.9.0.0/16"}
	b.run.configReloads <- &newCfg
	waitFor(t, "the reloaded allowlist", func() bool {
		got := b.al.GetConfig().Gateway.AllowedCIDRs
		return len(got) == 1 && got[0] == "10.9.0.0/16"
	})
	if b.alerts.has("Config reload failed") {
		t.Fatalf("a good reload alerted: %v", b.alerts.titles())
	}

	// A service token written by `claw token` is live after the change.
	const svcToken = "SVC-main-e2e"
	if err := servicetoken.Save(servicetoken.Path(b.cfg.DataDir()), map[string]string{"main": svcToken}); err != nil {
		t.Fatal(err)
	}
	b.tokens <- struct{}{}
	waitFor(t, "the service token", func() bool {
		_, ok := b.services.SessionTokens.Resolve(svcToken)
		return ok
	})

	// A reload whose MCP host cannot bind fails and alerts.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cerr := taken.Close(); cerr != nil {
			t.Error(cerr)
		}
	}()
	badCfg := *b.al.GetConfig()
	badCfg.MCPHost.Listen = taken.Addr().String()
	b.run.configReloads <- &badCfg
	waitFor(t, "the reload failure alert", func() bool { return b.alerts.has("Config reload failed") })

	// A core service dying ends the loop with that failure.
	b.fatal.fatalService("http", errors.New("listener closed"))
	err = b.stop(t)
	var died *serviceDiedError
	if !errors.As(err, &died) || died.Service != "http" || exitCode(err) != exitCodeServiceDied {
		t.Fatalf("loop = %v, want the http failure", err)
	}
	if !b.alerts.has("HTTP listener stopped") {
		t.Errorf("alerts = %v", b.alerts.titles())
	}
	if !strings.Contains(err.Error(), "HTTP listener stopped: listener closed") {
		t.Errorf("error text = %q", err)
	}
}

// TestGatewayRun_ReloadAgentsFail: a reload whose agents cannot be built is
// refused, the running services are restarted on the old config, and the
// failure alerts.
func TestGatewayRun_ReloadAgentsFail(t *testing.T) {
	b := bootGateway(t, "")
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	newCfg := *b.cfg
	newCfg.Agents.BaseDir = filepath.Join(blocker, "agents")
	newCfg.Agents.List = []config.AgentConfig{{ID: "bob", Default: true}}
	b.run.configReloads <- &newCfg
	waitFor(t, "the reload failure alert", func() bool { return b.alerts.has("Config reload failed") })
	if got := b.al.GetConfig().Agents.BaseDir; got == newCfg.Agents.BaseDir {
		t.Errorf("the refused config was applied (base dir %s)", got)
	}
	// The services came back: /ready answers again.
	b.g.waitReady(t)
	b.fatal.requestRestart()
	if err := b.stop(t); exitCode(err) != exitCodeServiceDied {
		t.Fatalf("loop = %v", err)
	}
}

// TestGatewayRun_TLSDevicesAndReloadWarnings runs with HTTPS on localhost and
// the device channel: the certificate is loaded and watched, the device API
// reaches the live channel, and a reload carrying invalid listener values, a
// changed bind and new media cleanup settings keeps the gateway running.
func TestGatewayRun_TLSDevicesAndReloadWarnings(t *testing.T) {
	tlsPort, devPort := freeLoopbackPort(t), freeLoopbackPort(t)
	extra := `, "devices": {"enabled": true}`
	b := bootGatewayWith(t, extra, func(s string) string {
		s = strings.Replace(s, `"tls": {"mode": "off"}`, fmt.Sprintf(`"tls": {"mode": "localhost"}, "tls_port": %d`, tlsPort), 1)
		return strings.Replace(s, `"channels": {"webui": {"enabled": true, "token": "e2e-webui-token"}}`,
			fmt.Sprintf(`"channels": {"webui": {"enabled": true, "token": "e2e-webui-token"}, "device": {"enabled": true, "token": "dev-token", "host": "127.0.0.1", "port": %d}}`, devPort), 1)
	})
	if b.services.TLSCerts == nil {
		t.Fatal("no certificate loaded with tls.mode localhost")
	}
	if _, err := os.Stat(filepath.Join(b.g.home, "tls")); err != nil {
		t.Errorf("no certificate written: %v", err)
	}
	b.g.waitReady(t)
	b.g.login(t)

	// The device API reaches the live channel; the secmsg lookup refuses a
	// channel that is not one.
	req, err := http.NewRequest(http.MethodDelete, b.g.url("/api/devices/no-such-device"), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := b.g.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if cerr := resp.Body.Close(); cerr != nil {
		t.Error(cerr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("device remove = %d", resp.StatusCode)
	}
	for _, name := range []string{"webui", "missing"} {
		resp, err := b.g.client.Get(b.g.url("/api/channels/secmsg/" + name + "/link"))
		if err != nil {
			t.Fatal(err)
		}
		if cerr := resp.Body.Close(); cerr != nil {
			t.Error(cerr)
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("secmsg link on %s = %d, want 404", name, resp.StatusCode)
		}
	}

	newCfg := *b.al.GetConfig()
	newCfg.Gateway.AllowedCIDRs = []string{"not-a-cidr"}
	newCfg.Gateway.TrustedProxies = []string{"not-a-proxy"}
	newCfg.Gateway.ExternalURL = "http://[::1"
	newCfg.Gateway.LockoutExempt = []string{"not an address"}
	newCfg.Gateway.Port++
	newCfg.Gateway.TLS.ExtraNames = []string{"claw.example.test"}
	newCfg.Tools.MediaCleanup.Enabled = !newCfg.Tools.MediaCleanup.Enabled
	newCfg.Tools.MediaCleanup.MaxAge = 7
	newCfg.Tools.MediaCleanup.Interval = 3
	oldStore := b.services.MediaStore
	b.run.configReloads <- &newCfg
	waitFor(t, "the new media store", func() bool { return b.al.GetConfig().Tools.MediaCleanup.MaxAge == 7 })
	if b.alerts.has("Config reload failed") {
		t.Fatalf("reload alerted: %v", b.alerts.titles())
	}
	b.g.waitReady(t)
	if b.services.MediaStore == oldStore {
		t.Error("changed media cleanup kept the old store")
	}

	// Regenerating the self-signed certificate re-applies the host policy;
	// the reloaded external URL is unusable, so the previous policy stays.
	if code, body := b.g.post(t, "/api/tls/regenerate"); code != http.StatusOK {
		t.Errorf("regenerate = %d %s", code, body)
	}

	b.fatal.requestRestart()
	if err := b.stop(t); exitCode(err) != exitCodeServiceDied {
		t.Fatalf("loop = %v", err)
	}
}

// TestGatewayRun_CogmemConsolidateTool: the cogmem_consolidate tool, called by
// the model in a turn, queues a consolidation run for the calling agent, and
// the worker for it is built.
func TestGatewayRun_CogmemConsolidateTool(t *testing.T) {
	b := bootGateway(t, "")
	if b.services.CogmemManager == nil {
		t.Fatal("no consolidation manager for a cognitive agent")
	}
	// The agent's turn asks for the tool, then answers once it has run; any
	// other call (the memory worker's) gets an empty JSON object.
	b.g.stub.SetDefault(stubprovider.Reply("{}"))
	b.g.stub.SetHook(func(req stubprovider.Request) *stubprovider.Step {
		if req.Tools == 0 || len(req.Messages) == 0 || !strings.Contains(req.Text(), "consolidate please") {
			return nil
		}
		if req.Messages[len(req.Messages)-1].Role == "tool" {
			step := stubprovider.Reply("queued")
			return &step
		}
		step := stubprovider.CallTool("cogmem_consolidate", `{}`)
		return &step
	})
	reply, err := b.al.ProcessDirect(t.Context(), "consolidate please", "cli:default")
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if !strings.Contains(reply, "queued") {
		t.Errorf("reply = %q", reply)
	}
	// The queued run opens the agent's memory store.
	inst, ok := b.al.GetRegistry().Get("main")
	if !ok {
		t.Fatal("main not registered")
	}
	db := store.DBPath(cogmemhost.Dir(inst.StateDir))
	waitFor(t, "the consolidation worker's store", func() bool {
		_, err := os.Stat(db)
		return err == nil
	})

	b.fatal.requestRestart()
	if err := b.stop(t); exitCode(err) != exitCodeServiceDied {
		t.Fatalf("loop = %v", err)
	}
}
