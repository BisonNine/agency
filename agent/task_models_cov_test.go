// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/spawnllm/protocoltypes"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/media"
	toolsagents "go.bison9.com/agency/tools/agents"
)

func TestAudioExtensionAndFormatType(t *testing.T) {
	exts := map[string]string{
		"audio/mpeg": ".mp3", "audio/MP3": ".mp3", "audio/wav; codec=1": ".wav", "audio/x-wav": ".wav",
		"audio/ogg": ".ogg", "audio/opus": ".opus", "audio/aac": ".aac", "audio/flac": ".flac",
		"": ".bin", "video/mp4": ".bin",
	}
	for ct, want := range exts {
		if got := audioExtension(ct); got != want {
			t.Errorf("audioExtension(%q) = %q, want %q", ct, got, want)
		}
	}
	types := map[string]string{
		"mp3": "audio/mpeg", "mp3_44100_128": "audio/mpeg", "WAV": "audio/wav", "opus": "audio/opus",
		"aac": "audio/aac", "flac": "audio/flac", "pcm": "", "": "",
	}
	for f, want := range types {
		if got := formatType(f); got != want {
			t.Errorf("formatType(%q) = %q, want %q", f, got, want)
		}
	}
}

func TestUsageText(t *testing.T) {
	tests := []struct {
		u      *protocoltypes.TaskUsage
		images int
		want   string
	}{
		{nil, 0, ""},
		{&protocoltypes.TaskUsage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8}, 0, "3 input and 5 output tokens"},
		{&protocoltypes.TaskUsage{TotalTokens: 8}, 0, "8 tokens"},
		{&protocoltypes.TaskUsage{Seconds: 1.5}, 0, "1.5 seconds of audio"},
		{&protocoltypes.TaskUsage{Characters: 12}, 2, "12 characters, 2 image(s)"},
	}
	for _, tt := range tests {
		if got := usageText(tt.u, make([]spawnllm.GeneratedImage, tt.images)); got != tt.want {
			t.Errorf("usageText(%+v, %d) = %q, want %q", tt.u, tt.images, got, tt.want)
		}
	}
}

// Speech takes no input files.
func TestTaskModelHost_SpeechRefusesFiles(t *testing.T) {
	e := newTaskHostEnv(t)
	e.write(t, "files/note.txt", []byte("hello"))
	if got := e.check(t, "alice", toolsagents.TaskModelRequest{Kind: "tts", Prompt: "x", Files: []string{"files/note.txt"}}); got != "Speech takes no files: give the text to speak as task." {
		t.Errorf("tts with a file = %q", got)
	}
}

// A loop with no configuration, registry or media store refuses the call
// instead of failing on a nil.
func TestTaskModelHost_LoopNotReady(t *testing.T) {
	h := &taskModelHost{al: &AgentLoop{}}
	if err := h.Check(context.Background(), "alice", t.TempDir(), toolsagents.TaskModelRequest{Kind: "text", Prompt: "x"}); !errors.Is(err, errLoopNotReady) {
		t.Errorf("Check on an unready loop = %v", err)
	}
	if _, _, err := h.isolatedFolder(&config.Config{}, "alice"); !errors.Is(err, errLoopNotReady) {
		t.Errorf("isolatedFolder on an unready loop = %v", err)
	}
}

