// ClawEh
// License: MIT

package ccv3

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// registered reports whether Open cached a store for dir.
func registered(dir string) bool {
	storesMu.Lock()
	defer storesMu.Unlock()
	_, ok := stores[dir]
	return ok
}

// A path that is a regular file, or under one, is not a card store: Open
// fails and caches nothing, so a later Open with a fixed path is not handed
// the unusable store.
func TestOpenRefusesUnusablePath(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cards")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"regular file": file, "under a file": filepath.Join(file, "sub")} {
		t.Run(name, func(t *testing.T) {
			if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "open card store "+dir) {
				t.Fatalf("Open(%s) = %v, want an open card store error", dir, err)
			}
			if registered(dir) {
				t.Error("a failed Open registered the store")
			}
		})
	}
}
