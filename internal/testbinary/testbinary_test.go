package testbinary

import (
	"os"
	"slices"
	"testing"

	"go.bison9.com/agency/app"
)

func TestSet(t *testing.T) {
	orig := slices.Clone(os.Args)
	t.Run("set", func(t *testing.T) {
		Set(t, "alice-agency")
		if got := app.BinaryName(); got != "alice-agency" {
			t.Errorf("BinaryName = %q, want alice-agency", got)
		}
		if !slices.Equal(os.Args[1:], orig[1:]) {
			t.Errorf("arguments changed: %v", os.Args)
		}
	})
	if !slices.Equal(os.Args, orig) {
		t.Errorf("os.Args not restored: %v", os.Args)
	}
}
