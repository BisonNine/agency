// Agency
// License: MIT

package app

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReleasePublicKeys checks both slots are returned in order and that each
// filled slot is a minisign Ed25519 public key: base64 of the "Ed" algorithm
// tag, an 8-byte key id and a 32-byte key, 56 characters starting "RW".
func TestReleasePublicKeys(t *testing.T) {
	keys := ReleasePublicKeys()
	require.Len(t, keys, 2)
	assert.Equal(t, []string{releaseKey1, releaseKey2}, keys)
	assert.NotEqual(t, keys[0], keys[1], "the two slots must hold different keys")

	filled := 0
	for i, k := range keys {
		if k == "" {
			continue
		}
		filled++
		assert.Len(t, k, 56, "slot %d", i+1)
		assert.Equal(t, "RW", k[:2], "slot %d", i+1)
		raw, err := base64.StdEncoding.DecodeString(k)
		require.NoError(t, err, "slot %d", i+1)
		require.Len(t, raw, 42, "slot %d", i+1)
		assert.Equal(t, "Ed", string(raw[:2]), "slot %d", i+1)
	}
	assert.Positive(t, filled, "with both slots empty upgrading fails closed")

	keys[0] = "changed"
	assert.Equal(t, releaseKey1, ReleasePublicKeys()[0], "callers must not alter the keys")
}
