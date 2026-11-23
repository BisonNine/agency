// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/accounting"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/media"
	"go.bison9.com/agency/voice"
)

// trCovPricedTranscriber transcribes everything as text at cost.
type trCovPricedTranscriber struct {
	text string
	cost float64
}

func (trCovPricedTranscriber) Name() string { return "priced" }

func (p trCovPricedTranscriber) Transcribe(context.Context, accounting.Identity, voice.AudioFile) (*voice.TranscriptionResponse, error) {
	c := p.cost
	return &voice.TranscriptionResponse{Text: p.text, CostUSD: &c}, nil
}

// trCovStore stores data under name with meta and returns its ref and path.
func trCovStore(t *testing.T, store *media.FileMediaStore, name string, data []byte, meta media.MediaMeta) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return mustStore(t, store, path, meta), path
}

// trCovLoop is a test loop with a media store and transcriber.
func trCovLoop(t *testing.T, tr voice.Transcriber) (*testLoop, *media.FileMediaStore) {
	t.Helper()
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	tl := newTestAgentLoop(t)
	store := media.NewFileMediaStore()
	tl.al.SetMediaStore(store)
	tl.al.SetTranscriber(tr)
	return tl, store
}

// Audio is transcribed and billed; refs that do not resolve and non-audio
// attachments stay attached; a transcript with no annotation to replace is
// appended, and an annotation left without a transcript stays as it was.
func TestTrCov_TranscribeKeepsOtherMediaAndAppends(t *testing.T) {
	tl, store := trCovLoop(t, trCovPricedTranscriber{text: "hello", cost: 0.25})
	audio, _ := trCovStore(t, store, "v.ogg", []byte("OggS"), media.MediaMeta{Filename: "v.ogg", ContentType: "audio/ogg"})
	doc, _ := trCovStore(t, store, "d.txt", []byte("x"), media.MediaMeta{Filename: "d.txt", ContentType: "text/plain"})

	t.Run("appended", func(t *testing.T) {
		msg := bus.InboundMessage{Channel: "test", ChatID: "c1", Content: "see attached", Media: []string{"media://unknown", doc, audio}}
		got, ok := tl.al.transcribeAudioInMessage(context.Background(), msg, nil)
		if !ok {
			t.Fatal("audio not reported")
		}
		if want := "see attached\n[voice: hello]"; got.Content != want {
			t.Fatalf("content = %q, want %q", got.Content, want)
		}
		if !slices.Equal(got.Media, []string{"media://unknown", doc}) {
			t.Fatalf("media = %v, want the unknown ref and the document kept", got.Media)
		}
	})

	t.Run("extra annotation kept", func(t *testing.T) {
		msg := bus.InboundMessage{Channel: "test", ChatID: "c1", Content: "[voice] and [audio]", Media: []string{audio}}
		got, _ := tl.al.transcribeAudioInMessage(context.Background(), msg, nil)
		if want := "[voice: hello] and [audio]"; got.Content != want {
			t.Fatalf("content = %q, want %q", got.Content, want)
		}
	})

	tl.al.spend.mu.Lock()
	total := tl.al.spend.total
	tl.al.spend.mu.Unlock()
	if total != 0.5 {
		t.Fatalf("spend recorded = %v, want 0.5 (two transcriptions at 0.25)", total)
	}
}

// A message whose media holds no audio is returned unchanged.
func TestTrCov_TranscribeNoAudio(t *testing.T) {
	tl, store := trCovLoop(t, failTranscriber{t: t})
	doc, _ := trCovStore(t, store, "d.txt", []byte("x"), media.MediaMeta{Filename: "d.txt", ContentType: "text/plain"})
	msg := bus.InboundMessage{Content: "hi", Media: []string{doc}}

	got, ok := tl.al.transcribeAudioInMessage(context.Background(), msg, nil)

	if ok || got.Content != "hi" || !slices.Equal(got.Media, []string{doc}) {
		t.Fatalf("got %+v (audio=%v), want the message unchanged", got, ok)
	}
}

// The alert for an untranscribed voice message carries at most 200
// characters of the error.
func TestTrCov_AlertUntranscribedTruncates(t *testing.T) {
	tl := newTestAgentLoop(t)
	rec := newLoopCovAlerts()
	tl.al.SetAlerter(rec)

	tl.al.alertUntranscribed("telegram", errors.New(strings.Repeat("é", 250)))

	got := rec.next(t)
	if got.EventID != "voice:failed:telegram" || got.Details != strings.Repeat("é", 200) {
		t.Fatalf("alert = %+v, want 200 characters of detail for telegram", got)
	}
}

