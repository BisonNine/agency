// Agency
// License: MIT

package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agency/global"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/utils"
)

// acpStdio swaps os.Stdin, os.Stdout and os.Stderr for pipes while acpCmd
// runs: the test writes JSON-RPC requests to in and reads responses from out.
type acpStdio struct {
	in  *os.File
	out *bufio.Reader
}

func withACPStdio(t *testing.T) *acpStdio {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = inR, outW, devNull
	// acpCmd silences the console logger; restore it afterwards.
	restoreLog := logger.RedirectForTest(io.Discard)
	level := logger.GetLevel()
	t.Cleanup(func() {
		os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr
		restoreLog()
		logger.SetLevel(level)
		logger.DisableFileLogging()
		// Some ends are closed by the test already.
		for _, f := range []*os.File{inR, inW, outR, outW, devNull} {
			utils.CloseQuietly(f)
		}
	})
	return &acpStdio{in: inW, out: bufio.NewReader(outR)}
}

// acpHome writes a AGENCY_HOME config whose device channel is the test gateway's.
func acpHome(t *testing.T, g *acpTestGateway, extra string) {
	t.Helper()
	t.Setenv(global.EnvVarHome, g.dataDir)
	body := fmt.Sprintf(`{"channels":{"device":{"enabled":true,"token":%q,"host":"127.0.0.1","port":%d,"tls":%v}}%s}`,
		g.dev.Token, g.dev.Port, g.dev.TLS, extra)
	if err := os.WriteFile(filepath.Join(g.dataDir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestACPCmd_ServesStdio: `claw acp` pairs itself with the running gateway,
// answers an ACP initialize on stdout and exits cleanly when stdin closes.
func TestACPCmd_ServesStdio(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls=%v", useTLS), func(t *testing.T) {
			g := startACPTestGateway(t, t.TempDir(), useTLS)
			acpHome(t, g, `,"logging":{"level":"warn"}`)
			io := withACPStdio(t)
			done := make(chan error, 1)
			cmd := NewACPCommand()
			cmd.SetArgs(nil)
			go func() { done <- cmd.Execute() }()

			req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}` + "\n"
			if _, err := io.in.WriteString(req); err != nil {
				t.Fatal(err)
			}
			line, err := readLine(io.out, done)
			if err != nil {
				t.Fatal(err)
			}
			var resp struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
			}
			if err := json.Unmarshal([]byte(line), &resp); err != nil || resp.ID != 1 || !strings.Contains(string(resp.Result), "protocolVersion") {
				t.Fatalf("initialize response %q (%v)", line, err)
			}
			if cerr := io.in.Close(); cerr != nil {
				t.Error(cerr)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("acp after stdin EOF = %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("acp did not exit after stdin closed")
			}
		})
	}
}

// readLine reads one stdout line, failing if acpCmd returns first.
func readLine(r *bufio.Reader, done <-chan error) (string, error) {
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		l, err := r.ReadString('\n')
		ch <- res{l, err}
	}()
	select {
	case got := <-ch:
		return got.line, got.err
	case err := <-done:
		return "", fmt.Errorf("acp exited before answering: %w", err)
	case <-time.After(30 * time.Second):
		return "", errors.New("no answer on stdout")
	}
}

// TestACPCmd_GatewayGone: when the gateway connection drops, acp exits.
func TestACPCmd_GatewayGone(t *testing.T) {
	g := startACPTestGateway(t, t.TempDir(), false)
	acpHome(t, g, "")
	io := withACPStdio(t)
	done := make(chan error, 1)
	go func() { done <- acpCmd(true, "", true) }()
	// Initialize proves the bridge is up before the gateway goes.
	if _, err := io.in.WriteString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := readLine(io.out, done); err != nil {
		t.Fatal(err)
	}
	if err := g.channel.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("acp after the gateway closed = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("acp did not exit after the gateway closed")
	}
}

func TestACPCmd_Errors(t *testing.T) {
	t.Run("config unreadable", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv(global.EnvVarHome, home)
		if err := os.WriteFile(filepath.Join(home, "config.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		withACPStdio(t)
		if err := acpCmd(false, "", true); err == nil || !strings.Contains(err.Error(), "error loading config") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("nothing listening", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv(global.EnvVarHome, home)
		port := freeLoopbackPort(t)
		body := fmt.Sprintf(`{"channels":{"device":{"enabled":true,"word_token":"a b c d e","port":%d,"tls":true}}}`, port)
		if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		withACPStdio(t)
		if err := acpCmd(false, "", false); err == nil || !strings.Contains(err.Error(), "acp: connect to wss://127.0.0.1:") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("not paired without auto-pair", func(t *testing.T) {
		g := startACPTestGateway(t, t.TempDir(), false)
		acpHome(t, g, "")
		withACPStdio(t)
		err := acpCmd(false, "", false)
		if err == nil || !strings.Contains(err.Error(), "is not paired yet") {
			t.Fatalf("err = %v", err)
		}
	})
}
