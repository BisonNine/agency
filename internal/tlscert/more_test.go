package tlscert

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/internal/testalerts"
)

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
}

// readOnly makes dir read-only until the test ends.
func readOnly(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

// failWriter fails every write.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func setupHome(t *testing.T, cfg string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CLAW_HOME", home)
	if cfg != "" {
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.json"), []byte(cfg), 0o600))
	}
	return home
}

func runTLS(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewTLSCommand()
	var out bytes.Buffer
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	return out.String(), err
}

func TestTLSCommandShowsSelfSigned(t *testing.T) {
	home := setupHome(t, "")
	out, err := runTLS(t)
	require.NoError(t, err)

	info, err := InspectFile(Options{DataDir: home})
	require.NoError(t, err)
	assert.Regexp(t, `^HTTPS listener: \S+ \(gateway\.tls\.mode "all"\)\n`, out)
	assert.Contains(t, out, "Source:         self-signed\n")
	assert.Contains(t, out, "Certificate:    "+filepath.Join(home, DirName, SelfSignedCertFile)+"\n")
	assert.Contains(t, out, "Key:            "+filepath.Join(home, DirName, SelfSignedKeyFile)+"\n")
	assert.Contains(t, out, "Subject:        "+info.Subject+"\n")
	assert.Contains(t, out, "Names:          "+strings.Join(info.Names(), ", ")+"\n")
	assert.Regexp(t, `Not after:      `+regexp.QuoteMeta(info.NotAfter.Format(time.RFC3339))+` \(\d+ days\)\n`, out)
	assert.True(t, strings.HasSuffix(out, "SHA-256:        "+info.Fingerprint+"\n"), out)
}

func TestTLSCommandHTTPSOff(t *testing.T) {
	setupHome(t, `{"gateway":{"tls":{"mode":"off"}}}`)
	out, err := runTLS(t)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(out, "HTTPS listener: off (gateway.tls.mode \"off\"; set it to \"all\" or \"localhost\" to enable)\n"), out)
}

func TestTLSCommandRegenerate(t *testing.T) {
	home := setupHome(t, "")
	_, err := runTLS(t)
	require.NoError(t, err)
	before, err := InspectFile(Options{DataDir: home})
	require.NoError(t, err)

	out, err := runTLS(t, "--regenerate")
	require.NoError(t, err)
	after, err := InspectFile(Options{DataDir: home})
	require.NoError(t, err)
	assert.NotEqual(t, before.Fingerprint, after.Fingerprint)
	assert.True(t, strings.HasPrefix(out, "Self-signed certificate regenerated.\n"), out)
	assert.Contains(t, out, "SHA-256:        "+after.Fingerprint+"\n")
}

func TestTLSCommandOperatorFile(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writePair(t, dir, certNames{dns: []string{"alice.example"}}, time.Now())
	setupHome(t, `{"gateway":{"tls":{"cert_file":"`+certPath+`","key_file":"`+keyPath+`"}}}`)

	out, err := runTLS(t)
	require.NoError(t, err)
	assert.Contains(t, out, "Source:         file\n")
	assert.Contains(t, out, "Names:          alice.example\n")

	_, err = runTLS(t, "--regenerate")
	require.Error(t, err)
	assert.Equal(t, "gateway.tls.cert_file and key_file are set ("+certPath+"); --regenerate applies only to the self-signed certificate", err.Error())
}

func TestTLSCommandErrors(t *testing.T) {
	t.Run("bad config", func(t *testing.T) {
		home := setupHome(t, "")
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.json"), []byte("{bad"), 0o600))
		_, err := runTLS(t)
		require.Error(t, err)
	})

	t.Run("operator pair does not load", func(t *testing.T) {
		dir := t.TempDir()
		setupHome(t, `{"gateway":{"tls":{"cert_file":"`+filepath.Join(dir, "x.crt")+`","key_file":"`+filepath.Join(dir, "x.key")+`"}}}`)
		_, err := runTLS(t)
		require.Error(t, err)
		assert.True(t, strings.HasPrefix(err.Error(), "gateway.tls: load "), err.Error())
	})

	t.Run("regenerate fails", func(t *testing.T) {
		skipIfRoot(t)
		home := setupHome(t, "")
		_, err := runTLS(t)
		require.NoError(t, err)
		readOnly(t, filepath.Join(home, DirName))
		_, err = runTLS(t, "--regenerate")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "generate self-signed certificate: ")
	})

	t.Run("output fails", func(t *testing.T) {
		for _, args := range [][]string{nil, {"--regenerate"}} {
			setupHome(t, "")
			cmd := NewTLSCommand()
			cmd.SetArgs(args)
			cmd.SetOut(failWriter{})
			cmd.SetErr(&bytes.Buffer{})
			require.Error(t, cmd.Execute(), args)
		}
	})
}