// Media inputs: no media store, a stored file without a name (its base name
// is used), and one over max_media_size.
func TestTaskModelHost_MediaInputEdges(t *testing.T) {
	e := newTaskHostEnv(t)
	ref := e.media(t, "clip.ogg", "", []byte("OggS fake"))

	e.al.mediaStore = nil
	if got := e.check(t, "alice", toolsagents.TaskModelRequest{Kind: "stt", Media: []string{ref}}); got != "Can't use "+ref+": attachments are not available." {
		t.Errorf("no media store = %q", got)
	}

	store := media.NewFileMediaStore()
	e.al.SetMediaStore(store)
	p := filepath.Join(t.TempDir(), "voice.ogg")
	if err := os.WriteFile(p, []byte("OggS fake"), 0o600); err != nil {
		t.Fatal(err)
	}
	unnamed, err := store.Store(p, media.MediaMeta{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.run(t, "alice", toolsagents.TaskModelRequest{Kind: "stt", Model: "whisper", Media: []string{unnamed}})
	if err != nil || res.Text != "hello from the audio" {
		t.Fatalf("unnamed media stt: %+v, %v", res, err)
	}
	if body := e.vendor.lastBody(); !strings.Contains(body, `filename="voice.ogg"`) {
		t.Errorf("upload does not name the stored file: %q", body)
	}

	e.cfg.Agents.Defaults.MaxMediaSize = 4
	if got := e.check(t, "alice", toolsagents.TaskModelRequest{Kind: "stt", Media: []string{unnamed}}); got != "Can't use "+unnamed+": it is 9 bytes, more than the limit of 4 bytes." {
		t.Errorf("too large media = %q", got)
	}
}

// A model in a list that the Models page does not have, or whose provider is
// gone, is reported among the attempts with the reason.
func TestTaskModelHost_UnconfiguredModelsNoted(t *testing.T) {
	e := newTaskHostEnv(t)
	e.cfg.Models = append(e.cfg.Models, modelspec.Model{ModelName: "orphan", Model: "orphan-wire", Provider: "nobody", Enabled: true})
	e.cfg.TaskModels.TextModels = []config.TaskModelRef{{Model: "missing"}, {Model: "orphan"}, {Model: "chat-a"}}
	res, err := e.run(t, "alice", toolsagents.TaskModelRequest{Kind: "text", Prompt: "hi"})
	if err != nil || res.Model != "chat-a" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	want := []toolsagents.TaskAttempt{
		{Model: "missing", Error: "not configured or disabled on the Models page"},
		{Model: "orphan", Error: "its provider is not configured"},
	}
	if len(res.Attempts) != len(want) || res.Attempts[0] != want[0] || res.Attempts[1] != want[1] {
		t.Errorf("attempts = %+v, want %+v", res.Attempts, want)
	}
}

// A call whose context ends returns the context's error, not the generic
// one.
func TestTaskModelHost_CanceledCall(t *testing.T) {
	e := newTaskHostEnv(t)
	if d := catalogDigest(e.cfg); d != e.al.catalogDigest {
		e.al.adoptCatalog(e.al.catalogs.Build(e.cfg), d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.host.Run(ctx, "alice", e.workspace, toolsagents.TaskModelRequest{Kind: "text", Prompt: "hi"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run on a canceled context = %v, want context.Canceled", err)
	}
}

// Model failures that move on to the next model: a speech-only provider for
// text, an empty text reply, a provider with no speech API, and empty audio.
func TestTaskModelHost_ModelFailuresNoted(t *testing.T) {
	e := newTaskHostEnv(t)
	// A server in front of the vendor answering the speech of "silent" with
	// no audio.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/audio/speech") {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if strings.Contains(string(body), "silent") {
				w.Header().Set("Content-Type", "audio/mpeg")
				return
			}
			r.Body = io.NopCloser(strings.NewReader(string(body)))
		}
		e.vendor.handle(w, r)
	}))
	t.Cleanup(srv.Close)
	e.cfg.Providers[0].BaseURL = srv.URL + "/v1"
	e.host.httpClient = srv.Client()

	e.cfg.TaskModels.TextModels = []config.TaskModelRef{{Model: "eleven"}}
	res, err := e.run(t, "alice", toolsagents.TaskModelRequest{Kind: "text", Prompt: "hi"})
	if err == nil || len(res.Attempts) != 1 || res.Attempts[0].Error != "provider e serves speech only" {
		t.Errorf("speech-only provider for text: %+v, %v", res, err)
	}

	e.cfg.TaskModels.TextModels = []config.TaskModelRef{{Model: "chat-a"}}
	e.vendor.chatText = "  "
	res, err = e.run(t, "alice", toolsagents.TaskModelRequest{Kind: "text", Prompt: "hi"})
	if err == nil || len(res.Attempts) != 1 || res.Attempts[0].Error != "the model returned no text" {
		t.Errorf("empty text: %+v, %v", res, err)
	}

	e.cfg.TaskModels.TTSModels = append(e.cfg.TaskModels.TTSModels, config.TTSTaskModel{Model: "claude"})
	e.cfg.TaskModels.STTModels = append(e.cfg.TaskModels.STTModels, config.TaskModelRef{Model: "claude"})
	res, err = e.run(t, "alice", toolsagents.TaskModelRequest{Kind: "tts", Model: "claude", Prompt: "hi"})
	if err == nil || len(res.Attempts) != 1 || !strings.Contains(res.Attempts[0].Error, "does not serve speech") {
		t.Errorf("tts on a provider with no speech API: %+v, %v", res, err)
	}
	e.write(t, "files/clip.ogg", []byte("OggS fake"))
	res2, err2 := e.run(t, "alice", toolsagents.TaskModelRequest{Kind: "stt", Model: "claude", Files: []string{"files/clip.ogg"}})
	if err2 == nil || len(res2.Attempts) != 1 || res2.Attempts[0].Error != res.Attempts[0].Error {
		t.Errorf("stt on a provider with no speech API: %+v, %v (tts said %q)", res2, err2, res.Attempts[0].Error)
	}

	res, err = e.run(t, "alice", toolsagents.TaskModelRequest{Kind: "tts", Model: "tts-1", Prompt: "silent"})
	if err == nil || len(res.Attempts) != 1 || res.Attempts[0].Error != "the model returned no audio" {
		t.Errorf("empty audio: %+v, %v", res, err)
	}
}

