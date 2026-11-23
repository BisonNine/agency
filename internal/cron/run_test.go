package cron

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.bison9.com/agency/cron"
)

// setupHome points the config at a fresh data directory and returns the
// job store path the commands use.
func setupHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CLAW_HOME", home)
	return filepath.Join(home, "cron", "jobs.json")
}

func runCron(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() {
		cmd := NewCronCommand()
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		err = cmd.Execute()
	})
	return out, err
}

func onlyJob(t *testing.T, storePath string) cron.CronJob {
	t.Helper()
	jobs := cron.NewCronService(storePath, nil).ListJobs(true)
	require.Len(t, jobs, 1)
	return jobs[0]
}

func TestCronRootShowsHelp(t *testing.T) {
	setupHome(t)
	cmd := NewCronCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs(nil)
	require.NoError(t, cmd.Execute())
	assert.Contains(t, buf.String(), "Manage scheduled tasks")
}

func TestCronBadConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAW_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.json"), []byte("{bad"), 0o600))
	_, err := runCron(t, "list")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "error loading config: ")
}

func TestCronAdd(t *testing.T) {
	t.Run("every", func(t *testing.T) {
		store := setupHome(t)
		out, err := runCron(t, "add", "-n", "Alice check", "-m", "hello", "-e", "90", "--channel", "slack", "--to", "C1")
		require.NoError(t, err)
		job := onlyJob(t, store)
		assert.Equal(t, "✓ Added job 'Alice check' ("+job.ID+")\n", out)
		assert.Equal(t, "every", job.Schedule.Kind)
		require.NotNil(t, job.Schedule.EveryMS)
		assert.Equal(t, int64(90000), *job.Schedule.EveryMS)
		assert.Equal(t, cron.CronPayload{Mode: "agent", Message: "hello", Channel: "slack", To: "C1", PeerKind: "channel"}, job.Payload)
		assert.True(t, job.Enabled)
	})

	t.Run("cron expression", func(t *testing.T) {
		store := setupHome(t)
		_, err := runCron(t, "add", "-n", "Bob", "-m", "hi", "-c", "0 9 * * *", "--peer-kind", "direct")
		require.NoError(t, err)
		job := onlyJob(t, store)
		assert.Equal(t, cron.CronSchedule{Kind: "cron", Expr: "0 9 * * *"}, job.Schedule)
		assert.Equal(t, "direct", job.Payload.PeerKind)
	})

	t.Run("needs a schedule", func(t *testing.T) {
		store := setupHome(t)
		_, err := runCron(t, "add", "-n", "Bob", "-m", "hi")
		require.Error(t, err)
		assert.Equal(t, "either --every or --cron must be specified", err.Error())
		assert.NoFileExists(t, store)
	})

	t.Run("store not writable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		store := setupHome(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(store), 0o500))
		_, err := runCron(t, "add", "-n", "Bob", "-m", "hi", "-e", "5")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "error adding job: ")
	})
}

func TestCronListOutput(t *testing.T) {
	store := setupHome(t)
	out, err := runCron(t, "list")
	require.NoError(t, err)
	assert.Equal(t, "No scheduled jobs.\n", out)

	cs := cron.NewCronService(store, nil)
	every := int64(60000)
	_, err = cs.AddJob("Every", cron.CronSchedule{Kind: "every", EveryMS: &every}, strings.Repeat("x", 81), "agent", "slack", "C1", "direct")
	require.NoError(t, err)
	cmdJob, err := cs.AddJob("Listen", cron.CronSchedule{Kind: cron.KindListen}, "", "agent", "slack", "C2", "channel")
	require.NoError(t, err)
	cmdJob.Payload.Command = "watch.sh"
	require.NoError(t, cs.UpdateJob(cmdJob))
	once, err := cs.AddJob("Once", cron.CronSchedule{Kind: "at"}, "short", "agent", "telegram", "T1", "channel")
	require.NoError(t, err)
	_, err = cs.EnableJob(once.ID, false)
	require.NoError(t, err)

	out, err = runCron(t, "list")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(out, "Scheduled Jobs:\n\n"), out)
	assert.Contains(t, out, "    Schedule: every 60s\n")
	assert.Contains(t, out, "    PeerKind: direct\n")
	assert.Contains(t, out, "    Message:  "+strings.Repeat("x", 80)+"...\n")
	assert.Contains(t, out, "    Schedule: listen (continuous)\n")
	assert.Contains(t, out, "    Command:  watch.sh\n")
	assert.Contains(t, out, "  Once ("+once.ID+")\n    Agent:    (operator/legacy)\n    Schedule: one-time\n    Status:   disabled\n    Next run: scheduled\n"+
		"    Channel:  telegram\n    To:       T1\n    Message:  short\n\n")
}

func TestCronRemove(t *testing.T) {
	store := setupHome(t)
	_, err := runCron(t, "add", "-n", "Bob", "-m", "hi", "-e", "5")
	require.NoError(t, err)
	id := onlyJob(t, store).ID

	out, err := runCron(t, "remove", id)
	require.NoError(t, err)
	assert.Equal(t, "✓ Removed job "+id+"\n", out)
	assert.Empty(t, cron.NewCronService(store, nil).ListJobs(true))

	out, err = runCron(t, "remove", id)
	require.NoError(t, err)
	assert.Equal(t, "✗ Job "+id+" not found\n", out)
}

func TestCronEnableDisable(t *testing.T) {
	store := setupHome(t)
	_, err := runCron(t, "add", "-n", "Bob", "-m", "hi", "-e", "5")
	require.NoError(t, err)
	id := onlyJob(t, store).ID

	out, err := runCron(t, "disable", id)
	require.NoError(t, err)
	assert.Equal(t, "✓ Job 'Bob' disabled\n", out)
	assert.False(t, onlyJob(t, store).Enabled)

	out, err = runCron(t, "enable", id)
	require.NoError(t, err)
	assert.Equal(t, "✓ Job 'Bob' enabled\n", out)
	assert.True(t, onlyJob(t, store).Enabled)

	for _, sub := range []string{"enable", "disable"} {
		out, err = runCron(t, sub, "missing")
		require.NoError(t, err)
		assert.Equal(t, "✗ Job missing not found\n", out)
	}
}

// A store that cannot be saved is reported for remove, enable and disable.
func TestCronSaveFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	store := setupHome(t)
	_, err := runCron(t, "add", "-n", "Bob", "-m", "hi", "-e", "5")
	require.NoError(t, err)
	id := onlyJob(t, store).ID
	dir := filepath.Dir(store)
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() {
		if chmodErr := os.Chmod(dir, 0o700); chmodErr != nil {
			t.Error(chmodErr)
		}
	})

	for _, sub := range []string{"enable", "disable"} {
		out, runErr := runCron(t, sub, id)
		require.NoError(t, runErr)
		assert.True(t, strings.HasPrefix(out, "✗ Failed to update job "+id+": "), out)
	}
	out, err := runCron(t, "remove", id)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(out, "✗ Failed to remove job "+id+": "), out)
}