func TestOptionsFromConfig(t *testing.T) {
	t.Setenv("CLAW_HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.Gateway.TLS.CertFile = " /a.crt "
	cfg.Gateway.TLS.KeyFile = " /a.key "
	cfg.Gateway.TLS.ExtraNames = []string{"alias.example"}
	cfg.Gateway.ExternalURL = "https://alice.example.com:8443/claw"
	o := OptionsFromConfig(cfg)
	assert.Equal(t, cfg.DataDir(), o.DataDir)
	assert.Equal(t, "/a.crt", o.CertFile)
	assert.Equal(t, "/a.key", o.KeyFile)
	assert.Equal(t, []string{"alias.example"}, o.ExtraNames)
	assert.Equal(t, "alice.example.com", o.ExternalHost)
	assert.Equal(t, cfg.PermissionModes(config.AreaSecrets), o.SecretModes)

	cfg.Gateway.ExternalURL = "http://[::1"
	assert.Empty(t, OptionsFromConfig(cfg).ExternalHost)
}

func TestNamesForConfig(t *testing.T) {
	assert.Nil(t, NamesForConfig(nil))

	t.Setenv("CLAW_HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.Gateway.TLS.Mode = config.TLSModeOff
	assert.Nil(t, NamesForConfig(cfg), "HTTPS off")

	cfg.Gateway.TLS.Mode = config.TLSModeAll
	assert.Nil(t, NamesForConfig(cfg), "no certificate yet")

	dir := t.TempDir()
	cfg.Gateway.TLS.CertFile, cfg.Gateway.TLS.KeyFile = writePair(t, dir, certNames{dns: []string{"alice.example"}, ips: []net.IP{net.ParseIP("192.0.2.1")}}, time.Now())
	assert.Equal(t, []string{"alice.example", "192.0.2.1"}, NamesForConfig(cfg))
}

func TestInspectFileSkipsOtherBlocks(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writePair(t, dir, certNames{dns: []string{"alice.example"}}, time.Now())
	certPEM, err := os.ReadFile(certPath)
	require.NoError(t, err)
	keyPEM, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	combined := filepath.Join(dir, "combined.pem")
	require.NoError(t, os.WriteFile(combined, append(keyPEM, certPEM...), 0o600))

	info, err := InspectFile(Options{CertFile: combined, KeyFile: keyPath})
	require.NoError(t, err)
	assert.Equal(t, []string{"alice.example"}, info.DNSNames)

	_, err = InspectFile(Options{CertFile: filepath.Join(dir, "missing.pem"), KeyFile: keyPath})
	require.ErrorIs(t, err, os.ErrNotExist)

	bad := filepath.Join(dir, "bad.pem")
	require.NoError(t, os.WriteFile(bad, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("junk")}), 0o600))
	_, err = InspectFile(Options{CertFile: bad, KeyFile: keyPath})
	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), bad+": "), err.Error())
}

func TestValidatePairMoreErrors(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	certPath, keyPath := writePair(t, dir, certNames{dns: []string{"ok.example"}}, now)
	futureCert, futureKey := writePair(t, t.TempDir(), certNames{dns: []string{"later.example"}}, now.Add(48*time.Hour))
	garbage := filepath.Join(dir, "garbage.crt")
	require.NoError(t, os.WriteFile(garbage, []byte("not a cert"), 0o600))

	for _, c := range []struct {
		name, cert, key, want string
	}{
		{"relative key", certPath, "user.key", `key_file "user.key" must be an absolute path`},
		{"cert is not PEM", garbage, keyPath, "cert_file " + garbage + " is not PEM"},
		{"not yet valid", futureCert, futureKey, "cert_file " + futureCert + " is not valid until "},
	} {
		_, err := ValidatePair(c.cert, c.key, now)
		if assert.Error(t, err, c.name) {
			assert.Contains(t, err.Error(), c.want, c.name)
		}
	}
}

