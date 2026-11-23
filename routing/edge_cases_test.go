package routing

import (
	"strings"
	"testing"

	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/config"
)

func TestNormalizeAccountID_TrimsDashesAndLength(t *testing.T) {
	if got := NormalizeAccountID("--Bad Name!--"); got != "bad-name" {
		t.Errorf("got %q, want bad-name", got)
	}
	long := strings.Repeat("a", MaxAgentIDLength+10) + " x"
	if got := NormalizeAccountID(long); len(got) != MaxAgentIDLength {
		t.Errorf("length = %d, want %d", len(got), MaxAgentIDLength)
	}
}

// humanConfig makes bob a human agent (his model is on a human provider).
func humanConfig(agents []config.AgentConfig, bindings []config.AgentBinding) *config.Config {
	cfg := testConfig(agents, bindings)
	cfg.Providers = []modelspec.Provider{{Name: "people", Protocol: config.HumanProtocol}}
	cfg.Models = []modelspec.Model{{ModelName: "bob-person", Provider: "people"}}
	return cfg
}

// A human agent is never the default, even when marked so or listed first.
func TestResolveDefaultAgentID_SkipsHumans(t *testing.T) {
	agents := []config.AgentConfig{
		{ID: "bob", Default: true, Models: []string{"bob-person"}},
		{ID: "alice"},
	}
	r := NewRouteResolver(humanConfig(agents, nil))
	if got := r.ResolveRoute(RouteInput{Channel: "telegram"}).AgentID; got != "alice" {
		t.Errorf("default = %q, want alice", got)
	}

	// Only humans, or a first agent with no id: the built-in default.
	r = NewRouteResolver(humanConfig([]config.AgentConfig{{ID: "bob", Models: []string{"bob-person"}}}, nil))
	if got := r.ResolveRoute(RouteInput{Channel: "telegram"}).AgentID; got != DefaultAgentID {
		t.Errorf("humans only: default = %q, want %q", got, DefaultAgentID)
	}
	r = NewRouteResolver(testConfig([]config.AgentConfig{{ID: ""}, {ID: "alice"}}, nil))
	if got := r.ResolveRoute(RouteInput{Channel: "telegram"}).AgentID; got != DefaultAgentID {
		t.Errorf("empty first id: default = %q, want %q", got, DefaultAgentID)
	}
}

func TestResolveRoute_MentionsAndEdges(t *testing.T) {
	agents := []config.AgentConfig{{ID: "main", Default: true}, {ID: "alice"}, {ID: "bob"}}
	bindings := []config.AgentBinding{
		// Ignored: no channel, or a peer with no kind/id.
		{AgentID: "bob", Match: config.BindingMatch{AccountID: "*"}},
		{AgentID: "bob", Match: config.BindingMatch{Channel: "slack", AccountID: "*", Peer: &config.PeerMatch{Kind: "direct"}}},
		{AgentID: "bob", Match: config.BindingMatch{Channel: "slack", AccountID: "*", Peer: &config.PeerMatch{ID: "x"}}},
		{AgentID: "main", AgentMentions: []string{"*"}, Match: config.BindingMatch{Channel: "slack", AccountID: "*", TeamID: "T1"}},
		{AgentID: "main", Match: config.BindingMatch{Channel: "slack", AccountID: "*", TeamID: "T2"}},
	}
	r := NewRouteResolver(testConfig(agents, bindings))

	// A "*" mention list routes to whichever agent is mentioned.
	got := r.ResolveRoute(RouteInput{Channel: "slack", TeamID: "T1", MentionedAgent: "alice"})
	if got.AgentID != "alice" || got.MatchedBy != "binding.team" {
		t.Errorf("mention = %+v", got)
	}
	// A team with no binding falls through to the default.
	if got := r.ResolveRoute(RouteInput{Channel: "slack", TeamID: "T9"}); got.AgentID != "main" || got.MatchedBy != "default" {
		t.Errorf("unbound team = %+v", got)
	}
	// An empty binding agent id resolves to the default agent.
	r2 := NewRouteResolver(testConfig(agents, []config.AgentBinding{{Match: config.BindingMatch{Channel: "irc", AccountID: "*"}}}))
	if got := r2.ResolveRoute(RouteInput{Channel: "irc"}); got.AgentID != "main" {
		t.Errorf("empty binding agent = %+v", got)
	}
}

func TestReaches_BindingKinds(t *testing.T) {
	agents := []config.AgentConfig{{ID: "main", Default: true}, {ID: "alice"}, {ID: "bob"}}
	bindings := []config.AgentBinding{
		{AgentID: "alice", Match: config.BindingMatch{Channel: "discord", AccountID: "*", GuildID: "G1"}},
		{AgentID: "main", AgentMentions: []string{"bob"}, Match: config.BindingMatch{Channel: "slack", AccountID: "*", TeamID: "T1"}},
		{AgentID: "main", AgentMentions: []string{"*"}, Match: config.BindingMatch{Channel: "irc", AccountID: "*"}},
		{AgentID: "alice", Match: config.BindingMatch{Channel: "matrix", AccountID: "*", Peer: &config.PeerMatch{ID: "p1"}}},
	}
	r := NewRouteResolver(testConfig(agents, bindings))
	tests := []struct {
		name  string
		input RouteInput
		agent string
		want  bool
	}{
		{"guild binding", RouteInput{Channel: "discord", GuildID: "G1"}, "alice", true},
		{"other guild", RouteInput{Channel: "discord", GuildID: "G2"}, "alice", false},
		{"team mention", RouteInput{Channel: "slack", TeamID: "T1"}, "bob", true},
		{"other team", RouteInput{Channel: "slack", TeamID: "T2"}, "bob", false},
		{"wildcard mention", RouteInput{Channel: "irc"}, "bob", true},
		{"peer binding without kind matches nothing", RouteInput{Channel: "matrix", Peer: &RoutePeer{Kind: "direct", ID: "p1"}}, "alice", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.Reaches(tt.input, tt.agent); got != tt.want {
				t.Errorf("Reaches = %v, want %v", got, tt.want)
			}
		})
	}
}