// Received attachments that cannot be saved are skipped; with nothing saved
// there is no note. A workspace whose tmp folder cannot be made gets none
// either.
func TestTrCov_MaterializeInboundMediaFailures(t *testing.T) {
	tl, store := trCovLoop(t, nil)
	agent := mustGetAgent(t, tl.al)
	goneRef, gonePath := trCovStore(t, store, "gone.txt", []byte("x"), media.MediaMeta{Filename: "gone.txt"})
	if err := os.Remove(gonePath); err != nil {
		t.Fatal(err)
	}

	msg := bus.InboundMessage{Media: []string{"media://unknown", goneRef}}
	if note := tl.al.materializeInboundMedia(msg, agent); note != "" {
		t.Fatalf("note = %q, want none when nothing was saved", note)
	}

	tmp := filepath.Join(agent.Workspace, "tmp")
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	okRef, _ := trCovStore(t, store, "ok.txt", []byte("x"), media.MediaMeta{Filename: "ok.txt"})
	if note := tl.al.materializeInboundMedia(bus.InboundMessage{Media: []string{okRef}}, agent); note != "" {
		t.Fatalf("note = %q, want none when tmp cannot be made", note)
	}
}

// The sweep skips folders, ignores a missing folder, and leaves (logging) an
// expired file it may not remove.
func TestTrCov_SweepReceivedMedia(t *testing.T) {
	buf := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(buf))
	sweepReceivedMedia(filepath.Join(t.TempDir(), "missing"), time.Now())

	dir := t.TempDir()
	old := time.Now().Add(-2 * receivedMediaTTL)
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "old.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{sub, file} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Errorf("restore %s: %v", dir, err)
		}
	})

	sweepReceivedMedia(dir, time.Now())

	if _, err := os.Stat(sub); err != nil {
		t.Fatalf("folder removed: %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("file removed from a read-only folder: %v", err)
	}
	if !strings.Contains(buf.String(), "Failed to remove expired received media file") {
		t.Fatalf("no warning logged:\n%s", buf.String())
	}
}

// A received file with no usable name is called "file"; a ref with no id
// keeps the bare name.
func TestTrCov_ReceivedFileNameFallbacks(t *testing.T) {
	cases := []struct{ ref, name, want string }{
		{"media://abcd-ef", "...", "abcd_file"},
		{"media://abcd-ef", "", "abcd_file"},
		{"media://", "photo.jpg", "photo.jpg"},
	}
	for _, c := range cases {
		if got := receivedFileName(c.ref, c.name); got != c.want {
			t.Errorf("receivedFileName(%q, %q) = %q, want %q", c.ref, c.name, got, c.want)
		}
	}
}

// Copying fails on a missing source, an unwritable destination and a
// source that cannot be read.
func TestTrCov_CopyFileContentsFailures(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyFileContents(filepath.Join(dir, "missing"), filepath.Join(dir, "dst"), 0o600); err == nil {
		t.Error("copy from a missing source succeeded")
	}
	if err := copyFileContents(src, filepath.Join(dir, "no", "dst"), 0o600); err == nil {
		t.Error("copy into a missing folder succeeded")
	}
	if err := copyFileContents(dir, filepath.Join(dir, "dst2"), 0o600); err == nil {
		t.Error("copy of a folder succeeded")
	}
	if err := copyFileContents(src, filepath.Join(dir, "dst3"), 0o640); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if info, err := os.Stat(filepath.Join(dir, "dst3")); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("copied file mode = %v (%v), want 0640", info.Mode().Perm(), err)
	}
}

// With echo on, feedback needs a channel manager; one that cannot deliver
// to the channel logs the failure.
func TestTrCov_SendTranscriptionFeedbackFailures(t *testing.T) {
	buf := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(buf))
	tl := newTestAgentLoop(t)
	tl.cfg.TaskModels.EchoTranscription = true

	tl.al.sendTranscriptionFeedback(context.Background(), "test", "c1", "m1", "Transcript: hi") // no manager: nothing

	withEchoChannel(t, tl.al, tl.msgBus)
	tl.al.sendTranscriptionFeedback(context.Background(), "nowhere", "c1", "m1", "Transcript: hi")
	if !strings.Contains(buf.String(), "Failed to send transcription feedback") {
		t.Fatalf("no warning logged:\n%s", buf.String())
	}
}
