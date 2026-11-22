// ClawEh
// License: MIT

package maestro

import (
	"context"
	"errors"
	"testing"
)

// A Maestro run outlives the turn that started it: Maestro runs its tasks on
// a context detached from the tool call's cancellation (keeping its values),
// so cancelling the calling turn, as /cancel does, does not reach a worker
// it dispatched. The worker finishes and the task is done. /cancel and
// /cancel all leave Maestro runs alone, like forum runs.
func TestIntegration_RunOutlivesCancelledTurn(t *testing.T) {
	h := newMaestroHarness(t)
	h.runner.started = make(chan struct{}, 1)
	h.runner.hold = make(chan struct{})

	const project = "cancel"
	h.call("maestro_project_create", map[string]any{"name": project, "title": "Cancel", "disclaimer_template": "none"}, false)
	h.createTaskSet(project, "main")
	h.callJSON("maestro_task_create", map[string]any{"project": project, "path": "main", "title": "Worker", "type": "test", "prompt": "Do the work."})

	turnCtx, cancelTurn := context.WithCancelCause(context.Background())
	h.callCtx(turnCtx, "maestro_task_run", map[string]any{"project": project, "path": "main"})
	<-h.runner.started
	cancelTurn(errors.New("interrupted by user"))
	close(h.runner.hold)

	task := h.waitTerminal(project, "main")
	work, ok := task["work"].(map[string]any)
	if !ok || work["status"] != "done" {
		t.Fatalf("task = %+v, want done", task)
	}
	h.runner.mu.Lock()
	defer h.runner.mu.Unlock()
	if len(h.runner.ctxErrs) != 1 || h.runner.ctxErrs[0] != nil {
		t.Fatalf("dispatch context errors = %v, want one live context", h.runner.ctxErrs)
	}
}