// save: speech whose type the API did not declare takes its extension from
// the format asked for; an image given only as a URL is reported as one (a
// data: URL is not), and a failed write is returned.
func TestTaskModelHost_SaveShapes(t *testing.T) {
	e := newTaskHostEnv(t)
	res := &toolsagents.TaskModelResult{}
	out := &callOutput{audio: []byte("RIFF"), audioType: "application/octet-stream", format: "wav"}
	if err := e.host.save(e.cfg, e.workspace, toolsagents.TaskKindTTS, out, res); err != nil || len(res.Paths) != 1 || !strings.HasSuffix(res.Paths[0], ".wav") {
		t.Fatalf("speech by format: %+v, %v", res, err)
	}

	res = &toolsagents.TaskModelResult{}
	out = &callOutput{images: []spawnllm.GeneratedImage{
		{URL: "https://img.example/cat.png"},
		{URL: "DATA:image/png;base64,AAAA"},
	}}
	if err := e.host.save(e.cfg, e.workspace, toolsagents.TaskKindImage, out, res); err != nil {
		t.Fatal(err)
	}
	if len(res.Paths) != 0 || len(res.URLs) != 1 || res.URLs[0] != "https://img.example/cat.png" {
		t.Errorf("URL images: paths %v, urls %v", res.Paths, res.URLs)
	}

	gen := filepath.Join(e.workspace, "files", "generated")
	if err := os.RemoveAll(gen); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gen, []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind string
		out  *callOutput
	}{
		{toolsagents.TaskKindTTS, &callOutput{audio: []byte("ID3"), audioType: "audio/mpeg"}},
		{toolsagents.TaskKindImage, &callOutput{images: []spawnllm.GeneratedImage{{Data: pngBytes, ContentType: "image/png"}}}},
		{toolsagents.TaskKindImage, &callOutput{text: "words only"}},
	} {
		if err := e.host.save(e.cfg, e.workspace, tc.kind, tc.out, &toolsagents.TaskModelResult{}); err == nil {
			t.Errorf("save %s with files/generated a file: no error", tc.kind)
		}
	}
}

