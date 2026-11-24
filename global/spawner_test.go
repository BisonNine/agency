// Agency
// License: MIT

package global

import "testing"

func TestSpawnModeString(t *testing.T) {
	tests := []struct {
		mode SpawnMode
		want string
	}{
		{SpawnCallback, "callback"},
		{SpawnAndWait, "wait"},
		{SpawnMode(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.mode.String(); got != tt.want {
			t.Errorf("SpawnMode(%d).String() = %q, want %q", tt.mode, got, tt.want)
		}
	}
}

func TestTurnUsageAdd(t *testing.T) {
	var u TurnUsage
	u.Add("alpha", "anthropic", 10, 5, 2, 1, 0.5)
	u.Add("", "", 3, 4, 1, 0, 0.25)
	want := TurnUsage{
		Model: "alpha", Provider: "anthropic",
		InputTokens: 13, OutputTokens: 9, CacheReadTokens: 3, CacheCreationTokens: 1,
		CostUSD: 0.75,
	}
	if u != want {
		t.Fatalf("after two Adds = %+v, want %+v", u, want)
	}

	// The most recent non-empty model and provider win.
	u.Add("beta", "openai", 0, 0, 0, 0, 0)
	if u.Model != "beta" || u.Provider != "openai" {
		t.Errorf("model/provider = %q/%q, want beta/openai", u.Model, u.Provider)
	}
}

func TestTurnUsageAddNil(t *testing.T) {
	var u *TurnUsage
	u.Add("alpha", "anthropic", 1, 1, 1, 1, 1) // must not panic
}
