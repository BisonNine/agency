// ClawEh
// License: MIT

package providers

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/config"
)

func TestHumanProvider_FromCatalogue(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "People", Protocol: config.HumanProtocol}},
		Models:    []config.ModelConfig{{ModelName: "Bob (human)", Model: "bob", Provider: "People", RequestTimeout: 90, Enabled: true}},
	}
	b := NewCatalogBuilder(nil, nil)
	human := func(c interface {
		Client(name string) (spawnllm.LLMProvider, func(), error)
	},
	) *HumanProvider {
		t.Helper()
		client, release, err := c.Client("Bob (human)")
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		hp, ok := client.(*HumanProvider)
		if !ok {
			t.Fatalf("the catalogue's client is %T, want a human client", client)
		}
		return hp
	}
	c := b.Build(cfg)
	hp := human(c)
	if hp.Timeout() != 90*time.Second || hp.GetDefaultModel() != "Bob (human)" {
		t.Fatalf("human client = %+v", hp)
	}
	// A second build hands the same client, so Adopt keeps it; a build for
	// a changed timeout makes a new one and leaves the first catalogue's
	// as it was.
	if human(b.Build(cfg)) != hp {
		t.Error("a rebuild made a new human client")
	}
	changed := *cfg
	changed.Models = []config.ModelConfig{{ModelName: "Bob (human)", Model: "bob", Provider: "People", RequestTimeout: 30, Enabled: true}}
	if other := human(b.Build(&changed)); other.Timeout() != 30*time.Second {
		t.Errorf("changed timeout = %s, want 30s", other.Timeout())
	}
	if human(c).Timeout() != 90*time.Second {
		t.Error("a later build changed the running catalogue's human client")
	}
}

// Outside a human agent's turn there is nobody to ask: the provider refuses.
func TestHumanProvider_RefusesWithoutRelay(t *testing.T) {
	_, err := NewHumanProvider("Bob (human)", 0).Chat(context.Background(), testIdentity, []spawnllm.Message{{Role: "user", Content: "hi"}}, nil, "", nil)
	if !errors.Is(err, ErrHumanUnreachable) {
		t.Fatalf("err = %v, want ErrHumanUnreachable", err)
	}
}

// The person sees only the latest user message, never the system prompt or
// the history, and their answer is the reply.
func TestHumanProvider_RelaysLatestUserMessage(t *testing.T) {
	var asked []string
	relay := func(_ context.Context, request string, timeout time.Duration) (string, error) {
		if timeout != time.Minute {
			t.Errorf("relay got timeout %s, want the provider's", timeout)
		}
		asked = append(asked, request)
		return "Looks good", nil
	}
	msgs := []spawnllm.Message{
		{Role: "system", Content: "You are Bob."},
		{Role: "user", Content: "old question"},
		{Role: "assistant", Content: "old answer"},
		{Role: "user", Content: "[From: Alice]\nPlease review the draft."},
	}
	resp, err := NewHumanProvider("Bob (human)", time.Minute).Chat(WithHumanRelay(context.Background(), relay), testIdentity, msgs, nil, "bob", nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(asked) != 1 || asked[0] != "[From: Alice]\nPlease review the draft." {
		t.Fatalf("relayed %q, want only the latest user message", asked)
	}
	if resp.Content != "Looks good" {
		t.Errorf("reply = %q, want the person's answer", resp.Content)
	}
}

// When the person does not answer within the timeout the reply is empty, not
// an error; the turn's own end is still an error.
func TestHumanProvider_Timeout(t *testing.T) {
	// The relay applies the timeout once the request is posted.
	wait := func(ctx context.Context, _ string, timeout time.Duration) (string, error) {
		waitCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		<-waitCtx.Done()
		return "", waitCtx.Err()
	}
	msgs := []spawnllm.Message{{Role: "user", Content: "hi"}}

	resp, err := NewHumanProvider("Bob (human)", 20*time.Millisecond).Chat(WithHumanRelay(context.Background(), wait), testIdentity, msgs, nil, "", nil)
	if err != nil || resp == nil || resp.Content != "" {
		t.Fatalf("timeout: resp %+v, err %v; want an empty reply", resp, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewHumanProvider("Bob (human)", time.Minute).Chat(WithHumanRelay(ctx, wait), testIdentity, msgs, nil, "", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled turn: err = %v, want context.Canceled", err)
	}
}
