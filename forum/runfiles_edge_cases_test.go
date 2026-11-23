// ClawEh
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package forum

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"
)

// edgeBrokenLog replaces the run's commits/ with a file, so every read of
// the log fails with ErrCorrupt, and returns a fresh handle of the run
// (whose commit index is not loaded yet).
func edgeBrokenLog(t *testing.T, s *forumStore) *forumStore {
	t.Helper()
	commits := filepath.Join(s.root, dirCommits)
	if err := os.RemoveAll(commits); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, commits, "x")
	return stReopen(s)
}

func TestRunFilesRefuseOnCorruptLog(t *testing.T) {
	s := edgeBrokenLog(t, stNewStore(t))
	turn := turnID(1, "alice")
	if err := s.WriteParticipants(&Participants{}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("WriteParticipants: %v, want ErrCorrupt", err)
	}
	req := &AttemptRequest{Layer: "debate", Round: 1, Turn: turn, Attempt: 1, Kind: TurnParticipant, Participant: "alice"}
	if err := s.WriteAttemptRequest(req); !errors.Is(err, ErrCorrupt) {
		t.Errorf("WriteAttemptRequest: %v, want ErrCorrupt", err)
	}
	if err := s.WriteAttemptReply("debate", turn, 1, &AttemptReply{}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("WriteAttemptReply: %v, want ErrCorrupt", err)
	}
	if _, err := s.ListAttempts("debate"); !errors.Is(err, ErrCorrupt) {
		t.Errorf("ListAttempts: %v, want ErrCorrupt", err)
	}
	out := &OutputRecord{LayerID: "debate", Turn: turn, Attempt: 1, Format: FormatText}
	if err := s.WriteOutput(out, []byte("x"), nil); !errors.Is(err, ErrCorrupt) {
		t.Errorf("WriteOutput: %v, want ErrCorrupt", err)
	}
}

func TestRunFilesBadIDs(t *testing.T) {
	s := stNewStore(t)
	if _, err := s.ReadLayerInputs("../x"); err == nil {
		t.Error("ReadLayerInputs accepted a path")
	}
	if err := s.WriteAttemptReply("debate", "../t", 1, &AttemptReply{}); err == nil {
		t.Error("WriteAttemptReply accepted a path")
	}
	if err := s.WriteOutput(&OutputRecord{LayerID: "debate", Turn: "t", Attempt: 0}, nil, nil); err == nil {
		t.Error("WriteOutput accepted attempt 0")
	}
	if s.has("../x") {
		t.Error("has reports a path outside the run")
	}
	if _, _, err := s.ReadPrefix("../x", 1); err == nil {
		t.Error("ReadPrefix read outside the run")
	}
	if _, _, err := s.ReadPrefix("missing.txt", 1); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadPrefix(missing) = %v, want ErrNotExist", err)
	}
}

func TestWriteAttemptRequestFailures(t *testing.T) {
	s := stNewStore(t)
	stLaunch(t, s, stConfig())
	turn := turnID(1, "alice")
	req := func() *AttemptRequest {
		return &AttemptRequest{
			Layer: "debate", Round: 1, Turn: turn, Attempt: 1, Kind: TurnParticipant,
			Participant: "alice", AgentID: "alice", SentAt: time.Now().UTC(), ThroughSeq: 1, Message: "hi",
		}
	}

	// The layer folder is a file.
	writeTestFile(t, filepath.Join(s.root, dirLayers, "debate"), "x")
	wantErrContains(t, "layer a file", s.WriteAttemptRequest(req()), "write attempt request", "not a directory")
	if err := os.Remove(filepath.Join(s.root, dirLayers, "debate")); err != nil {
		t.Fatal(err)
	}

	// The turn folder is read-only: the temporary attempt folder cannot be made.
	turnDir := filepath.Join(s.root, dirLayers, "debate", dirCalls, turn)
	if err := os.MkdirAll(turnDir, 0o700); err != nil {
		t.Fatal(err)
	}
	chmodRestored(t, turnDir, 0o500)
	if err := s.WriteAttemptRequest(req()); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("read-only turn folder: %v, want ErrPermission", err)
	}
	if err := os.Chmod(turnDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// An orphan attempt folder that cannot be removed fails the write and
	// leaves no temporary folder behind.
	orphan := filepath.Join(turnDir, "1", "stuck")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(orphan, "f"), "x")
	chmodRestored(t, orphan, 0o500)
	if err := s.WriteAttemptRequest(req()); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("stuck orphan: %v, want ErrPermission", err)
	}
	entries, err := os.ReadDir(turnDir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "1" {
		t.Errorf("turn folder holds %v after the failure", entries)
	}
}

