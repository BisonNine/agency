// Agency
// License: MIT

package agent

import (
	"context"
	"testing"

	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/agentreg"
)

func TestImageExtension_GIFAndSVG(t *testing.T) {
	for ct, want := range map[string]string{"image/gif": ".gif", "IMAGE/SVG+XML": ".svg"} {
		if got := imageExtension(spawnllm.GeneratedImage{ContentType: ct}); got != want {
			t.Errorf("imageExtension(%q) = %q, want %q", ct, got, want)
		}
	}
}

func TestWithImageNotes(t *testing.T) {
	if got := withImageNotes("A cat.", nil); got != "A cat." {
		t.Errorf("no notes = %q", got)
	}
	if got := withImageNotes("", []string{"a", "b"}); got != "a\nb" {
		t.Errorf("no content = %q", got)
	}
	if got := withImageNotes("A cat.", []string{"a"}); got != "A cat.\n\na" {
		t.Errorf("content and notes = %q", got)
	}
}

// A clone with no name of its own names its source as the workspace owner.
func TestTurnImages_WorkspaceOwnerOfUnnamedClone(t *testing.T) {
	ti := &turnImages{agent: &AgentInstance{ID: "clone-1", Spec: agentreg.Spec{Origin: agentreg.OriginTemp, SourceID: "alice"}}}
	if !ti.agent.Spec.IsClone() {
		t.Fatal("the spec is not a clone's")
	}
	if got := ti.workspaceOwner(); got != "alice" {
		t.Errorf("workspaceOwner = %q, want the source's id", got)
	}
}

// A saved image that can't be opened for sending is reported with its path,
// and nothing is sent.
func TestTurnImages_DeliverUnreadable(t *testing.T) {
	l := newSendFilesLoop(t)
	ti := &turnImages{al: l.al, agent: l.agent, opts: processOptions{Channel: "telegram", ChatID: "42"}}
	outcome, r := ti.deliverImage(context.Background(), pngImage(), "files/generated/gone.png")
	want := "Image not sent: it could not be read. Image saved as files/generated/gone.png."
	if r.reply != want || r.note != want || outcome == "sent" {
		t.Errorf("deliverImage = %q, %+v", outcome, r)
	}
	expectNoDelivery(t, l)
}
