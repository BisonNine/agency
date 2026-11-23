// ClawEh
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package forum

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// edgeRunningState is the State of stConfig's run right after launch.
func edgeRunningState(snap *Snapshot) *State {
	st := replayInitialState(snap)
	st.Status, st.Seq = StatusRunning, 1
	return st
}

func TestReplayApplyRefusesMalformedCommits(t *testing.T) {
	cfg := stConfig()
	snap := &Snapshot{Layers: []string{"debate", "summary"}}
	cases := map[string]struct {
		c    Commit
		want string
	}{
		"layer not enabled": {
			Commit{Seq: 2, Kind: CommitLayerStarted, Layer: "other"},
			"not an enabled layer of the snapshot",
		},
		"layer without a commit kind of its own": {
			Commit{Seq: 2, Kind: CommitAttempt},
			"names no layer",
		},
		"unknown kind": {
			Commit{Seq: 2, Kind: "bogus"},
			`unknown kind "bogus"`,
		},
		"turn without output": {
			Commit{Seq: 2, Kind: CommitTurn, Layer: "debate"},
			"has no output",
		},
		"moderation without decision": {
			Commit{Seq: 2, Kind: CommitModerated, Layer: "debate"},
			"moderation has no decision",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := replayApply(cfg, snap, edgeRunningState(snap), &c.c)
			if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want ErrCorrupt with %q", err, c.want)
			}
		})
	}

	// A snapshot layer the configuration lacks.
	snapExtra := &Snapshot{Layers: []string{"debate", "ghost"}}
	err := replayApply(cfg, snapExtra, edgeRunningState(snapExtra), &Commit{Seq: 2, Kind: CommitLayerStarted, Layer: "ghost"})
	if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "not in the configuration") {
		t.Errorf("ghost layer: err = %v", err)
	}
}

func TestCheckCommitLayerNotEnabled(t *testing.T) {
	cfg := stConfig()
	snap := &Snapshot{Layers: []string{"debate"}}
	err := checkCommit(cfg, snap, newCommitIndex(), &Commit{Seq: 2, Kind: CommitLayerStarted, Layer: "summary"})
	if err == nil || !strings.Contains(err.Error(), "not an enabled layer of the snapshot") {
		t.Errorf("err = %v", err)
	}
}

func TestVerifyCommitArtifacts(t *testing.T) {
	s := stNewStore(t)
	stLaunch(t, s, stConfig())
	turn := turnID(1, "alice")
	if err := verifyCommit(s, &Commit{Kind: CommitAttempt, Layer: "debate", Turn: "../x", Attempt: 1}, false); err == nil || err.Error() != "malformed attempt" {
		t.Errorf("malformed attempt: %v", err)
	}
	if err := verifyCommit(s, &Commit{Kind: CommitTurn, Layer: "debate", Turn: turn}, false); err == nil || err.Error() != "no output record" {
		t.Errorf("turn without output: %v", err)
	}
	stReserve(t, s, "debate", 1, turn, TurnParticipant, "alice", 1, 1, "first")
	err := verifyCommit(s, &Commit{Kind: CommitAttempt, Layer: "debate", Turn: turn, Attempt: 1}, true)
	if !errors.Is(err, os.ErrExist) || !strings.Contains(err.Error(), "reserved again without being resent") {
		t.Errorf("repeat without resend: %v", err)
	}
}

// edgeRewriteSnapshot replaces the run's snapshot.json with edit's result.
func edgeRewriteSnapshot(t *testing.T, s *forumStore, edit func(*Snapshot)) {
	t.Helper()
	snap, err := s.ReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	edit(snap)
	data, err := marshalRecord(snap)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(s.root, fileSnapshot), string(data))
}

func TestVerifySnapshotMismatches(t *testing.T) {
	t.Run("another run", func(t *testing.T) {
		s := stNewStore(t)
		stLaunch(t, s, stConfig())
		edgeRewriteSnapshot(t, s, func(snap *Snapshot) { snap.Run = 2 })
		_, _, err := verify(s)
		if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "names run 2, not 1") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("another launcher", func(t *testing.T) {
		s := stNewStore(t)
		stLaunch(t, s, stConfig())
		edgeRewriteSnapshot(t, s, func(snap *Snapshot) { snap.Origin.AgentID = "bob" })
		s.owner = "alice"
		if _, _, err := verify(s); !errors.Is(err, errForeign) {
			t.Errorf("err = %v, want errForeign", err)
		}
	})
	t.Run("configuration does not decode", func(t *testing.T) {
		s := stNewStore(t)
		stLaunch(t, s, stConfig())
		bad := []byte(`{"version":"one"}`)
		writeTestFile(t, filepath.Join(s.root, fileConfig), string(bad))
		edgeRewriteSnapshot(t, s, func(snap *Snapshot) { snap.ConfigDigest = digest(bad) })
		_, _, err := verify(s)
		if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), fileConfig) {
			t.Errorf("err = %v", err)
		}
	})
}

// edgeAppendRaw writes a commit file by hand, bypassing AppendCommit's
// checks, as a damaged or hand-edited log would hold it.
func edgeAppendRaw(t *testing.T, s *forumStore, c Commit) {
	t.Helper()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(s.root, filepath.FromSlash(commitRel(c.Seq))), string(data))
}

func TestReplayAndLoadStateOfDamagedLog(t *testing.T) {
	s := stNewStore(t)
	cfg := stConfig()
	snap := stLaunch(t, s, cfg)
	edgeAppendRaw(t, s, Commit{Seq: 2, Kind: "bogus"})
	r := stReopen(s)
	if err := r.Lock(); err != nil {
		t.Fatal(err)
	}
	defer r.Unlock()
	if _, err := replayState(r, cfg, snap); !errors.Is(err, ErrCorrupt) || !strings.HasPrefix(err.Error(), "replay state: ") {
		t.Errorf("replayState: %v", err)
	}
	if _, err := loadState(r, cfg, snap); !errors.Is(err, ErrCorrupt) || !strings.HasPrefix(err.Error(), "load state: ") {
		t.Errorf("loadState: %v", err)
	}

	// A log that cannot be read at all.
	if err := os.RemoveAll(filepath.Join(s.root, dirCommits)); err != nil {
		t.Fatal(err)
	}
	if _, err := replayState(r, cfg, snap); !errors.Is(err, ErrCorrupt) || !strings.HasPrefix(err.Error(), "replay state: ") {
		t.Errorf("replayState without log: %v", err)
	}
}