// isolatedFolder refuses when there is no temporary root, and when the root
// can't be made.
func TestTaskModelHost_IsolatedFolderErrors(t *testing.T) {
	al := mustNewAgentLoop(t, newTestConfig(t), bus.NewMessageBus(), &mockProvider{}, realClients{}) // not the data directory's owner
	h := &taskModelHost{al: al}
	if _, _, err := h.isolatedFolder(al.GetConfig(), "alice"); err == nil || err.Error() != "no folder for an isolated CLI call in this process" {
		t.Errorf("no root: %v", err)
	}

	e := newTaskHostEnv(t)
	root := e.al.GetRegistry().TempRoot()
	if root == "" {
		t.Fatal("the owner loop has no temporary root")
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.host.isolatedFolder(e.cfg, "alice"); err == nil || !strings.HasPrefix(err.Error(), "folder for an isolated CLI call: ") {
		t.Errorf("root is a file: %v", err)
	}
}

// Bytes detected as WAVE are audio/wav, as the transcription APIs name it.
func TestContentTypeOf_WaveBytes(t *testing.T) {
	wav := wavFromPCM([]byte{1, 0, 2, 0}, 16000)
	if got := contentTypeOf("clip", wav); got != "audio/wav" {
		t.Errorf("contentTypeOf(WAV bytes) = %q, want audio/wav", got)
	}
}

// A model's own failure, or a reply without the image asked for, moves on
// and is reported among the attempts.
func TestTaskModelHost_CallFailuresNoted(t *testing.T) {
	e := newTaskHostEnv(t)
	check := func(what string, req toolsagents.TaskModelRequest, wantErr string) {
		t.Helper()
		res, err := e.run(t, "alice", req)
		if err == nil || len(res.Attempts) != 1 || !strings.Contains(res.Attempts[0].Error, wantErr) {
			t.Errorf("%s: %+v, %v; want one attempt failing with %q", what, res, err, wantErr)
		}
	}
	check("chat image with no image", toolsagents.TaskModelRequest{Kind: "image", Model: "chat-a", Prompt: "a dog"}, "the model returned no image")

	e.vendor.fail["tts-1-wire"] = http.StatusBadRequest
	check("speech refused", toolsagents.TaskModelRequest{Kind: "tts", Model: "tts-1", Prompt: "hi"}, "refused")
	e.vendor.fail["whisper-1"] = http.StatusBadRequest
	e.write(t, "files/clip.ogg", []byte("OggS fake"))
	check("transcription refused", toolsagents.TaskModelRequest{Kind: "stt", Model: "whisper", Files: []string{"files/clip.ogg"}}, "refused")
}

// cliTaskModel adds the CLI text model "cc", whose command prints out (a
// claude -p JSON result), as the only Text model.
func cliTaskModel(t *testing.T, e *taskHostEnv, out string) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "claude")
	body := "#!/bin/sh\ncat > /dev/null\nprintf '%s' '" + out + "'\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	e.cfg.Providers = append(e.cfg.Providers, modelspec.Provider{Name: "Claude CLI", Protocol: "claude-cli", Command: script})
	e.cfg.Models = append(e.cfg.Models, modelspec.Model{ModelName: "cc", Model: "claude-cli", Provider: "Claude CLI", Enabled: true})
	e.cfg.TaskModels.TextModels = []config.TaskModelRef{{Model: "cc"}}
	if err := os.MkdirAll(e.cfg.CLIPath(), 0o700); err != nil {
		t.Fatal(err)
	}
}

// A call's reported cost is in the result.
func TestTaskModelHost_Cost(t *testing.T) {
	e := newTaskHostEnv(t)
	cliTaskModel(t, e, `{"type":"result","subtype":"success","is_error":false,"result":"cli reply","total_cost_usd":0.5}`)
	res, err := e.run(t, "alice", toolsagents.TaskModelRequest{Kind: "text", Prompt: "hi"})
	if err != nil || res.Text != "cli reply" || res.CostUSD == nil || *res.CostUSD != 0.5 {
		t.Fatalf("result = %+v, %v; want the cost", res, err)
	}
}

// A CLI text model whose isolated folder can't be made fails that model.
func TestTaskModelHost_CLIFolderError(t *testing.T) {
	e := newTaskHostEnv(t)
	cliTaskModel(t, e, `{"type":"result","subtype":"success","is_error":false,"result":"cli reply"}`)
	root := e.al.GetRegistry().TempRoot()
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := e.run(t, "alice", toolsagents.TaskModelRequest{Kind: "text", Prompt: "hi"})
	if err == nil || len(res.Attempts) != 1 || !strings.HasPrefix(res.Attempts[0].Error, "folder for an isolated CLI call: ") {
		t.Errorf("result = %+v, %v", res, err)
	}
}