func TestLookupFQDN(t *testing.T) {
	saved := lookupCNAME
	t.Cleanup(func() { lookupCNAME = saved })
	for _, c := range []struct {
		name, cname string
		err         error
		want        string
	}{
		{"canonical name", "Alice.Example.COM.", nil, "alice.example.com"},
		{"lookup fails", "", errors.New("no resolver"), ""},
		{"empty", " ", nil, ""},
		{"the bare host name", "alice.", nil, ""},
		{"no domain", "bob", nil, ""},
		{"localhost", "localhost.", nil, ""},
	} {
		lookupCNAME = func(context.Context, string) (string, error) { return c.cname, c.err }
		assert.Equal(t, c.want, lookupFQDN("alice"), c.name)
	}
}

// The FQDN, when the resolver has one, is one of the certificate's names.
func TestExpectedNamesIncludesFQDN(t *testing.T) {
	saved := lookupCNAME
	t.Cleanup(func() { lookupCNAME = saved })
	lookupCNAME = func(context.Context, string) (string, error) { return "alice.corp.example.", nil }
	n := expectedNames(Options{})
	assert.Contains(t, n.dns, "alice.corp.example")
}

func TestCoveredByChecksIPs(t *testing.T) {
	certPEM, _, err := generateSelfSigned(certNames{dns: []string{"alice.example"}, ips: []net.IP{net.ParseIP("192.0.2.1")}}, time.Now())
	require.NoError(t, err)
	block, _ := pem.Decode(certPEM)
	leaf, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)

	assert.True(t, (&certNames{dns: []string{"alice.example"}, ips: []net.IP{net.ParseIP("192.0.2.1")}}).coveredBy(leaf))
	assert.False(t, (&certNames{dns: []string{"alice.example"}, ips: []net.IP{net.ParseIP("192.0.2.2")}}).coveredBy(leaf))
	assert.False(t, (&certNames{dns: []string{"bob.example"}}).coveredBy(leaf))
}

func TestGenerateSelfSignedCommonName(t *testing.T) {
	for _, c := range []struct {
		name  string
		names certNames
		want  string
	}{
		{"first DNS name", certNames{dns: []string{"alice.example", "bob.example"}, ips: []net.IP{net.ParseIP("192.0.2.1")}}, "alice.example"},
		{"first IP without DNS names", certNames{ips: []net.IP{net.ParseIP("192.0.2.1")}}, "192.0.2.1"},
		{"fallback", certNames{}, global.SelfSignedFallbackCN},
	} {
		certPEM, _, err := generateSelfSigned(c.names, time.Now())
		require.NoError(t, err)
		block, _ := pem.Decode(certPEM)
		leaf, err := x509.ParseCertificate(block.Bytes)
		require.NoError(t, err)
		assert.Equal(t, c.want, leaf.Subject.CommonName, c.name)
	}
}

// An unloadable self-signed pair is replaced on load.
func TestLoadReplacesUnloadableSelfSigned(t *testing.T) {
	dir := t.TempDir()
	tlsDir := filepath.Join(dir, DirName)
	require.NoError(t, os.MkdirAll(tlsDir, 0o700))
	for _, f := range []string{SelfSignedCertFile, SelfSignedKeyFile} {
		require.NoError(t, os.WriteFile(filepath.Join(tlsDir, f), []byte("garbage"), 0o600))
	}
	m, err := Load(Options{DataDir: dir})
	require.NoError(t, err)
	assert.True(t, m.Info().SelfSigned)
	_, err = InspectFile(Options{DataDir: dir})
	require.NoError(t, err)
}

func TestLoadSelfSignedWriteFailures(t *testing.T) {
	t.Run("data directory is a file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "file")
		require.NoError(t, os.WriteFile(file, nil, 0o600))
		_, err := Load(Options{DataDir: file})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "generate self-signed certificate: ")
	})

	t.Run("directory not writable", func(t *testing.T) {
		skipIfRoot(t)
		dir := t.TempDir()
		tlsDir := filepath.Join(dir, DirName)
		require.NoError(t, os.MkdirAll(tlsDir, 0o700))
		readOnly(t, tlsDir)
		_, err := Load(Options{DataDir: dir})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "generate self-signed certificate: ")
	})
}

func TestWriteAtomicRenameFailureCleansUp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "cert.pem")
	// A non-empty directory cannot be replaced by a file.
	require.NoError(t, os.MkdirAll(filepath.Join(target, "x"), 0o700))
	require.Error(t, writeAtomic(target, []byte("data"), 0o600))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "temp file removed")
}

func TestGetCertificateBeforeLoad(t *testing.T) {
	_, err := (&Manager{}).getCertificate(&tls.ClientHelloInfo{})
	require.ErrorIs(t, err, errNoCertificate)
}

