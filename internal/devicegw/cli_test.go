// ClawEh
// License: MIT

package devicegw

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agency/channels/device"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/utils"
)

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		if _, cerr := io.Copy(&buf, r); cerr != nil {
			t.Error(cerr)
		}
		done <- buf.String()
	}()
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return <-done
}

// newHome points CLAW_HOME at a fresh directory holding config.json with body
// ("" writes no config) and returns the directory.
func newHome(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv(global.EnvVarHome, home)
	if body != "" {
		if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// stubLANIPs replaces the LAN address lookup for the duration of the test.
func stubLANIPs(t *testing.T, ips ...string) {
	t.Helper()
	old := lanIPv4s
	lanIPv4s = func() []string { return ips }
	t.Cleanup(func() { lanIPv4s = old })
}

// addPending opens the pairing store under home and records one pending
// pairing for deviceID, returning its request id.
func addPending(t *testing.T, deviceID, displayName, clientID string) string {
	t.Helper()
	store, _, err := openStore()
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer utils.CloseQuietly(store)
	id, err := store.CreatePending(context.Background(), device.PendingPairing{
		DeviceID: deviceID, DisplayName: displayName, ClientID: clientID, Role: "operator",
	})
	if err != nil {
		t.Fatalf("CreatePending: %v", err)
	}
	return id
}

func TestNewDevicesCommand_Subcommands(t *testing.T) {
	cmd := NewDevicesCommand()
	want := map[string]bool{"pair": true, "list": true, "approve": true, "reject": true, "remove": true}
	for _, c := range cmd.Commands() {
		delete(want, c.Name())
	}
	if len(want) != 0 {
		t.Errorf("missing subcommands: %v", want)
	}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("devices with no subcommand: %v", err)
	}
	if !strings.Contains(out.String(), "pair") {
		t.Errorf("help output lacks the subcommands: %q", out.String())
	}
}

// TestPair_ProvisionsAndPrintsQR: pair generates the shared token, enables the
// listener, saves the config and prints the QR, the connect URLs and the
// loopback and approve hints.
func TestPair_ProvisionsAndPrintsQR(t *testing.T) {
	home := newHome(t, `{}`)
	stubLANIPs(t, "192.0.2.10")
	var err error
	out := captureStdout(t, func() { err = pair() })
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	for _, want := range []string{
		"Scan this QR code with your device:",
		"Setup payload:",
		"ws://192.0.2.10:18791",
		"generated a shared token and enabled the device listener",
		"WARNING: the device listener is on loopback (127.0.0.1)",
		"devices approve <request-id>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "no routable LAN IPv4") {
		t.Errorf("LAN warning printed although a LAN address exists:\n%s", out)
	}
	raw, rerr := os.ReadFile(filepath.Join(home, "config.json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	var saved struct {
		Channels struct {
			Device struct {
				Enabled bool   `json:"enabled"`
				Token   string `json:"token"`
			} `json:"device"`
		} `json:"channels"`
	}
	if jerr := json.Unmarshal(raw, &saved); jerr != nil {
		t.Fatal(jerr)
	}
	if !saved.Channels.Device.Enabled || len(saved.Channels.Device.Token) != 64 {
		t.Errorf("saved device config = %+v, want enabled with a 64-hex token", saved.Channels.Device)
	}
	if !strings.Contains(out, saved.Channels.Device.Token) {
		t.Error("setup payload does not carry the saved token")
	}
}

// TestPair_AlreadyProvisioned: a provisioned, LAN-listening device channel
// prints neither the provisioning note nor the loopback warning; with no LAN
// address and no external URL it warns that devices cannot reach it.
func TestPair_AlreadyProvisioned(t *testing.T) {
	newHome(t, `{"channels":{"device":{"enabled":true,"token":"tok","word_token":"a b c d e","host":"0.0.0.0","port":19000}}}`)
	stubLANIPs(t)
	var err error
	out := captureStdout(t, func() { err = pair() })
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	if strings.Contains(out, "generated a shared token") {
		t.Errorf("provisioning note printed for an already provisioned listener:\n%s", out)
	}
	if strings.Contains(out, "on loopback") {
		t.Errorf("loopback warning printed for host 0.0.0.0:\n%s", out)
	}
	if !strings.Contains(out, "WARNING: no routable LAN IPv4 address detected; set channels.device.external_url.") {
		t.Errorf("no-LAN warning missing:\n%s", out)
	}
}

// TestPair_ExternalURL: the external URL is what the QR advertises, so no LAN
// warning is printed even without a LAN address.
func TestPair_ExternalURL(t *testing.T) {
	newHome(t, `{"channels":{"device":{"enabled":true,"token":"tok","word_token":"a b c d e","host":"0.0.0.0","external_url":"https://claw.example.com"}}}`)
	stubLANIPs(t)
	var err error
	out := captureStdout(t, func() { err = pair() })
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	if !strings.Contains(out, "wss://claw.example.com:443") {
		t.Errorf("connect URL missing:\n%s", out)
	}
	if strings.Contains(out, "no routable LAN") {
		t.Errorf("LAN warning printed with an external URL:\n%s", out)
	}
}

func TestPair_Errors(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"bad config", `{not json`, "provision device listener"},
		{"bad external url", `{"channels":{"device":{"enabled":true,"token":"t","word_token":"w","external_url":"http://"}}}`, "build setup payload"},
		{"token too long for a QR", `{"channels":{"device":{"enabled":true,"word_token":"w","token":"` + strings.Repeat("x", 4000) + `"}}}`, "render qr"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			newHome(t, tc.body)
			stubLANIPs(t, "192.0.2.10")
			var err error
			captureStdout(t, func() { err = pair() })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("pair error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestListApproveRemove walks a pairing through the CLI: listed as pending,
// approved, listed as paired, then removed.
func TestListApproveRemove(t *testing.T) {
	newHome(t, `{}`)
	named := addPending(t, "dev-alice", "Alice's R1", "rabbit")
	unnamed := addPending(t, "dev-bob", "", "bob-client")

	var err error
	out := captureStdout(t, func() { err = list() })
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"Pending (2):", named + "  Alice's R1  device=dev-alice  role=operator", unnamed + "  bob-client  device=dev-bob", "Paired (0):"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}

	out = captureStdout(t, func() { err = approve(named) })
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !strings.Contains(out, "Approved device dev-alice") {
		t.Errorf("approve output = %q", out)
	}
	captureStdout(t, func() { err = approve(unnamed) })
	if err != nil {
		t.Fatalf("approve: %v", err)
	}

	out = captureStdout(t, func() { err = list() })
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"Pending (0):", "Paired (2):", "dev-alice  Alice's R1  roles=", "dev-bob  bob-client  roles="} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}

	out = captureStdout(t, func() { err = remove("dev-alice") })
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !strings.Contains(out, "Removed device dev-alice (tokens revoked)") {
		t.Errorf("remove output = %q", out)
	}
	out = captureStdout(t, func() { err = list() })
	if err != nil || !strings.Contains(out, "Paired (1):") || strings.Contains(out, "dev-alice") {
		t.Errorf("after remove list = %q (%v), want only dev-bob", out, err)
	}
}

func TestReject(t *testing.T) {
	newHome(t, `{}`)
	id := addPending(t, "dev-alice", "Alice's R1", "rabbit")
	var err error
	out := captureStdout(t, func() { err = reject(id) })
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if !strings.Contains(out, "Rejected pairing request "+id) {
		t.Errorf("reject output = %q", out)
	}
	out = captureStdout(t, func() { err = list() })
	if err != nil || !strings.Contains(out, "Pending (0):") {
		t.Errorf("after reject list = %q (%v), want no pending", out, err)
	}
}

// TestUnknownIDs: approving, rejecting or removing an id the store does not
// hold is an error, not a silent success.
func TestUnknownIDs(t *testing.T) {
	newHome(t, `{}`)
	for name, fn := range map[string]func() error{
		"approve": func() error { return approve("nope") },
		"reject":  func() error { return reject("nope") },
	} {
		var err error
		out := captureStdout(t, func() { err = fn() })
		if err == nil {
			t.Errorf("%s of an unknown id succeeded: %q", name, out)
		}
	}
}

// TestCommandsWithoutStore: every store command reports a config it cannot
// load instead of opening the store.
func TestCommandsWithoutStore(t *testing.T) {
	for name, fn := range map[string]func() error{
		"list":    list,
		"approve": func() error { return approve("x") },
		"reject":  func() error { return reject("x") },
		"remove":  func() error { return remove("x") },
	} {
		t.Run(name, func(t *testing.T) {
			newHome(t, `{not json`)
			var err error
			captureStdout(t, func() { err = fn() })
			if err == nil {
				t.Fatalf("%s with an unreadable config succeeded", name)
			}
		})
	}
}

func TestOpenStore_Errors(t *testing.T) {
	t.Run("state dir is a file", func(t *testing.T) {
		home := newHome(t, `{}`)
		if err := os.WriteFile(filepath.Join(home, global.InternalDir), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := openStore(); err == nil {
			t.Fatal("openStore succeeded with the state directory blocked by a file")
		}
	})
	t.Run("database is a directory", func(t *testing.T) {
		home := newHome(t, `{}`)
		if err := os.MkdirAll(filepath.Join(home, global.InternalDir, "gateway.db"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, _, err := openStore(); err == nil {
			t.Fatal("openStore succeeded with gateway.db a directory")
		}
	})
}

// TestSubcommandsRun drives each subcommand through cobra so the RunE wiring
// reaches the right function.
func TestSubcommandsRun(t *testing.T) {
	newHome(t, `{}`)
	stubLANIPs(t, "192.0.2.10")
	id := addPending(t, "dev-alice", "Alice's R1", "rabbit")
	id2 := addPending(t, "dev-bob", "Bob's R1", "rabbit")
	steps := []struct {
		args []string
		want string
	}{
		{[]string{"list"}, "Pending (2):"},
		{[]string{"pair"}, "Scan this QR code"},
		{[]string{"approve", id}, "Approved device dev-alice"},
		{[]string{"reject", id2}, "Rejected pairing request " + id2},
		{[]string{"remove", "dev-alice"}, "Removed device dev-alice"},
	}
	for _, s := range steps {
		cmd := NewDevicesCommand()
		cmd.SetArgs(s.args)
		var err error
		out := captureStdout(t, func() { err = cmd.Execute() })
		if err != nil {
			t.Fatalf("devices %v: %v", s.args, err)
		}
		if !strings.Contains(out, s.want) {
			t.Errorf("devices %v output lacks %q:\n%s", s.args, s.want, out)
		}
	}
}

// TestPair_UnsetPortAndHost: a port and host cleared in the config fall back
// to the default device port and the loopback bind address.
func TestPair_UnsetPortAndHost(t *testing.T) {
	newHome(t, `{"channels":{"device":{"enabled":true,"token":"tok","word_token":"a b c d e","host":"","port":0}}}`)
	stubLANIPs(t, "192.0.2.10")
	var err error
	out := captureStdout(t, func() { err = pair() })
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	for _, want := range []string{"ws://192.0.2.10:18791", "on loopback (127.0.0.1)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}
