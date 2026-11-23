// ClawEh
// License: MIT

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
)

// The named-token quota, block reset and rate update reach the agent's
// tokens: a token used past its rate is blocked, shows so in the quota, and
// is usable again once its block is reset.
func TestMessageTokens_QuotaResetAndUpdate(t *testing.T) {
	tl := newTestAgentLoop(t)
	al := tl.al
	tok, err := al.CreateMessageToken("main", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !al.UpdateMessageToken("main", tok.ID, 1, 5) {
		t.Fatal("UpdateMessageToken did not find the token")
	}
	if al.UpdateMessageToken("main", "missing", 1, 5) {
		t.Fatal("UpdateMessageToken found a token that does not exist")
	}

	if _, _, d := al.CheckMessageToken(tok.Token); d != MsgTokenValid {
		t.Fatalf("first use = %v, want valid", d)
	}
	if _, ra, d := al.CheckMessageToken(tok.Token); d != MsgTokenRateLimited || ra <= 0 {
		t.Fatalf("second use = %v (retry %v), want rate limited", d, ra)
	}
	q := al.MessageTokenQuota("main")
	if len(q) != 1 || q[0].ID != tok.ID || q[0].RatePerMin != 1 || q[0].BlockMinutes != 5 || !q[0].Blocked {
		t.Fatalf("quota = %+v, want alice blocked at 1/min for 5 min", q)
	}
	if n := al.ResetMessageTokenBlocks("main", "alice"); n != 1 {
		t.Fatalf("ResetMessageTokenBlocks = %d, want 1", n)
	}
	if q := al.MessageTokenQuota("main"); q[0].Blocked {
		t.Fatalf("quota after reset = %+v, want unblocked", q)
	}
}

// Without a named-token store every named-token call reports nothing and
// creation fails.
func TestMessageTokens_NoNamedStore(t *testing.T) {
	tl := newTestAgentLoop(t)
	al := tl.al
	al.mu.Lock()
	al.namedTokens = nil
	al.mu.Unlock()

	if got := al.ListMessageTokens("main"); got != nil {
		t.Errorf("ListMessageTokens = %v, want nil", got)
	}
	if _, err := al.CreateMessageToken("main", "alice"); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("CreateMessageToken = %v, want the store unavailable", err)
	}
	if al.DeleteMessageToken("main", "x") {
		t.Error("DeleteMessageToken reported a removal")
	}
	if got := al.MessageTokenQuota("main"); got != nil {
		t.Errorf("MessageTokenQuota = %v, want nil", got)
	}
	if n := al.ResetMessageTokenBlocks("main", ""); n != 0 {
		t.Errorf("ResetMessageTokenBlocks = %d, want 0", n)
	}
	if al.UpdateMessageToken("main", "x", 1, 1) {
		t.Error("UpdateMessageToken reported a token")
	}
	if _, _, d := al.CheckMessageToken("anything"); d != MsgTokenInvalid {
		t.Errorf("CheckMessageToken = %v, want invalid", d)
	}
}

// An agent whose token store cannot be written gets no message-token
// manager (its endpoint stays closed) and the failure is logged.
func TestMessageTokens_BuildMessageManagersStoreUnwritable(t *testing.T) {
	buf := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(buf))
	tl := newTestAgentLoop(t)
	cfg := *tl.cfg
	cfg.Agents.List = []config.AgentConfig{{ID: "main", Name: "Main", Default: true, Message: &config.MessageConfig{WindowMinutes: 60, WindowCount: 2}}}
	dir := filepath.Join(mustGetAgent(t, tl.al).Workspace, "state")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Errorf("restore %s: %v", dir, err)
		}
	})

	managers := buildMessageManagers(tl.al.GetRegistry(), &cfg)

	if _, ok := managers["main"]; ok {
		t.Fatal("a manager was built on an unwritable store")
	}
	if !strings.Contains(buf.String(), "Failed to initialize callback manager") {
		t.Fatalf("no warning logged:\n%s", buf.String())
	}
}
