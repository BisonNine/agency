// ClawEh
// License: MIT

package agent

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/media"
)

// mediaEdgePNG is enough of a PNG for magic-byte detection.
var mediaEdgePNG = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0x0D, 0x49, 0x48, 0x44, 0x52}

// mediaEdgeFile writes data to name in a temp dir and stores it in store.
func mediaEdgeFile(t *testing.T, store *media.FileMediaStore, name string, data []byte, meta media.MediaMeta) (ref, path string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return mustStore(t, store, path, meta), path
}

// A ref the store does not know and a stored file that has since gone are
// dropped from the message; a ref that is not a media ref passes through.
func TestMedia_ResolveMediaRefsDropsUnresolvable(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	store := media.NewFileMediaStore()
	goneRef, gonePath := mediaEdgeFile(t, store, "gone.png", mediaEdgePNG, media.MediaMeta{})
	if err := os.Remove(gonePath); err != nil {
		t.Fatal(err)
	}
	okRef, _ := mediaEdgeFile(t, store, "ok.png", mediaEdgePNG, media.MediaMeta{})

	got := resolveMediaRefs([]spawnllm.Message{{
		Role: "user", Content: "look", Media: []string{"media://unknown", goneRef, "https://example.invalid/a.png", okRef},
	}}, store, config.DefaultMaxMediaSize)

	if len(got[0].Media) != 2 || got[0].Media[0] != "https://example.invalid/a.png" || !strings.HasPrefix(got[0].Media[1], "data:image/png;base64,") {
		t.Fatalf("media = %v, want the other ref kept and the readable image encoded", got[0].Media)
	}
	if got[0].Content != "look" {
		t.Fatalf("content = %q, want it unchanged", got[0].Content)
	}
}

// The vision side-model gets data URLs for images only: encoded data URIs
// pass through, media refs to images are encoded, and everything else
// (other refs, non-images, unknown or vanished refs, oversized images) is
// left out.
func TestMedia_ResolveImageDataURIs(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	tl := newTestAgentLoop(t)
	const inline = "data:image/png;base64,AAAA"

	// No store: only the inline image.
	if got := tl.al.resolveImageDataURIs([]string{inline, "media://x"}); !slices.Equal(got, []string{inline}) {
		t.Fatalf("no store: %v, want only the inline image", got)
	}

	store := media.NewFileMediaStore()
	tl.al.SetMediaStore(store)
	imgRef, _ := mediaEdgeFile(t, store, "a.png", mediaEdgePNG, media.MediaMeta{})
	txtRef, _ := mediaEdgeFile(t, store, "a.txt", []byte("hello"), media.MediaMeta{ContentType: "text/plain"})
	goneRef, gonePath := mediaEdgeFile(t, store, "b.png", mediaEdgePNG, media.MediaMeta{})
	if err := os.Remove(gonePath); err != nil {
		t.Fatal(err)
	}
	bigRef, _ := mediaEdgeFile(t, store, "big.png", append(slices.Clone(mediaEdgePNG), make([]byte, 64)...), media.MediaMeta{})
	tl.cfg.Agents.Defaults.MaxMediaSize = len(mediaEdgePNG) + 1

	got := tl.al.resolveImageDataURIs([]string{inline, "https://example.invalid/x.png", "media://unknown", imgRef, txtRef, goneRef, bigRef})

	want := []string{inline, "data:image/png;base64," + base64.StdEncoding.EncodeToString(mediaEdgePNG)}
	if !slices.Equal(got, want) {
		t.Fatalf("resolveImageDataURIs = %v, want %v", got, want)
	}
}

// An image that cannot be opened or read yields no data URL.
func TestMedia_EncodeImageToDataURLFailures(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	dir := t.TempDir()

	locked := filepath.Join(dir, "locked.png")
	if err := os.WriteFile(locked, mediaEdgePNG, 0o000); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(locked)
	if err != nil {
		t.Fatal(err)
	}
	if got := encodeImageToDataURL(locked, "image/png", info, config.DefaultMaxMediaSize); got != "" {
		t.Fatalf("unopenable file encoded: %q", got)
	}

	// A directory opens but cannot be read.
	sub := filepath.Join(dir, "dir.png")
	if mkErr := os.Mkdir(sub, 0o700); mkErr != nil {
		t.Fatal(mkErr)
	}
	info, err = os.Stat(sub)
	if err != nil {
		t.Fatal(err)
	}
	if got := encodeImageToDataURL(sub, "image/png", info, config.DefaultMaxMediaSize); got != "" {
		t.Fatalf("unreadable file encoded: %q", got)
	}
}
