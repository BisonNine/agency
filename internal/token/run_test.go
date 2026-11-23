package token

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.bison9.com/agency/app"
	"go.bison9.com/agency/servicetoken"
)

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	done := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(r)
		done <- b
	}()
	defer func() { os.Stdout = orig }()
	fn()
	require.NoError(t, w.Close())
	return string(<-done)
}

func runToken(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() {
		cmd := NewTokenCommand()
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		err = cmd.Execute()
	})
	return out, err
}

func TestTokenRootShowsHelp(t *testing.T) {
	cmd := NewTokenCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs(nil)
	require.NoError(t, cmd.Execute())
	assert.Contains(t, buf.String(), "Mint, rotate, revoke, and list long-lived service tokens.")
}

func TestTokenIssueAndRotateOutput(t *testing.T) {
	for _, sub := range []string{"issue", "rotate"} {
		t.Run(sub, func(t *testing.T) {
			home := setupHome(t)
			out, err := runToken(t, sub, "Alice")
			require.NoError(t, err)

			re := regexp.MustCompile(`^Service token for agent "alice" \(store it securely; it is not shown again\):\n\n  (SST[0-9a-f]{64})\n\n` +
				`Use it as an Authorization: Bearer header on /mcp, or as the session_token\n` +
				`parameter on /internal\. A running ` + regexp.QuoteMeta(app.ShortName()) + ` picks it up automatically within a few seconds\.\n$`)
			m := re.FindStringSubmatch(out)
			require.NotNil(t, m, "output: %q", out)

			toks, err := servicetoken.Load(servicetoken.Path(home))
			require.NoError(t, err)
			assert.Equal(t, map[string]string{"alice": servicetoken.Hash(m[1])}, toks, "only the hash is stored")
		})
	}
}

func TestTokenList(t *testing.T) {
	setupHome(t)
	out, err := runToken(t, "list")
	require.NoError(t, err)
	assert.Equal(t, "No service tokens issued.\n", out)

	_, err = runToken(t, "issue", "bob")
	require.NoError(t, err)
	_, err = runToken(t, "issue", "alice")
	require.NoError(t, err)
	out, err = runToken(t, "list")
	require.NoError(t, err)
	assert.Equal(t, "Agents with a service token:\n  alice\n  bob\n", out)
}

func TestTokenRevokeOutput(t *testing.T) {
	setupHome(t)
	_, err := runToken(t, "issue", "alice")
	require.NoError(t, err)

	out, err := runToken(t, "revoke", "alice")
	require.NoError(t, err)
	assert.Equal(t, `Revoked service token for agent "alice". A running `+app.ShortName()+" removes it automatically within a few seconds.\n", out)

	out, err = runToken(t, "revoke", "alice")
	require.NoError(t, err)
	assert.Equal(t, "No service token for agent \"alice\".\n", out)

	_, err = runToken(t, "revoke", "ghost")
	require.Error(t, err)
	assert.Equal(t, `no agent "ghost" in config and no service token for it (agents: alice, bob)`, err.Error())
}

func TestTokenUnknownAgentMessage(t *testing.T) {
	setupHome(t)
	_, err := runToken(t, "issue", "ghost")
	require.Error(t, err)
	assert.Equal(t, `no agent "ghost" in config (agents: alice, bob)`, err.Error())
}

func TestTokenBadConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAW_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.json"), []byte("{bad"), 0o600))
	for _, args := range [][]string{{"issue", "alice"}, {"revoke", "alice"}, {"list"}} {
		_, err := runToken(t, args...)
		require.Error(t, err, args)
		assert.Contains(t, err.Error(), "load config: ", args)
	}
}

func TestTokenCorruptStore(t *testing.T) {
	home := setupHome(t)
	path := servicetoken.Path(home)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("{bad"), 0o600))
	for _, args := range [][]string{{"issue", "alice"}, {"revoke", "alice"}, {"list"}} {
		_, err := runToken(t, args...)
		require.Error(t, err, args)
		assert.Contains(t, err.Error(), "servicetoken: parse ", args)
	}
}

// A store that cannot be written fails issue and revoke and leaves it as it was.
func TestTokenStoreNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	home := setupHome(t)
	path := servicetoken.Path(home)
	require.NoError(t, servicetoken.Save(path, map[string]string{"bob": servicetoken.Hash("t")}))
	dir := filepath.Dir(path)
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	for _, args := range [][]string{{"issue", "alice"}, {"revoke", "bob"}} {
		_, err := runToken(t, args...)
		require.Error(t, err, args)
		assert.Contains(t, err.Error(), "servicetoken: write ", args)
	}
	toks, err := servicetoken.Load(path)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"bob": servicetoken.Hash("t")}, toks)
}