// A missing certificate or key file is a failed reload, alerted, with the
// previous certificate still served.
func TestReloadMissingFiles(t *testing.T) {
	for _, missing := range []string{"cert", "key"} {
		t.Run(missing, func(t *testing.T) {
			dir := t.TempDir()
			rec := &testalerts.Recorder{}
			certPath, keyPath := writePair(t, dir, certNames{dns: []string{"alice.example"}}, time.Now())
			m, err := Load(Options{CertFile: certPath, KeyFile: keyPath, Alerter: rec})
			require.NoError(t, err)
			before := m.Info().Fingerprint
			path := map[string]string{"cert": certPath, "key": keyPath}[missing]
			require.NoError(t, os.Remove(path))

			changed, err := m.Reload()
			require.ErrorIs(t, err, os.ErrNotExist)
			assert.False(t, changed)
			assert.Equal(t, before, m.Info().Fingerprint)
			assert.Len(t, rec.Alerts(), 1)
		})
	}
}

func TestCheckExpiryMore(t *testing.T) {
	t.Run("fresh self-signed is left alone", func(t *testing.T) {
		rec := &testalerts.Recorder{}
		m, err := Load(Options{DataDir: t.TempDir(), Alerter: rec})
		require.NoError(t, err)
		before := m.Info().Fingerprint
		m.CheckExpiry()
		assert.Equal(t, before, m.Info().Fingerprint)
		assert.Empty(t, rec.Alerts())
	})

	t.Run("self-signed renewal fails", func(t *testing.T) {
		skipIfRoot(t)
		dir := t.TempDir()
		rec := &testalerts.Recorder{}
		gen := time.Now().Add(-(selfSignedValidity - 10*day))
		m, err := Load(Options{DataDir: dir, Alerter: rec, Now: func() time.Time { return gen }})
		require.NoError(t, err)
		before := m.Info()
		m.opts.Now = nil
		readOnly(t, filepath.Join(dir, DirName))

		m.CheckExpiry()
		assert.Equal(t, before.Fingerprint, m.Info().Fingerprint)
		got := rec.Alerts()
		require.Len(t, got, 1)
		assert.Equal(t, "Self-signed TLS certificate renewal failed", got[0].Title)
		assert.Equal(t, "tls-selfsigned", got[0].EventID)
		assert.Equal(t, "the certificate expires "+before.NotAfter.Format(time.RFC3339)+
			" and could not be regenerated; HTTPS clients will start refusing it", got[0].Description)
	})

	t.Run("expired user certificate uses the process alerter", func(t *testing.T) {
		rec := testalerts.Install(t)
		dir := t.TempDir()
		certPath, keyPath := writePair(t, dir, certNames{dns: []string{"alice.example"}}, time.Now())
		m, err := Load(Options{CertFile: certPath, KeyFile: keyPath})
		require.NoError(t, err)
		notAfter := m.Info().NotAfter

		// The 14- and 3-day warnings, then the expiry, one check each.
		for _, at := range []time.Time{notAfter.Add(-13 * day), notAfter.Add(-2 * day), notAfter.Add(time.Hour)} {
			m.opts.Now = func() time.Time { return at }
			m.CheckExpiry()
		}
		got := rec.Alerts()
		require.Len(t, got, 3)
		assert.Equal(t, certPath+" expired "+notAfter.Format(time.RFC3339)+"; browsers now refuse the WebUI until a renewed pair is installed", got[2].Description)
	})
}

// Watch notices a replaced pair on its next poll and stops with its context.
func TestWatchReloadsChangedPair(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writePair(t, dir, certNames{dns: []string{"one.example"}}, time.Now())
	m, err := Load(Options{CertFile: certPath, KeyFile: keyPath, Alerter: &testalerts.Recorder{}})
	require.NoError(t, err)
	changed := make(chan Info, 1)
	m.OnChange(func(i Info) {
		select {
		case changed <- i:
		default:
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Watch(ctx, time.Millisecond)

	writePair(t, dir, certNames{dns: []string{"two.example"}}, time.Now())
	touch(t, certPath)
	select {
	case info := <-changed:
		assert.Equal(t, []string{"two.example"}, info.DNSNames)
	case <-time.After(30 * time.Second):
		t.Fatal("Watch did not pick up the new pair")
	}
}

// A zero interval uses the default; the watcher still stops with ctx.
func TestWatchDefaultInterval(t *testing.T) {
	m, err := Load(Options{DataDir: t.TempDir(), Alerter: &testalerts.Recorder{}})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	m.Watch(ctx, 0)
	cancel()
}
