// Agency
// License: MIT

package cron

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// breakStore points cs at a path whose parent is a file, so every save fails.
func breakStore(t *testing.T, cs *CronService) {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cs.storePath = filepath.Join(blocker, "jobs.json")
}

func TestUpdateWatchState(t *testing.T) {
	cs := newTestService(t)
	job := addEnabledJob(t, cs, "every", new(int64(60_000)))
	if err := cs.UpdateWatchState(job.ID, "abc123", 2); err != nil {
		t.Fatal(err)
	}
	// Persisted: a fresh service over the same store sees it.
	re := NewCronService(cs.storePath, nil)
	got := re.ListJobs(true)[0].State
	if got.WatchDigest != "abc123" || got.WatchFailures != 2 {
		t.Errorf("reloaded watch state = %q/%d", got.WatchDigest, got.WatchFailures)
	}
	if err := cs.UpdateWatchState("missing", "x", 0); err == nil || !strings.Contains(err.Error(), `job "missing" not found`) {
		t.Errorf("missing job err = %v", err)
	}
}

func TestLoadAndSetOnJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	legacy := `{"version":1,"jobs":[
		{"id":"a","name":"cmd","enabled":true,"schedule":{"kind":"every","everyMs":60000},"payload":{"command":"ls"}},
		{"id":"b","name":"dlv","enabled":true,"schedule":{"kind":"every","everyMs":60000},"payload":{"deliver":true,"message":"hi"}},
		{"id":"c","name":"agt","enabled":true,"schedule":{"kind":"every","everyMs":60000},"payload":{"message":"think"}},
		{"id":"d","name":"dm","enabled":true,"schedule":{"kind":"every","everyMs":60000},"payload":{"mode":"agent","peer_kind":"direct"}}
	]}`
	cs := NewCronService(path, nil)
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cs.Load(); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{"a": {"command", "channel"}, "b": {"deliver", "channel"}, "c": {"agent", "channel"}, "d": {"agent", "direct"}}
	for _, j := range cs.ListJobs(true) {
		if got := [2]string{j.Payload.Mode, j.Payload.PeerKind}; got != want[j.ID] {
			t.Errorf("job %s mode/peerKind = %v, want %v", j.ID, got, want[j.ID])
		}
	}

	var ran []string
	cs.SetOnJob(func(_ context.Context, j *CronJob) (string, error) { ran = append(ran, j.ID); return "", nil })
	cs.executeJobByID("c")
	if len(ran) != 1 || ran[0] != "c" {
		t.Errorf("handler set by SetOnJob ran %v", ran)
	}

	// A store that is a directory cannot be read.
	if err := NewCronService(t.TempDir(), nil).Load(); err == nil {
		t.Error("Load of a directory: want error")
	}
}

func TestStart_Errors(t *testing.T) {
	if os.Geteuid() == 0 {
		return // root ignores directory permissions
	}
	cs := newTestService(t)
	addEnabledJob(t, cs, "every", new(int64(60_000)))
	dir := filepath.Dir(cs.storePath)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	if err := cs.Start(); err == nil || !strings.Contains(err.Error(), "failed to save store") {
		t.Errorf("unwritable store Start = %v", err)
	}
	cs.Stop()
}

// Due jobs whose reset cannot be saved still run, and the operator is told.
func TestCheckJobs_SaveFailureAlerts(t *testing.T) {
	ran := make(chan string, 1)
	cs := newTestServiceWithHandler(t, func(_ context.Context, j *CronJob) (string, error) { ran <- j.ID; return "", nil })
	rec := &alertRecorder{}
	cs.SetAlerter(rec)
	cs.store.Jobs = []CronJob{makeEnabledJobWithNextRun("due", time.Now().Add(-time.Minute).UnixMilli())}
	cs.running = true
	breakStore(t, cs)
	cs.checkJobs()
	if got := <-ran; got != "due" {
		t.Errorf("ran %q", got)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.alerts) == 0 || rec.alerts[0].EventID != "cron-store" {
		t.Errorf("alerts = %+v, want a cron-store alert", rec.alerts)
	}
}

// A job removed while its handler runs is not recreated by the state update.
func TestExecuteJobByID_JobRemovedDuringRun(t *testing.T) {
	var cs *CronService
	cs = newTestServiceWithHandler(t, func(_ context.Context, j *CronJob) (string, error) {
		if _, err := cs.RemoveJob(j.ID); err != nil {
			t.Errorf("RemoveJob: %v", err)
		}
		return "", nil
	})
	job := addEnabledJob(t, cs, "every", new(int64(60_000)))
	cs.executeJobByID(job.ID)
	if n := len(cs.ListJobs(true)); n != 0 {
		t.Errorf("%d jobs after removal during run, want 0", n)
	}
}

