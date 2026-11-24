package test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/internal/test/stubprovider"
	"go.bison9.com/agency/providers"
)

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	type read struct {
		b   []byte
		err error
	}
	done := make(chan read)
	go func() {
		b, err := io.ReadAll(r)
		done <- read{b, err}
	}()
	defer func() { os.Stdout = orig }()
	fn()
	require.NoError(t, w.Close())
	got := <-done
	require.NoError(t, got.err)
	return string(got.b)
}

// stubConfig is a config whose provider is a stub that answers model
// "alice-model" with OK and refuses every other model's key.
func stubConfig(t *testing.T, models ...modelspec.Model) *config.Config {
	t.Helper()
	stub := stubprovider.New(t)
	stub.SetHook(func(req stubprovider.Request) *stubprovider.Step {
		if req.Model == "alice-model" {
			st := stubprovider.Reply(" OK \n")
			return &st
		}
		st := stubprovider.Unauthorized()
		return &st
	})
	home := t.TempDir()
	t.Setenv("AGENCY_HOME", home)
	cfg := config.DefaultConfig()
	cfg.Providers = []modelspec.Provider{{Name: "stub", Protocol: "openai-chat", BaseURL: stub.URL(), APIKey: "test-key"}}
	cfg.Models = models
	cfg.Agents.Defaults.Models = []string{models[0].ModelName}
	require.NoError(t, config.SaveConfig(filepath.Join(home, "config.json"), cfg))
	return cfg
}

var (
	alice = modelspec.Model{ModelName: "Alice", Model: "alice-model", Provider: "stub", Enabled: true}
	bob   = modelspec.Model{ModelName: "Bob", Model: "bob-model", Provider: "stub", Enabled: true}
	carol = modelspec.Model{ModelName: "Carol", Model: "carol-model", Provider: "stub"}
)

func runTest(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() {
		cmd := NewTestCommand()
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		err = cmd.Execute()
	})
	return out, err
}

// resultLine finds the table row for model.
func resultLine(t *testing.T, out, model string) string {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, model+" ") {
			return line
		}
	}
	t.Fatalf("no row for %s in:\n%s", model, out)
	return ""
}

func TestModelsAll(t *testing.T) {
	stubConfig(t, alice, bob, carol)
	out, err := runTest(t, "models", "-t", "5")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(out, "Testing 2 model(s) (1 disabled, skipped) (timeout: 5s)...\n\n"), out)
	assert.Contains(t, out, "MODEL                           STATUS    TIME      RESPONSE/ERROR\n")
	assert.Regexp(t, `^Alice\s+OK\s+\S+\s+OK$`, resultLine(t, out, "Alice"))
	assert.Regexp(t, `^Bob\s+FAIL\s+\S+\s+API request failed`, resultLine(t, out, "Bob"))
	assert.Contains(t, out, "Incorrect API key provided")
	assert.NotContains(t, out, "Carol")
	assert.True(t, strings.HasSuffix(out, "\n1/2 models reachable\n"), out)
}

func TestModelsAllEnabled(t *testing.T) {
	stubConfig(t, alice)
	out, err := runTest(t, "models")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(out, "Testing 1 model(s) (timeout: 30s)...\n\n"), out)
	assert.True(t, strings.HasSuffix(out, "\n1/1 models reachable\n"), out)
}

func TestModelsNoneEnabled(t *testing.T) {
	stubConfig(t, carol)
	out, err := runTest(t, "models")
	require.NoError(t, err)
	assert.Equal(t, "No enabled models in models.\n", out)
}

func TestModelsOne(t *testing.T) {
	stubConfig(t, alice, bob)
	out, err := runTest(t, "models", "Alice")
	require.NoError(t, err)
	assert.Regexp(t, `^Alice\s+OK`, resultLine(t, out, "Alice"))
	assert.NotContains(t, out, "Bob")
	assert.True(t, strings.HasSuffix(out, "\n1/1 models reachable\n"), out)
}

func TestModelCommand(t *testing.T) {
	stubConfig(t, alice, carol)
	out, err := runTest(t, "model", "Alice")
	require.NoError(t, err)
	assert.Regexp(t, `^Alice\s+OK`, resultLine(t, out, "Alice"))

	_, err = runTest(t, "model", "Carol")
	require.Error(t, err)
	assert.Equal(t, `model "Carol" is disabled`, err.Error())

	_, err = runTest(t, "model", "Dave")
	require.Error(t, err)
	assert.Equal(t, `model "Dave" not found in models`, err.Error())
}

func TestBadConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENCY_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.json"), []byte("{bad"), 0o600))
	for _, args := range [][]string{{"models"}, {"model", "Alice"}} {
		_, err := runTest(t, args...)
		require.Error(t, err)
		assert.True(t, strings.HasPrefix(err.Error(), "failed to load config: "), err.Error())
	}
}

func TestNoModelsConfigured(t *testing.T) {
	cfg := &config.Config{}
	out := captureStdout(t, func() { require.NoError(t, testAll(cfg, time.Second)) })
	assert.Equal(t, "No enabled models in models.\n", out)
}

func TestTestModelProviderErrors(t *testing.T) {
	cfg := stubConfig(t, alice)
	cat := providers.NewCatalogBuilder(nil, nil).Build(cfg)
	defer closeCatalog(cat)

	r := testModel(cfg, cat, modelspec.Model{ModelName: "Ghost", Provider: "missing"}, time.Second)
	assert.False(t, r.ok)
	assert.Equal(t, `provider init: provider "missing" not found`, r.err)

	r = testModel(cfg, cat, modelspec.Model{ModelName: "Ghost", Provider: "stub"}, time.Second)
	assert.False(t, r.ok)
	assert.True(t, strings.HasPrefix(r.err, "provider init: "), r.err)
}

func TestFormatDuration(t *testing.T) {
	assert.Equal(t, "-", formatDuration(0))
	assert.Equal(t, "250ms", formatDuration(250*time.Millisecond))
	assert.Equal(t, "1.5s", formatDuration(1500*time.Millisecond))
}

func TestPrintResults(t *testing.T) {
	out := captureStdout(t, func() {
		printResults([]result{
			{modelName: "Alice", ok: true, response: "OK", elapsed: 20 * time.Millisecond},
			{modelName: "Bob", err: "refused"},
		})
	})
	var want bytes.Buffer
	want.WriteString("MODEL                           STATUS    TIME      RESPONSE/ERROR\n")
	want.WriteString(strings.Repeat("-", 30) + "  " + strings.Repeat("-", 8) + "  " + strings.Repeat("-", 8) + "  " + strings.Repeat("-", 40) + "\n")
	want.WriteString("Alice                           OK        20ms      OK\n")
	want.WriteString("Bob                             FAIL      -         refused\n")
	want.WriteString("\n1/2 models reachable\n")
	assert.Equal(t, want.String(), out)
}
