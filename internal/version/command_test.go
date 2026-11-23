package version

import (
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.bison9.com/agency/app"
)

func TestNewVersionCommand(t *testing.T) {
	cmd := NewVersionCommand()

	require.NotNil(t, cmd)

	assert.Equal(t, "version", cmd.Use)

	assert.Len(t, cmd.Aliases, 1)
	assert.True(t, cmd.HasAlias("v"))

	assert.False(t, cmd.HasFlags())

	assert.Equal(t, "Show version information", cmd.Short)

	assert.False(t, cmd.HasSubCommands())

	assert.NotNil(t, cmd.Run)
	assert.Nil(t, cmd.RunE)

	assert.Nil(t, cmd.PersistentPreRun)
	assert.Nil(t, cmd.PersistentPostRun)
}

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	done := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(r)
		done <- b
	}()
	defer func() { os.Stdout = orig }()
	fn()
	require.NoError(t, w.Close())
	return string(<-done)
}

func TestVersionOutput(t *testing.T) {
	tests := []struct {
		name  string
		build string
		goVer string
		want  string
	}{
		{"stamped", "2026-01-02T03:04:05Z", "go1.99", "  Build: 2026-01-02T03:04:05Z\n  Go: go1.99\n"},
		{"unstamped", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saved := buildInfo
			buildInfo = func() (string, string) { return tt.build, tt.goVer }
			t.Cleanup(func() { buildInfo = saved })

			cmd := NewVersionCommand()
			cmd.SetArgs(nil)
			out := captureStdout(t, func() { require.NoError(t, cmd.Execute()) })
			assert.Equal(t, app.Name()+" "+app.Version()+"\n"+tt.want, out)
		})
	}
}
