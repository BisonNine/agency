package channels

import (
	"context"
	"slices"
	"testing"

	"go.bison9.com/agency/config"
)

// withFactoryInputs sets the inputs registered for name (none when nil) for
// the test, restoring the previous registration after it.
func withFactoryInputs(t *testing.T, name string, inputs FactoryInputs) {
	t.Helper()
	factoryInputsMu.Lock()
	prev, had := factoryInputs[name]
	delete(factoryInputs, name)
	factoryInputsMu.Unlock()
	if inputs != nil {
		RegisterFactoryInputs(name, inputs)
	}
	t.Cleanup(func() {
		factoryInputsMu.Lock()
		defer factoryInputsMu.Unlock()
		if had {
			factoryInputs[name] = prev
		} else {
			delete(factoryInputs, name)
		}
	})
}

// withoutTelegramFactory clears the Telegram factory for the test.
func withoutTelegramFactory(t *testing.T) {
	t.Helper()
	telegramBotFactoryMu.Lock()
	prev := telegramBotFactory
	telegramBotFactory = nil
	telegramBotFactoryMu.Unlock()
	t.Cleanup(func() {
		telegramBotFactoryMu.Lock()
		telegramBotFactory = prev
		telegramBotFactoryMu.Unlock()
	})
}

// withoutSecMsgFactory clears the SecMsg factory for the test.
func withoutSecMsgFactory(t *testing.T) {
	t.Helper()
	secmsgFactoryMu.Lock()
	prev := secmsgFactory
	secmsgFactory = nil
	secmsgFactoryMu.Unlock()
	t.Cleanup(func() {
		secmsgFactoryMu.Lock()
		secmsgFactory = prev
		secmsgFactoryMu.Unlock()
	})
}

func specNames(specs []channelSpec) []string {
	names := make([]string, len(specs))
	for i, s := range specs {
		names[i] = s.name
	}
	return names
}

func TestFingerprint_Unmarshalable(t *testing.T) {
	if got := fingerprint(make(chan int)); got != "" {
		t.Fatalf("fingerprint of a channel = %q, want \"\"", got)
	}
	if a, b := fingerprint("x"), fingerprint("x"); a == "" || a != b {
		t.Fatalf("fingerprint not stable: %q %q", a, b)
	}
}

// Two Telegram bots with the same channel name: the second is skipped.
func TestPlan_DuplicateNameSkipped(t *testing.T) {
	registerFakeTelegram(t)
	cfg := &config.Config{}
	cfg.Channels.Telegram = []config.TelegramBotConfig{
		{ID: "alice", Enabled: true, Token: "1:a", AllowFrom: config.FlexibleStringSlice{"*"}},
		{ID: "alice", Enabled: true, Token: "2:b", AllowFrom: config.FlexibleStringSlice{"*"}},
	}
	m := newTestManager()
	specs := m.plan(cfg)
	if got := specNames(specs); !slices.Equal(got, []string{"telegram-alice"}) {
		t.Fatalf("planned %v, want [telegram-alice]", got)
	}
	if specs[0].inputs != fingerprint(cfg.Channels.Telegram[0]) {
		t.Fatal("the first bot was not the one kept")
	}
}

func TestPlan_Matrix(t *testing.T) {
	matrix := func() *config.Config {
		cfg := &config.Config{}
		cfg.Channels.Matrix = config.MatrixConfig{
			Enabled: true, Homeserver: "https://matrix.example.org", UserID: "@bob:example.org", AccessToken: "t",
		}
		return cfg
	}
	t.Run("factory not registered", func(t *testing.T) {
		factoriesMu.Lock()
		prev, had := factories["matrix"]
		delete(factories, "matrix")
		factoriesMu.Unlock()
		t.Cleanup(func() {
			if had {
				factoriesMu.Lock()
				factories["matrix"] = prev
				factoriesMu.Unlock()
			}
		})
		if specs := newTestManager().plan(matrix()); len(specs) != 0 {
			t.Fatalf("planned %v without a matrix factory", specNames(specs))
		}
	})
	t.Run("registered", func(t *testing.T) {
		registerFakeFactory(t, "matrix")
		withFactoryInputs(t, "matrix", func(cfg *config.Config) any { return cfg.Channels.Matrix })
		specs := newTestManager().plan(matrix())
		if got := specNames(specs); !slices.Equal(got, []string{"matrix"}) {
			t.Fatalf("planned %v, want [matrix]", got)
		}
		s := specs[0]
		if s.platform != "matrix" || s.inputs == "" || !s.checkAllowFrom {
			t.Fatalf("spec %+v", s)
		}
	})
	t.Run("without inputs it is always rebuilt", func(t *testing.T) {
		registerFakeFactory(t, "matrix")
		withFactoryInputs(t, "matrix", nil)
		specs := newTestManager().plan(matrix())
		if len(specs) != 1 || specs[0].inputs != "" {
			t.Fatalf("planned %+v, want matrix with no inputs", specs)
		}
	})
}

func TestPlan_TelegramFactoryNotRegistered(t *testing.T) {
	withoutTelegramFactory(t)
	cfg := &config.Config{}
	cfg.Channels.Telegram = []config.TelegramBotConfig{
		{Enabled: true, Token: "1:a"},
		{ID: "bob", Enabled: true, Token: "2:b"},
	}
	if specs := newTestManager().plan(cfg); len(specs) != 0 {
		t.Fatalf("planned %v without a telegram factory", specNames(specs))
	}
}

func TestPlan_SecMsgFactoryNotRegistered(t *testing.T) {
	withoutSecMsgFactory(t)
	withDiscovery(t, func(context.Context, string) ([]string, error) {
		t.Error("discovery ran without a SecMsg factory")
		return nil, nil
	})
	cfg := &config.Config{}
	cfg.Channels.SecMsg = []config.SecMsgConfig{{Name: "signal", Enabled: true, Address: "127.0.0.1:9600"}}
	if specs := newTestManager().plan(cfg); len(specs) != 0 {
		t.Fatalf("planned %v without a SecMsg factory", specNames(specs))
	}
}

// When discovery fails, every running account of the daemon is kept, in name
// order.
func TestReload_SecMsgDiscoveryFailureKeepsEveryAccountSorted(t *testing.T) {
	accounts := []string{"bob", "alice"}
	registerFakeSecMsg(t, &accounts)
	cfg := &config.Config{}
	cfg.Channels.SecMsg = []config.SecMsgConfig{{Name: "signal", Enabled: true, Address: "127.0.0.1:9600", AllowFrom: config.FlexibleStringSlice{"*"}}}
	m, _ := startedManager(t, cfg, nil)

	accounts = nil // the daemon is down
	daemon := fingerprint(cfg.Channels.SecMsg[0])
	if got := specNames(m.runningSpecs(daemon)); !slices.Equal(got, []string{"signal-alice", "signal-bob"}) {
		t.Fatalf("running specs %v, want [signal-alice signal-bob]", got)
	}
	if res := m.Reload(context.Background(), cfg, nil); !slices.Equal(res.Kept, []string{"signal-alice", "signal-bob"}) {
		t.Fatalf("reload = %+v, want both accounts kept", res)
	}
}