func TestExecuteJobByID_AtJobOutcomes(t *testing.T) {
	future := time.Now().Add(time.Hour).UnixMilli()

	// An "at" job kept after its run is disabled.
	cs := newTestServiceWithHandler(t, func(_ context.Context, _ *CronJob) (string, error) { return "", nil })
	job, err := cs.AddJob("once", CronSchedule{Kind: "at", AtMS: &future}, "m", "", "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if job.Payload.Mode != "agent" || job.Payload.PeerKind != "channel" || !job.DeleteAfterRun {
		t.Errorf("AddJob defaults = %+v", job)
	}
	j := cs.ListJobs(true)[0]
	j.DeleteAfterRun = false
	if err = cs.UpdateJob(&j); err != nil {
		t.Fatal(err)
	}
	cs.executeJobByID(job.ID)
	got := cs.ListJobs(true)[0]
	if got.Enabled || got.State.NextRunAtMS != nil {
		t.Errorf("kept at-job after run: enabled=%v next=%v", got.Enabled, got.State.NextRunAtMS)
	}

	// An "at" job that cannot be removed after its run stays, and the save
	// failure is reported.
	cs2 := newTestServiceWithHandler(t, func(_ context.Context, _ *CronJob) (string, error) { return "", nil })
	rec := &alertRecorder{}
	cs2.SetAlerter(rec)
	job2, err := cs2.AddJob("once", CronSchedule{Kind: "at", AtMS: &future}, "m", "", "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	breakStore(t, cs2)
	cs2.executeJobByID(job2.ID)
	if len(rec.alerts) == 0 {
		t.Error("no alert for the failed save")
	}
}

// A recurring job with no next run (an empty cron expression) runs once and
// has no next run.
func TestExecuteJobByID_NoNextRun(t *testing.T) {
	cs := newTestServiceWithHandler(t, func(_ context.Context, _ *CronJob) (string, error) { return "", nil })
	job, err := cs.AddJob("x", CronSchedule{Kind: "cron"}, "m", "agent", "cli", "direct", "channel", "", "")
	if err != nil {
		t.Fatal(err)
	}
	cs.executeJobByID(job.ID)
	got := cs.ListJobs(true)[0]
	if got.State.NextRunAtMS != nil || got.State.LastStatus != "ok" {
		t.Errorf("state = %+v", got.State)
	}
}

func TestAddJob_SaveFailure(t *testing.T) {
	cs := newTestService(t)
	breakStore(t, cs)
	if _, err := cs.AddJob("x", CronSchedule{Kind: "every", EveryMS: new(int64(1000))}, "m", "", "", "", "", "", ""); err == nil {
		t.Error("AddJob with an unwritable store: want error")
	}
}

func TestEnableJob_Paths(t *testing.T) {
	cs := newTestService(t)
	job := addEnabledJob(t, cs, "every", new(int64(60_000)))

	got, err := cs.EnableJob(job.ID, false)
	if err != nil || got.Enabled || got.State.NextRunAtMS != nil {
		t.Fatalf("disable = %+v, %v", got, err)
	}
	got, err = cs.EnableJob(job.ID, true)
	if err != nil || !got.Enabled || got.State.NextRunAtMS == nil {
		t.Fatalf("enable = %+v, %v", got, err)
	}
	// Not found is a nil job and no error; callers report it.
	if got, err := cs.EnableJob("missing", true); got != nil || err != nil {
		t.Errorf("missing = %+v, %v", got, err)
	}

	breakStore(t, cs)
	if _, err := cs.EnableJob(job.ID, true); err == nil {
		t.Error("enable with an unwritable store: want error")
	}
}

func TestRemoveJob_NotFoundAndSaveFailure(t *testing.T) {
	cs := newTestService(t)
	job := addEnabledJob(t, cs, "every", new(int64(60_000)))
	if removed, err := cs.RemoveJob("missing"); removed || err != nil {
		t.Errorf("missing = %v, %v", removed, err)
	}
	breakStore(t, cs)
	removed, err := cs.RemoveJob(job.ID)
	if !removed || err == nil {
		t.Errorf("remove with unwritable store = %v, %v; want true and an error", removed, err)
	}
}

func TestAlertSaveFailed_NoAlerter(t *testing.T) {
	cs := newTestService(t)
	cs.alertSaveFailed(errors.New("x")) // no alerter: nothing to do, no panic
}
