package pidfile

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
}

func TestWriteErrors(t *testing.T) {
	assert.EqualError(t, Write(""), "pidfile: no data directory")

	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	err := Write(file)
	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), "pidfile: create "), err.Error())
}

func TestWriteUnwritableDirectory(t *testing.T) {
	skipIfRoot(t)
	dir := t.TempDir()
	internal := filepath.Dir(Path(dir))
	require.NoError(t, os.MkdirAll(internal, 0o500))
	t.Cleanup(func() {
		if err := os.Chmod(internal, 0o700); err != nil {
			t.Error(err)
		}
	})
	err := Write(dir)
	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), "pidfile: write "+Path(dir)+": "), err.Error())
}

func TestRemoveEdgeCases(t *testing.T) {
	Remove("") // nothing to do

	skipIfRoot(t)
	dir := t.TempDir()
	require.NoError(t, Write(dir))
	internal := filepath.Dir(Path(dir))
	require.NoError(t, os.Chmod(internal, 0o500))
	t.Cleanup(func() {
		if err := os.Chmod(internal, 0o700); err != nil {
			t.Error(err)
		}
	})
	Remove(dir) // logged, not fatal
	assert.FileExists(t, Path(dir))
}

// fakeStatus points procRoot at a tree holding status for pid.
func fakeStatus(t *testing.T, pid int, status string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, strconv.Itoa(pid))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o600))
	saved := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = saved })
}

func TestRSSBytesParsing(t *testing.T) {
	const pid = 4242
	tests := []struct {
		name   string
		status string
		want   int64
		ok     bool
	}{
		{"resident set", "Name:\tclaw\nVmSize:\t900000 kB\nVmRSS:\t  2048 kB\n", 2048 * 1024, true},
		{"no value", "VmRSS:\n", 0, false},
		{"not a number", "VmRSS:\tlots kB\n", 0, false},
		{"no VmRSS line", "Name:\tclaw\n", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeStatus(t, pid, tt.status)
			got, ok := RSSBytes(pid)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}
