// ClawEh
// License: MIT

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agency/channels/device"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
)

// deviceMux returns a handler on a fresh test config with the device routes
// registered, a fixed LAN address, and a reload counter.
func deviceMux(t *testing.T) (*Handler, *http.ServeMux, *int) {
	t.Helper()
	h := NewHandler(setupTestEnv(t))
	h.lanIPv4s = func() []string { return []string{"192.168.1.20"} }
	reloads := 0
	h.SetReloadTrigger(func() error { reloads++; return nil })
	mux := http.NewServeMux()
	h.registerDeviceRoutes(mux)
	return h, mux, &reloads
}

func serve(mux http.Handler, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	mux.ServeHTTP(rec, req)
	return rec
}

func decodeMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

func decodeInto(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
}

// savedDevice reads channels.device back from the file the handler saved.
func savedDevice(t *testing.T, h *Handler) config.DeviceChannelConfig {
	t.Helper()
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Channels.Device
}

// POST /api/devices/pair provisions both tokens and enables the listener on
// first use (saved, one reload, QR rendered); a second call keeps the tokens
// and does not reload.
func TestDevicePair_ProvisionsOnceAndRendersQR(t *testing.T) {
	h, mux, reloads := deviceMux(t)

	rec := serve(mux, http.MethodPost, "/api/devices/pair", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	out := decodeMap(t, rec)
	if out["hasToken"] != true || out["enabled"] != true {
		t.Fatalf("hasToken/enabled = %v/%v, want true/true", out["hasToken"], out["enabled"])
	}
	if png, _ := out["qr_png"].(string); !strings.HasPrefix(png, "data:image/png;base64,") {
		t.Fatalf("qr_png = %.40q, want a PNG data URL", png)
	}
	if ascii, _ := out["qr_ascii"].(string); ascii == "" {
		t.Fatal("qr_ascii is empty")
	}
	if *reloads != 1 {
		t.Fatalf("reloads = %d, want 1", *reloads)
	}
	saved := savedDevice(t, h)
	if !saved.Enabled || saved.Token == "" || saved.WordToken == "" {
		t.Fatalf("saved device = enabled %v token %q word %q, want all set", saved.Enabled, saved.Token, saved.WordToken)
	}
	if out["word_token"] != saved.WordToken {
		t.Fatalf("word_token = %v, want the saved %q", out["word_token"], saved.WordToken)
	}

	rec = serve(mux, http.MethodPost, "/api/devices/pair", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("second pair: status = %d: %s", rec.Code, rec.Body.String())
	}
	if *reloads != 1 {
		t.Fatalf("second pair reloaded: reloads = %d, want 1", *reloads)
	}
	if again := savedDevice(t, h); again.Token != saved.Token || again.WordToken != saved.WordToken {
		t.Fatal("second pair changed the saved tokens")
	}
}

// A failing reload is logged; the pairing still answers 200.
func TestDevicePair_ReloadFailureStillAnswers(t *testing.T) {
	h, mux, _ := deviceMux(t)
	h.SetReloadTrigger(func() error { return errors.New("boom") })
	if rec := serve(mux, http.MethodPost, "/api/devices/pair", ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := serve(mux, http.MethodPost, "/api/devices/word-token/regenerate", ""); rec.Code != http.StatusOK {
		t.Fatalf("regenerate: status = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := serve(mux, http.MethodPost, "/api/devices/settings", `{"listen_lan":true}`); rec.Code != http.StatusOK {
		t.Fatalf("settings: status = %d: %s", rec.Code, rec.Body.String())
	}
}

// GET /api/devices/pair reports the pairing without changing anything and
// without rendering a QR.
func TestDevicePairStatus_ReadOnly(t *testing.T) {
	h, mux, reloads := deviceMux(t)
	rec := serve(mux, http.MethodGet, "/api/devices/pair", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	out := decodeMap(t, rec)
	if out["hasToken"] != false || out["enabled"] != false {
		t.Fatalf("hasToken/enabled = %v/%v, want false/false", out["hasToken"], out["enabled"])
	}
	if _, ok := out["qr_png"]; ok {
		t.Fatal("status must not render a QR")
	}
	if *reloads != 0 {
		t.Fatalf("reloads = %d, want 0", *reloads)
	}
	if d := savedDevice(t, h); d.Token != "" || d.Enabled {
		t.Fatalf("status changed the config: %+v", d)
	}
}

// POST /api/devices/settings saves what was sent (trimmed address, LAN host),
// reloads only on a change, and refuses a body that is not JSON or a network
// listener without a token.
func TestDeviceSettings_SavesAndReloadsOnChange(t *testing.T) {
	h, mux, reloads := deviceMux(t)

	rec := serve(mux, http.MethodPost, "/api/devices/settings", "{")
	if rec.Code != http.StatusBadRequest || rec.Body.String() != `{"error":"invalid body"}`+"\n" {
		t.Fatalf("bad body: %d %q", rec.Code, rec.Body.String())
	}

	rec = serve(mux, http.MethodPost, "/api/devices/settings", `{"listen_lan":true,"enabled":true}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(),
		"channels.device.token or channels.device.word_token must be set when the device listener is on a network address") {
		t.Fatalf("LAN without a token: %d %q", rec.Code, rec.Body.String())
	}
	if d := savedDevice(t, h); d.Host == "0.0.0.0" || *reloads != 0 {
		t.Fatalf("refused save was applied: host %q reloads %d", d.Host, *reloads)
	}
	if rec := serve(mux, http.MethodPost, "/api/devices/pair", ""); rec.Code != http.StatusOK {
		t.Fatalf("pair: %d", rec.Code)
	}

	rec = serve(mux, http.MethodPost, "/api/devices/settings",
		`{"listen_lan":true,"external_url":"  wss://claw.example.com  ","enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	d := savedDevice(t, h)
	if d.Host != "0.0.0.0" || d.ExternalURL != "wss://claw.example.com" || !d.Enabled {
		t.Fatalf("saved = host %q url %q enabled %v", d.Host, d.ExternalURL, d.Enabled)
	}
	out := decodeMap(t, rec)
	if out["listen_lan"] != true || out["external_url"] != "wss://claw.example.com" {
		t.Fatalf("response listen_lan/external_url = %v/%v", out["listen_lan"], out["external_url"])
	}
	if *reloads != 2 {
		t.Fatalf("reloads = %d, want 2", *reloads)
	}

	// The same values again change nothing and do not reload.
	rec = serve(mux, http.MethodPost, "/api/devices/settings",
		`{"listen_lan":true,"external_url":"wss://claw.example.com","enabled":true}`)
	if rec.Code != http.StatusOK || *reloads != 2 {
		t.Fatalf("no-op save: status %d reloads %d, want 200 and 2", rec.Code, *reloads)
	}

	rec = serve(mux, http.MethodPost, "/api/devices/settings", `{"listen_lan":false,"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if d := savedDevice(t, h); d.Host != "127.0.0.1" || d.Enabled {
		t.Fatalf("saved = host %q enabled %v, want 127.0.0.1 false", d.Host, d.Enabled)
	}
	if *reloads != 3 {
		t.Fatalf("reloads = %d, want 3", *reloads)
	}
}

// POST /api/devices/word-token/regenerate replaces the word passphrase, keeps
// the QR token, and reloads.
func TestDeviceWordTokenRegenerate(t *testing.T) {
	h, mux, reloads := deviceMux(t)
	if rec := serve(mux, http.MethodPost, "/api/devices/pair", ""); rec.Code != http.StatusOK {
		t.Fatalf("pair: %d", rec.Code)
	}
	before := savedDevice(t, h)
	rec := serve(mux, http.MethodPost, "/api/devices/word-token/regenerate", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	after := savedDevice(t, h)
	if after.Token != before.Token {
		t.Fatal("regenerate changed the QR token")
	}
	if after.WordToken == "" || after.WordToken == before.WordToken {
		t.Fatalf("word token %q not replaced (was %q)", after.WordToken, before.WordToken)
	}
	if out := decodeMap(t, rec); out["word_token"] != after.WordToken {
		t.Fatalf("word_token = %v, want %q", out["word_token"], after.WordToken)
	}
	if *reloads != 2 {
		t.Fatalf("reloads = %d, want 2", *reloads)
	}
}

// createPending stores a pending pairing and returns its request id.
func createPending(t *testing.T, h *Handler, deviceID string) string {
	t.Helper()
	store, _, err := h.openDeviceStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.CreatePending(context.Background(), device.PendingPairing{DeviceID: deviceID, PublicKey: "pk-" + deviceID, DisplayName: "Rabbit R1", Role: "node"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Approving a pending request pairs the device; rejecting one drops it; an
// unknown request is a 404 either way.
func TestDevicePending_ApproveAndReject(t *testing.T) {
	h, mux, _ := deviceMux(t)
	approveID := createPending(t, h, "dev-a")
	rejectID := createPending(t, h, "dev-b")

	rec := serve(mux, http.MethodPost, "/api/devices/pending/"+approveID+"/approve", "")
	if rec.Code != http.StatusOK || rec.Body.String() != `{"device_id":"dev-a","status":"approved"}`+"\n" {
		t.Fatalf("approve: %d %q", rec.Code, rec.Body.String())
	}
	rec = serve(mux, http.MethodPost, "/api/devices/pending/"+rejectID+"/reject", "")
	if rec.Code != http.StatusOK || rec.Body.String() != `{"status":"rejected"}`+"\n" {
		t.Fatalf("reject: %d %q", rec.Code, rec.Body.String())
	}

	devs := listDevices(t, mux)
	if len(devs) != 1 || devs[0].DeviceID != "dev-a" {
		t.Fatalf("paired = %+v, want only dev-a", devs)
	}
	rec = serve(mux, http.MethodGet, "/api/devices/pending", "")
	if rec.Code != http.StatusOK || rec.Body.String() != `{"pending":[]}`+"\n" {
		t.Fatalf("pending after resolve: %d %q", rec.Code, rec.Body.String())
	}

	for _, action := range []string{"approve", "reject"} {
		rec = serve(mux, http.MethodPost, "/api/devices/pending/nope/"+action, "")
		if rec.Code != http.StatusNotFound || rec.Body.String() != `{"error":"pending request not found"}`+"\n" {
			t.Fatalf("%s unknown: %d %q", action, rec.Code, rec.Body.String())
		}
	}
}

// POST /api/devices/{id}/agent stores the trimmed agent id; an unknown device
// is a 404 and a body that is not JSON a 400.
func TestDeviceAgent_Assign(t *testing.T) {
	h, mux, _ := deviceMux(t)
	pairTestDevice(t, h, "dev1")

	rec := serve(mux, http.MethodPost, "/api/devices/dev1/agent", "{")
	if rec.Code != http.StatusBadRequest || rec.Body.String() != `{"error":"invalid body"}`+"\n" {
		t.Fatalf("bad body: %d %q", rec.Code, rec.Body.String())
	}
	rec = serve(mux, http.MethodPost, "/api/devices/nope/agent", `{"agent_id":"main"}`)
	if rec.Code != http.StatusNotFound || rec.Body.String() != `{"error":"device not found"}`+"\n" {
		t.Fatalf("unknown device: %d %q", rec.Code, rec.Body.String())
	}
	rec = serve(mux, http.MethodPost, "/api/devices/dev1/agent", `{"agent_id":"  main  "}`)
	if rec.Code != http.StatusOK || rec.Body.String() != `{"ok":true}`+"\n" {
		t.Fatalf("assign: %d %q", rec.Code, rec.Body.String())
	}
	devs := listDevices(t, mux)
	if len(devs) != 1 || devs[0].AgentID != "main" || devs[0].AgentName != "Main" || devs[0].AgentMissing {
		t.Fatalf("devices = %+v, want dev1 assigned to Main", devs)
	}
}

// Once the store is open, a failing database answers each handler's own 500
// without detail.
func TestDeviceHandlers_DatabaseFailure(t *testing.T) {
	h, mux, _ := deviceMux(t)
	store, _, err := h.openDeviceStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, body, want string
	}{
		{http.MethodGet, "/api/devices/pending", "", "list failed"},
		{http.MethodGet, "/api/devices", "", "list failed"},
		{http.MethodPost, "/api/devices/pending/x/approve", "", "approve failed"},
		{http.MethodPost, "/api/devices/pending/x/reject", "", "reject failed"},
		{http.MethodPost, "/api/devices/dev1/agent", `{"agent_id":"main"}`, "update failed"},
		{http.MethodPost, "/api/devices/dev1/user", `{"user_name":"Alice"}`, "update failed"},
		{http.MethodPost, "/api/devices/dev1/agents", `{"allowed_agents":[]}`, "update failed"},
		{http.MethodDelete, "/api/devices/dev1", "", "remove failed"},
	} {
		rec := serve(mux, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusInternalServerError || rec.Body.String() != `{"error":"`+tc.want+`"}`+"\n" {
			t.Errorf("%s %s: %d %q, want 500 %q", tc.method, tc.path, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

// When the device store cannot be opened every store-backed endpoint answers
// "store open failed" and alerts, without the error text.
func TestDeviceHandlers_StoreOpenFailure(t *testing.T) {
	h, mux, _ := deviceMux(t)
	cfg, err := h.currentConfig()
	if err != nil {
		t.Fatal(err)
	}
	// A file where the internal directory belongs makes the open fail.
	if err := os.MkdirAll(cfg.DataDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir(), global.InternalDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &alertRecorder{}
	h.SetAlerter(rec)
	reqs := []struct{ method, path, body string }{
		{http.MethodGet, "/api/devices/pending", ""},
		{http.MethodGet, "/api/devices", ""},
		{http.MethodPost, "/api/devices/pending/x/approve", ""},
		{http.MethodPost, "/api/devices/dev1/agent", `{"agent_id":"main"}`},
		{http.MethodPost, "/api/devices/dev1/user", `{"user_name":"Alice"}`},
		{http.MethodPost, "/api/devices/dev1/agents", `{"allowed_agents":[]}`},
		{http.MethodDelete, "/api/devices/dev1", ""},
	}
	for _, tc := range reqs {
		got := serve(mux, tc.method, tc.path, tc.body)
		if got.Code != http.StatusInternalServerError || got.Body.String() != `{"error":"store open failed"}`+"\n" {
			t.Errorf("%s %s: %d %q", tc.method, tc.path, got.Code, got.Body.String())
		}
	}
	if len(rec.alerts) != len(reqs) {
		t.Fatalf("alerts = %d, want %d", len(rec.alerts), len(reqs))
	}
}

// A data dir change on reload reopens the store at the new path instead of
// serving the old database.
func TestOpenDeviceStore_ReopensOnDataDirChange(t *testing.T) {
	h, _, _ := deviceMux(t)
	first, _, err := h.openDeviceStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := h.openDeviceStore(context.Background())
	if err != nil || again != first {
		t.Fatalf("second open = %p %v, want the cached %p", again, err, first)
	}
	h.deviceStorePath = "elsewhere"
	reopened, _, err := h.openDeviceStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reopened == first {
		t.Fatal("a different path must reopen the store")
	}
}
