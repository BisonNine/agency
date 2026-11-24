//go:build !windows

package cron

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tenebris-tech/alerter"
	"go.bison9.com/agent/clock"
)

// setAsideNow is the fake time the set-aside tests run at.
var setAsideNow = time.Date(2026, 11, 23, 10, 15, 0, 0, time.UTC)

// damagedService writes content (at mode) as the store and returns a service
// for it with a fake clock and an alert recorder.
func damagedService(t *testing.T, content []byte, mode os.FileMode) (*CronService, *alertRecorder, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cron")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "jobs.json")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	cs := NewCronService(path, nil)
	cs.clock = clock.NewFake(setAsideNow)
	rec := &alertRecorder{}
	cs.SetAlerter(rec)
	return cs, rec, path
}

// requireSetAside checks that the damaged store was set aside as want with
// its bytes and mode, that a new empty store took its place, that the one
// alert says so, and that a job added afterwards is saved.
func requireSetAside(t *testing.T, cs *CronService, rec *alertRecorder, path, want string, content []byte, mode os.FileMode) {
	t.Helper()
	fi, err := os.Stat(want)
	if err != nil {
		t.Fatalf("set-aside file: %v", err)
	}
	if fi.Mode().Perm() != mode {
		t.Errorf("set-aside mode = %04o, want %04o", fi.Mode().Perm(), mode)
	}
	if cerr := os.Chmod(want, 0o600); cerr != nil {
		t.Fatal(cerr)
	}
	if got := mustRead(t, want); !bytes.Equal(got, content) {
		t.Errorf("set-aside bytes = %q, want %q", got, content)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	var store CronStore
	if err := json.Unmarshal(data, &store); err != nil || store.Version != 1 || len(store.Jobs) != 0 {
		t.Errorf("new store = %s, %v; want an empty valid store", data, err)
	}

	if len(rec.alerts) != 1 {
		t.Fatalf("alerts = %+v, want one", rec.alerts)
	}
	a := rec.alerts[0]
	wantDesc := "The scheduled jobs file could not be read and was set aside as " + want + ", so no scheduled jobs are running"
	if a.Title != "Cron store unreadable" || a.EventID != "cron-store" || a.Priority != alerter.Normal || a.Description != wantDesc {
		t.Errorf("alert = %+v, want description %q", a, wantDesc)
	}

	if st := cs.Status(); st["enabled"] != true || st["jobs"] != 0 {
		t.Errorf("status = %v, want running with no jobs", st)
	}
	if _, err := cs.AddJob("after", CronSchedule{Kind: "every", EveryMS: new(int64(60_000))}, "hi", "agent", "cli", "direct", "channel", ""); err != nil {
		t.Fatalf("AddJob after set-aside: %v", err)
	}
	if jobs := NewCronService(path, nil).ListJobs(true); len(jobs) != 1 || jobs[0].Name != "after" {
		t.Errorf("saved jobs = %+v, want the added job", jobs)
	}
}

// TestStart_SetsAsideUnparseableStore: a store whose contents cannot be
// parsed is renamed with its bytes and mode, an empty store replaces it, and
// the service starts and saves new jobs.
func TestStart_SetsAsideUnparseableStore(t *testing.T) {
	content := []byte("{not json")
	cs, rec, path := damagedService(t, content, 0o640)
	if err := cs.Start(); err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(cs.Stop)
	requireSetAside(t, cs, rec, path, path+".unreadable-20261123-101500", content, 0o640)
}

// TestStart_SetsAsideUnreadableStore: a store that cannot be read (mode 000)
// is handled the same way.
func TestStart_SetsAsideUnreadableStore(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 000 file")
	}
	content := []byte(`{"version":1,"jobs":[]}`)
	cs, rec, path := damagedService(t, content, 0)
	if err := cs.Start(); err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(cs.Stop)
	requireSetAside(t, cs, rec, path, path+".unreadable-20261123-101500", content, 0)
}

