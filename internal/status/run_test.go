package status

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.bison9.com/agency/app"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/internal/admin"
	"go.bison9.com/agency/internal/pidfile"
	"go.bison9.com/agency/internal/testbinary"
	"go.bison9.com/agency/internal/tlscert"
)

func TestStatusCommandBadConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAW_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.json"), []byte("{bad"), 0o600))
	out := captureStdout(t, func() { NewStatusCommand().Run(nil, nil) })
	assert.True(t, strings.HasPrefix(out, "Error loading config: "), out)
}

func TestStatusCommandReport(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAW_HOME", home)
	cfg := `{"providers":[` +
		`{"name":"Alice AI","protocol":"openai","api_key":"k"},` +
		`{"name":"Bob Local","protocol":"ollama","base_url":"http://127.0.0.1:11434"},` +
		`{"name":"Empty","protocol":"anthropic"}]}`
	configPath := filepath.Join(home, "config.json")
	require.NoError(t, os.WriteFile(configPath, []byte(cfg), 0o600))

	out := captureStdout(t, func() { NewStatusCommand().Run(nil, nil) })

	assert.True(t, strings.HasPrefix(out, "Status\nVersion: "+app.Version()+"\n"), out)
	assert.Contains(t, out, app.Name()+":")
	assert.Contains(t, out, "not running\n")
	assert.Contains(t, out, "Access:\n")
	assert.Contains(t, out, "Config: "+configPath+" ✓\n")
	assert.Contains(t, out, "\nProviders (3):\n")
	assert.Contains(t, out, "  Alice AI:        ✓ (openai)\n")
	assert.Contains(t, out, "  Bob Local:       ✓ (ollama · http://127.0.0.1:11434)\n")
	assert.Contains(t, out, "  Empty:           not set (anthropic)\n")
}

// When no config file exists and none can be written, the defaults are
// reported and the config path is marked missing.
func TestStatusCommandNoConfigFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	home := t.TempDir()
	t.Setenv("CLAW_HOME", home)
	require.NoError(t, os.Chmod(home, 0o500))
	t.Cleanup(func() { _ = os.Chmod(home, 0o700) })

	out := captureStdout(t, func() { NewStatusCommand().Run(nil, nil) })
	assert.Contains(t, out, "Config: "+filepath.Join(home, "config.json")+" ✗\n")
	assert.NotContains(t, out, "Providers")
}

func TestPrintProcessRunning(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, pidfile.Write(dir))
	out := captureStdout(t, func() { printProcess(dir) })
	prefix := app.Name() + ":"
	want := prefix + strings.Repeat(" ", max(16-len(prefix), 0)) + " running (pid " + strconv.Itoa(os.Getpid()) + "), "
	assert.True(t, strings.HasPrefix(out, want), out)
	assert.True(t, strings.HasSuffix(out, "B RAM\n"), out)
}

func TestPrintAccess_ExternalURLAndDeviceDefaults(t *testing.T) {
	cfg := accessConfig(t, "127.0.0.1", config.TLSModeOff)
	cfg.Gateway.ExternalURL = "https://alice.example.com"
	cfg.Channels.Device.Enabled = true
	cfg.Channels.Device.Host = ""
	cfg.Channels.Device.Port = 0
	got := accessReport(cfg)
	wantLines(t, got,
		"  External URL:       https://alice.example.com\n",
		"  Device listener:    ws://127.0.0.1:",
	)
}

func TestPrintAccess_Certificate(t *testing.T) {
	t.Run("self-signed", func(t *testing.T) {
		cfg := accessConfig(t, "127.0.0.1", "")
		m, err := tlscert.Load(tlscert.OptionsFromConfig(cfg))
		require.NoError(t, err)
		info := m.Info()
		wantLines(t, accessReport(cfg),
			"  Certificate:        self-signed, expires "+info.NotAfter.Format("2006-01-02")+"\n"+
				"                      (self-signed: the browser warns once; compare this fingerprint)\n"+
				"  Fingerprint:        "+info.Fingerprint+"\n")
	})

	t.Run("operator file", func(t *testing.T) {
		cfg := accessConfig(t, "127.0.0.1", "")
		m, err := tlscert.Load(tlscert.OptionsFromConfig(cfg))
		require.NoError(t, err)
		info := m.Info()
		cfg.Gateway.TLS.CertFile, cfg.Gateway.TLS.KeyFile = info.CertFile, info.KeyFile
		got := accessReport(cfg)
		wantLines(t, got,
			"  Certificate:        file, expires "+info.NotAfter.Format("2006-01-02")+"\n"+
				"  Fingerprint:        "+info.Fingerprint+"\n")
		assert.NotContains(t, got, "(self-signed:")
	})

	t.Run("unreadable", func(t *testing.T) {
		cfg := accessConfig(t, "127.0.0.1", "")
		bad := filepath.Join(t.TempDir(), "bad.pem")
		require.NoError(t, os.WriteFile(bad, []byte("not a certificate"), 0o600))
		cfg.Gateway.TLS.CertFile, cfg.Gateway.TLS.KeyFile = bad, bad
		wantLines(t, accessReport(cfg), "  Certificate:        unreadable: ")
	})
}

func TestPrintAccess_AdminProblems(t *testing.T) {
	t.Run("file too open", func(t *testing.T) {
		testbinary.Set(t, "alice-claw")
		cfg := accessConfig(t, "127.0.0.1", config.TLSModeOff)
		path := admin.Path(cfg.DataDir())
		require.NoError(t, admin.Write(path, "alice", "correct horse battery", admin.DefaultFileMode))
		require.NoError(t, os.Chmod(path, 0o644))
		wantLines(t, accessReport(cfg),
			"  Admin account:      ignored, file more open than allowed — run: chmod 600 "+path+"\n")
	})

	t.Run("unreadable", func(t *testing.T) {
		cfg := accessConfig(t, "127.0.0.1", config.TLSModeOff)
		path := admin.Path(cfg.DataDir())
		require.NoError(t, os.MkdirAll(path, 0o700))
		wantLines(t, accessReport(cfg), "  Admin account:      unreadable: credentials path "+path+" is a directory\n")
	})
}
