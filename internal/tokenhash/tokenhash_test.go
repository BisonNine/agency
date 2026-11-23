package tokenhash

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestHash(t *testing.T) {
	sum := sha256.Sum256([]byte("SSTalice"))
	want := "sha256:" + hex.EncodeToString(sum[:])
	if got := Hash("SSTalice"); got != want {
		t.Errorf("Hash = %q, want %q", got, want)
	}
	if Hash("SSTalice") == Hash("SSTbob") {
		t.Error("different tokens hash alike")
	}
}

func TestIsHashed(t *testing.T) {
	for _, tc := range []struct {
		stored string
		want   bool
	}{
		{Hash("SSTalice"), true},
		{"SSTalice", false},
		{"0123abcd", false},
		{"", false},
	} {
		if got := IsHashed(tc.stored); got != tc.want {
			t.Errorf("IsHashed(%q) = %v, want %v", tc.stored, got, tc.want)
		}
	}
}