// TestStart_SetAsideNameNotReused: an earlier set-aside file with the same
// name is kept, and the new one gets a numbered name.
func TestStart_SetAsideNameNotReused(t *testing.T) {
	content := []byte("{not json")
	cs, rec, path := damagedService(t, content, 0o600)
	earlier := path + ".unreadable-20261123-101500"
	if err := os.WriteFile(earlier, []byte("earlier"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cs.Start(); err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(cs.Stop)
	if got, err := os.ReadFile(earlier); err != nil || string(got) != "earlier" {
		t.Errorf("earlier set-aside file = %q, %v; want it untouched", got, err)
	}
	requireSetAside(t, cs, rec, path, earlier+"-2", content, 0o600)
}

// TestStart_RestartSetsAsideAgain: a store damaged again after a start is set
// aside again when the service is built and started anew (a config reload),
// under its own name.
func TestStart_RestartSetsAsideAgain(t *testing.T) {
	cs, _, path := damagedService(t, []byte("{not json"), 0o600)
	if err := cs.Start(); err != nil {
		t.Fatal(err)
	}
	cs.Stop()

	content := []byte("[broken again")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	again := NewCronService(path, nil)
	again.clock = clock.NewFake(setAsideNow.Add(time.Minute))
	rec := &alertRecorder{}
	again.SetAlerter(rec)
	if err := again.Start(); err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(again.Stop)
	requireSetAside(t, again, rec, path, path+".unreadable-20261123-101600", content, 0o600)
	if _, err := os.Stat(path + ".unreadable-20261123-101500"); err != nil {
		t.Errorf("first set-aside file: %v", err)
	}
}

// TestStart_MissingStoreNoAlert: a store that does not exist is created
// empty, with no alert and nothing set aside.
func TestStart_MissingStoreNoAlert(t *testing.T) {
	cs := newTestService(t)
	rec := &alertRecorder{}
	cs.SetAlerter(rec)
	if err := cs.Start(); err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(cs.Stop)
	if len(rec.alerts) != 0 {
		t.Errorf("alerts = %+v, want none", rec.alerts)
	}
	if m := mustGlob(t, cs.storePath+".unreadable-*"); len(m) != 0 {
		t.Errorf("set-aside files = %v, want none", m)
	}
	if _, err := os.Stat(cs.storePath); err != nil {
		t.Errorf("store not created: %v", err)
	}
}

// TestStart_SetAsideImpossible: when the damaged store cannot be renamed
// (read-only folder) the service still starts, with no jobs, never writes
// over the file, and says so in one alert. Once the file is repaired the
// service reads it and saves again.
func TestStart_SetAsideImpossible(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	content := []byte("{not json")
	cs, rec, path := damagedService(t, content, 0o600)
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	if err := cs.Start(); err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(cs.Stop)

	if len(rec.alerts) != 1 {
		t.Fatalf("alerts = %+v, want one", rec.alerts)
	}
	wantDesc := "The scheduled jobs file " + path + " could not be read or set aside, so no scheduled jobs are running"
	if a := rec.alerts[0]; a.Title != "Cron store unreadable" || a.EventID != "cron-store" || a.Description != wantDesc {
		t.Errorf("alert = %+v, want description %q", a, wantDesc)
	}
	if st := cs.Status(); st["enabled"] != true || st["jobs"] != 0 {
		t.Errorf("status = %v, want running with no jobs", st)
	}
	_, err := cs.AddJob("x", CronSchedule{Kind: "every", EveryMS: new(int64(60_000))}, "hi", "agent", "cli", "direct", "channel", "")
	if err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Errorf("AddJob = %v, want the damaged-store error", err)
	}
	if len(cs.ListJobs(true)) != 0 {
		t.Error("a refused job was kept in memory")
	}
	if got := mustRead(t, path); !bytes.Equal(got, content) {
		t.Errorf("store = %q, want it untouched", got)
	}
	if m := mustGlob(t, path+".unreadable-*"); len(m) != 0 {
		t.Errorf("set-aside files = %v, want none", m)
	}

	// The operator repairs the file: the service reads it and saves again.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"jobs":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	cs.checkJobs()
	if _, err := cs.AddJob("y", CronSchedule{Kind: "every", EveryMS: new(int64(60_000))}, "hi", "agent", "cli", "direct", "channel", ""); err != nil {
		t.Errorf("AddJob after repair = %v", err)
	}
}

// TestNewCronService_DamagedStoreNotOverwritten: a service built on a store
// it cannot load (the command line's) refuses to add a job rather than write
// over the file.
func TestNewCronService_DamagedStoreNotOverwritten(t *testing.T) {
	content := []byte("{not json")
	cs, rec, path := damagedService(t, content, 0o600)
	if _, err := cs.AddJob("x", CronSchedule{Kind: "every", EveryMS: new(int64(60_000))}, "hi", "agent", "cli", "direct", "channel", ""); err == nil {
		t.Error("AddJob on a damaged store succeeded")
	}
	if got := mustRead(t, path); !bytes.Equal(got, content) {
		t.Errorf("store = %q, want it untouched", got)
	}
	if len(rec.alerts) != 0 {
		t.Errorf("alerts = %+v, want none before Start", rec.alerts)
	}
}

func mustGlob(t *testing.T, pattern string) []string {
	t.Helper()
	m, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestStart_StoreIsDirectory: a store path that is a folder cannot be read
// or renamed onto the reserved name; the reserved name is removed again and
// the service runs with no jobs without saving.
func TestStart_StoreIsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	if err := os.MkdirAll(filepath.Join(path, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	cs := NewCronService(path, nil)
	rec := &alertRecorder{}
	cs.SetAlerter(rec)
	if err := cs.Start(); err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(cs.Stop)
	if m := mustGlob(t, path+".unreadable-*"); len(m) != 0 {
		t.Errorf("set-aside files = %v, want none", m)
	}
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		t.Errorf("store folder = %v, %v; want it untouched", fi, err)
	}
	if len(rec.alerts) != 1 || !strings.Contains(rec.alerts[0].Description, "could not be read or set aside") {
		t.Errorf("alerts = %+v", rec.alerts)
	}
	cs.mu.Lock()
	err := cs.saveStoreUnsafe()
	cs.mu.Unlock()
	if err == nil {
		t.Error("save over a store that could not be set aside succeeded")
	}
}

// TestSetAside_NoFreeName: when every numbered name is taken the file stays
// where it is.
func TestSetAside_NoFreeName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := path + ".unreadable-20261123-101500"
	for n := 1; n <= 100; n++ {
		name := base
		if n > 1 {
			name = base + "-" + strconv.Itoa(n)
		}
		if err := os.WriteFile(name, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := setAside(path, setAsideNow); err == nil {
		t.Fatal("setAside with no free name succeeded")
	}
	if got := mustRead(t, path); string(got) != "{" {
		t.Errorf("store = %q, want it untouched", got)
	}
}
