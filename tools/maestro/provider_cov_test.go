package maestro

import (
	"bytes"
	"strings"
	"testing"

	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/logger"
)

func TestGlobalProvider_Metadata(t *testing.T) {
	if GlobalProvider.Namespace() != "maestro" || GlobalProvider.Suite() != "maestro" ||
		GlobalProvider.Description() != "Maestro task orchestration: projects, playbooks, tasks, and reports" {
		t.Errorf("metadata: %q %q", GlobalProvider.Namespace(), GlobalProvider.Description())
	}
	if ok, why := GlobalProvider.Available(nil); !ok || why != "" {
		t.Errorf("Available = %v %q", ok, why)
	}
	if defs := GlobalProvider.RegisterTools(toolspec.Deps{}); defs != nil {
		t.Errorf("enumeration pass: %d tools", len(defs))
	}
	if !isNilRunner(nil) {
		t.Error("a nil runner is nil")
	}
}

// Blank lines are dropped; a DEBUG line reaches the central log at debug.
func TestLogWriter_BlankAndDebugLines(t *testing.T) {
	var got []string
	w := &logWriter{agent: "alice", emit: func(level, msg string, _ map[string]any) { got = append(got, level+"|"+msg) }}
	if _, err := w.Write([]byte("\n\r\n2026-09-19 10:00:00 [DEBUG] [1] detail\n")); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "DEBUG|detail" {
		t.Errorf("forwarded = %v", got)
	}

	var buf bytes.Buffer
	restore := logger.RedirectForTest(&buf)
	defer restore()
	prev := logger.GetLevel()
	logger.SetLevel(logger.DEBUG)
	defer logger.SetLevel(prev)
	lw := &logWriter{agent: "alice"}
	if _, err := lw.Write([]byte("2026-09-19 10:00:00 [DEBUG] [1] fine detail\n")); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "fine detail") || !strings.Contains(out, `"level":"debug"`) {
		t.Errorf("central log:\n%s", out)
	}
}
