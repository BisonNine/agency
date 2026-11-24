package internal

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withoutInstallRoot points the system install location at an empty temp
// dir, so a real /opt/agency on the test machine does not take over.
func withoutInstallRoot(t *testing.T) {
	t.Helper()
	saved := installRoot
	installRoot = t.TempDir()
	t.Cleanup(func() { installRoot = saved })
}

func TestGetConfigPath(t *testing.T) {
	withoutInstallRoot(t)
	// Empty, and restored after: GetAgencyHome may set it.
	t.Setenv("AGENCY_HOME", "")
	t.Setenv("HOME", "/tmp/home")

	got := GetConfigPath()
	want := filepath.Join("/tmp/home", ".agency", "config.json")

	assert.Equal(t, want, got)
}

func TestGetConfigPath_WithAgencyHome(t *testing.T) {
	withoutInstallRoot(t)
	t.Setenv("AGENCY_HOME", "/custom/agency")
	t.Setenv("HOME", "/tmp/home")

	got := GetConfigPath()
	want := filepath.Join("/custom/agency", "config.json")

	assert.Equal(t, want, got)
}

func TestGetConfigPath_Windows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-specific HOME behavior varies; run on windows")
	}

	testUserProfilePath := `C:\Users\Test`
	t.Setenv("USERPROFILE", testUserProfilePath)

	got := GetConfigPath()
	want := filepath.Join(testUserProfilePath, ".agency", "config.json")

	require.True(t, strings.EqualFold(got, want), "GetConfigPath() = %q, want %q", got, want)
}
