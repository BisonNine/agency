// Agency
// License: MIT

package config

import (
	"encoding/json"
	"testing"
)

// The forum suite is off unless the agent's `forum` switch is on.
func TestConfig_AgentSuiteEnabledForum(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"agents":{"list":[{"id":"alice","forum":true},{"id":"bob"}]}}`), &c); err != nil {
		t.Fatal(err)
	}
	if !c.AgentSuiteEnabled("alice", "forum") {
		t.Error("forum is off for alice, whose switch is on")
	}
	if c.AgentSuiteEnabled("bob", "forum") {
		t.Error("forum is on for bob, whose switch is absent (default off)")
	}
	if c.AgentSuiteEnabled("nobody", "forum") {
		t.Error("forum is on for an unknown agent")
	}
	out, err := json.Marshal(c.Agents.List[1])
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"id":"bob"}` {
		t.Errorf("an agent with forum off is saved as %s, want no forum key", out)
	}
}

// Every configured agent has the billing suite; an unknown agent has none.
func TestConfig_AgentSuiteEnabledBilling(t *testing.T) {
	c := &Config{}
	c.Agents.List = []AgentConfig{{ID: "alice"}}
	if !c.AgentSuiteEnabled("Alice", BillingSuite) {
		t.Error("alice has no billing tool")
	}
	if c.AgentSuiteEnabled("bob", BillingSuite) {
		t.Error("an unknown agent has the billing tool")
	}
}
