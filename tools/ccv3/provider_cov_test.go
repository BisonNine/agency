package ccv3

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/global"
)

func TestGlobalProvider_Metadata(t *testing.T) {
	if GlobalProvider.Namespace() != Suite || GlobalProvider.Description() != "Character cards: manage the character-card library" {
		t.Errorf("metadata: %q %q", GlobalProvider.Namespace(), GlobalProvider.Description())
	}
	if ok, why := GlobalProvider.Available(nil); !ok || why != "" {
		t.Errorf("Available = %v %q", ok, why)
	}
	// Asked directly for an agent without the switch, the provider builds
	// nothing.
	if defs := GlobalProvider.RegisterTools(toolspec.Deps{Cfg: testConfig(t), AgentID: "bob"}); len(defs) != 0 {
		t.Errorf("bob: %d tools", len(defs))
	}
}

func TestArgumentsThatCannotBeEncoded(t *testing.T) {
	cfg := testConfig(t)
	mustRefuse(t, run(t, cfg, map[string]any{"action": "create", "id": "x", "card": map[string]any{"name": math.Inf(1)}}), "The card argument cannot be encoded as JSON.")
	mustOK(t, run(t, cfg, map[string]any{"action": "create", "id": "x", "card": card("X")}))
	mustRefuse(t, run(t, cfg, map[string]any{"action": "update", "id": "x", "changes": map[string]any{"name": math.NaN()}}), "The changes argument cannot be encoded as JSON.")
	mustRefuse(t, run(t, cfg, map[string]any{"action": "list", "colour": "red", "size": 1}), "Unknown arguments colour, size; use action, id, new_id, card or changes.")
}

// I/O failures are reported in a fixed sentence, not as refusals.
func TestLibraryFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not apply to root")
	}
	ctx := context.Background()

	t.Run("cannot be opened", func(t *testing.T) {
		cfg := testConfig(t)
		home := os.Getenv(global.EnvVarHome)
		if err := os.Chmod(home, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
		res := build(t, cfg, "alice")[0].Execute(ctx, map[string]any{"action": "list"})
		if !res.IsError || res.ForLLM != "The card library could not be opened; try again later." {
			t.Errorf("open: %+v", res)
		}
	})

	t.Run("cannot be listed", func(t *testing.T) {
		cfg := testConfig(t)
		if err := os.WriteFile(cfg.CardsPath(), []byte("not a folder"), 0o600); err != nil {
			t.Fatal(err)
		}
		res := build(t, cfg, "alice")[0].Execute(ctx, map[string]any{"action": "list"})
		if !res.IsError || res.ForLLM != "The card library could not be listed; try again later." {
			t.Errorf("list: %+v", res)
		}
	})

	t.Run("read-only library", func(t *testing.T) {
		cfg := testConfig(t)
		mustOK(t, run(t, cfg, map[string]any{"action": "create", "id": "holmes", "card": card("Holmes")}))
		dir := cfg.CardsPath()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		for _, tc := range []struct {
			args map[string]any
			want string
		}{
			{map[string]any{"action": "rename", "id": "holmes", "new_id": "watson"}, `Card "holmes" could not be renamed; try again later.`},
			{map[string]any{"action": "delete", "id": "holmes"}, `Card "holmes" could not be deleted; try again later.`},
		} {
			res := run(t, cfg, tc.args)
			if !res.IsError || res.ForLLM != tc.want {
				t.Errorf("%v: %+v", tc.args["action"], res)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "holmes.json")); err != nil {
			t.Errorf("card lost: %v", err)
		}
	})
}