func TestResendRefusals(t *testing.T) {
	s := stNewStore(t)
	stLaunch(t, s, stConfig())
	turn := turnID(1, "alice")
	stReserve(t, s, "debate", 1, turn, TurnParticipant, "alice", 1, 1, "first")
	rel := attemptRel("debate", turn, 1)
	resend := &AttemptRequest{Layer: "debate", Round: 1, Turn: turn, Attempt: 1, Kind: TurnParticipant, Participant: "alice", ThroughSeq: 1, Message: "again", Resent: true}

	// reply.json that is not a regular file cannot be checked.
	replyPath := filepath.Join(s.root, filepath.FromSlash(rel), fileReply)
	if err := os.Mkdir(replyPath, 0o700); err != nil {
		t.Fatal(err)
	}
	wantErrContains(t, "reply a folder", s.WriteAttemptRequest(resend), "write attempt request", "not a regular file")
	if err := os.Remove(replyPath); err != nil {
		t.Fatal(err)
	}

	// request.json that cannot be replaced.
	reqPath := filepath.Join(s.root, filepath.FromSlash(rel), fileRequest)
	if err := os.Remove(reqPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(reqPath, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	wantErrContains(t, "request a folder", s.WriteAttemptRequest(resend), "write attempt request")

	// A reply without its request is refused.
	wantErrContains(t, "reply without request", s.WriteAttemptReply("debate", turn, 1, &AttemptReply{Outcome: OutcomeOK, Text: "x"}), "write attempt reply")
	if err := os.RemoveAll(reqPath); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAttemptReply("debate", turn, 1, &AttemptReply{Outcome: OutcomeOK, Text: "x"}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("reply with the request missing: %v, want ErrNotExist", err)
	}
}

func TestResendAfterReplyRefused(t *testing.T) {
	s := stNewStore(t)
	stLaunch(t, s, stConfig())
	turn := turnID(1, "alice")
	stReserve(t, s, "debate", 1, turn, TurnParticipant, "alice", 1, 1, "first")
	stReply(t, s, "debate", turn, 1, `{"claim":"x"}`, nil)
	resend := &AttemptRequest{Layer: "debate", Round: 1, Turn: turn, Attempt: 1, Kind: TurnParticipant, Participant: "alice", ThroughSeq: 1, Message: "again", Resent: true}
	err := s.WriteAttemptRequest(resend)
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("resend after a reply: %v, want ErrExist", err)
	}
	wantErrContains(t, "resend after a reply", err, "the attempt has a reply")
}

func TestWriteOutputFileFailures(t *testing.T) {
	s := stNewStore(t)
	stLaunch(t, s, stConfig())
	turn := turnID(1, "alice")
	stReserve(t, s, "debate", 1, turn, TurnParticipant, "alice", 1, 1, "first")
	dir := filepath.Join(s.root, filepath.FromSlash(attemptRel("debate", turn, 1)))

	busy := filepath.Join(dir, fileOutput+FormatJSON.Extension(), "x")
	if err := os.MkdirAll(busy, 0o700); err != nil {
		t.Fatal(err)
	}
	out := &OutputRecord{LayerID: "debate", Turn: turn, Attempt: 1, Format: FormatJSON}
	wantErrContains(t, "output a folder", s.WriteOutput(out, []byte(`{}`), nil), "write output")
	if err := os.RemoveAll(filepath.Dir(busy)); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(dir, filePublished+FormatJSON.Extension(), "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	wantErrContains(t, "published a folder", s.WriteOutput(out, []byte(`{}`), []byte(`{}`)), "write published")
}

func TestWriteTranscriptNotRegular(t *testing.T) {
	s := stNewStore(t)
	if err := os.Mkdir(filepath.Join(s.root, fileTranscript), 0o700); err != nil {
		t.Fatal(err)
	}
	wantErrContains(t, "transcript a folder", s.AppendTranscript("x\n"), "append transcript", "not a regular file")
	wantErrContains(t, "transcript a folder", s.ReplaceTranscript([]byte("x\n")), "replace transcript")
}

func TestReadCommitsLayoutErrors(t *testing.T) {
	s := stNewStore(t)
	if err := os.RemoveAll(filepath.Join(s.root, dirCommits)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadCommits(); !errors.Is(err, ErrCorrupt) {
		t.Errorf("commits/ missing: %v, want ErrCorrupt", err)
	}

	s = stNewStore(t)
	commits := filepath.Join(s.root, dirCommits)
	chmodRestored(t, commits, 0)
	if _, err := s.ReadCommits(); !errors.Is(err, ErrCorrupt) {
		t.Errorf("commits/ unreadable: %v, want ErrCorrupt", err)
	}

	s = stNewStore(t)
	stLaunch(t, s, stConfig())
	chmodRestored(t, filepath.Join(s.root, filepath.FromSlash(commitRel(1))), 0)
	if _, err := stReopen(s).ReadCommits(); !errors.Is(err, ErrCorrupt) {
		t.Errorf("commit unreadable: %v, want ErrCorrupt", err)
	}
}

func TestAppendCommitContractErrors(t *testing.T) {
	// A snapshot that does not decode.
	s := stNewStore(t)
	writeTestFile(t, filepath.Join(s.root, fileSnapshot), "{")
	if _, err := nextAppend(s, &Commit{Kind: CommitLaunched}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("bad snapshot: %v, want ErrCorrupt", err)
	}

	// A snapshot without the configuration.
	s = stNewStore(t)
	stLaunch(t, s, stConfig())
	cfgPath := filepath.Join(s.root, fileConfig)
	if err := os.Remove(cfgPath); err != nil {
		t.Fatal(err)
	}
	r := stReopen(s)
	if _, err := nextAppend(r, &Commit{Kind: CommitPauseRequested}); !errors.Is(err, ErrNotFound) {
		t.Errorf("config missing: %v, want ErrNotFound", err)
	}

	// A configuration that no longer decodes.
	writeTestFile(t, cfgPath, `{"version":"one"}`)
	r = stReopen(s)
	if _, err := nextAppend(r, &Commit{Kind: CommitPauseRequested}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("bad config: %v, want ErrCorrupt", err)
	}
}

func TestAppendCommitResendUnreadableRequest(t *testing.T) {
	s := stNewStore(t)
	stLaunch(t, s, stConfig())
	turn := turnID(1, "alice")
	stReserve(t, s, "debate", 1, turn, TurnParticipant, "alice", 1, 1, "first")
	chmodRestored(t, filepath.Join(s.root, filepath.FromSlash(path.Join(attemptRel("debate", turn, 1), fileRequest))), 0)
	_, err := nextAppend(s, &Commit{
		Kind: CommitAttempt, Layer: "debate", Round: 1, Turn: turn, TurnKind: TurnParticipant,
		Participant: "alice", Attempt: 1, ThroughSeq: 1,
	})
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("reserve again with an unreadable request: %v, want ErrPermission", err)
	}
}
