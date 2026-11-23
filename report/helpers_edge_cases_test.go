// ClawEh
// License: MIT

package report

import (
	"testing"
	"time"

	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/cron"
	"go.bison9.com/agency/routing"
)

// The group trigger names each condition a group message must meet.
func TestGroupTrigger(t *testing.T) {
	for _, tc := range []struct {
		g    config.GroupTriggerConfig
		want string
	}{
		{config.GroupTriggerConfig{}, "any message"},
		{config.GroupTriggerConfig{MentionOnly: true}, "mention only"},
		{config.GroupTriggerConfig{Prefixes: []string{"!", "/ask"}}, "prefixes ! /ask"},
		{config.GroupTriggerConfig{MentionOnly: true, Prefixes: []string{"!"}}, "mention only, prefixes !"},
	} {
		if got := groupTrigger(tc.g); got != tc.want {
			t.Errorf("groupTrigger(%+v) = %q, want %q", tc.g, got, tc.want)
		}
	}
}

// Every schedule kind reads as when the job runs.
func TestScheduleText(t *testing.T) {
	every, at := int64(90_000), time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	for _, tc := range []struct {
		s    cron.CronSchedule
		want string
	}{
		{cron.CronSchedule{Kind: "every", EveryMS: &every}, "every 90s"},
		{cron.CronSchedule{Kind: "cron", Expr: "0 9 * * *", TZ: "UTC"}, "0 9 * * * (UTC)"},
		{cron.CronSchedule{Kind: "cron", Expr: "0 9 * * *"}, "0 9 * * *"},
		{cron.CronSchedule{Kind: cron.KindListen}, "listen (continuous)"},
		{cron.CronSchedule{Kind: "at", AtMS: &at}, "once at " + time.UnixMilli(at).Format(timeFormat)},
		{cron.CronSchedule{Kind: "at"}, "one-time"},
	} {
		if got := scheduleText(tc.s); got != tc.want {
			t.Errorf("scheduleText(%+v) = %q, want %q", tc.s, got, tc.want)
		}
	}
}

// A watch job's payload names the probed tool.
func TestPayloadTextWatch(t *testing.T) {
	p := cron.CronPayload{Mode: "watch", Message: "new  mail", Watch: &cron.CronWatch{Tool: "mail_list"}}
	if got := payloadText(p); got != "watch: watch mail_list: new mail" {
		t.Errorf("payloadText = %q", got)
	}
}

func TestByteSize(t *testing.T) {
	for n, want := range map[int]string{
		0: "0 bytes", 1000: "1000 bytes", 2048: "2 KiB", 3 << 20: "3 MiB",
	} {
		if got := byteSize(n); got != want {
			t.Errorf("byteSize(%d) = %q, want %q", n, got, want)
		}
	}
}

// Every flag that changes what a model may do is listed.
func TestModelSettings(t *testing.T) {
	m := &modelspec.Model{
		NoTools: true, ThinkingLevel: "high", Vision: "auto", RequestTimeout: 30,
		ContextWindow: 8000, ReasoningEffort: "low",
	}
	want := "disabled, no tools, thinking high, vision auto, timeout 30s, context 8000, reasoning low"
	if got := modelSettings(m); got != want {
		t.Errorf("modelSettings = %q, want %q", got, want)
	}
	if got := modelSettings(&modelspec.Model{Enabled: true, Vision: modelspec.VisionOff}); got != "" {
		t.Errorf("modelSettings(plain) = %q, want empty", got)
	}
}

// The default agent follows the router: the marked default, else the first
// agent, else the built-in id.
func TestDefaultAgentID(t *testing.T) {
	cfg := func(list ...config.AgentConfig) *config.Config {
		c := &config.Config{}
		c.Agents.List = list
		return c
	}
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"no agents", cfg(), routing.DefaultAgentID},
		{"marked default", cfg(config.AgentConfig{ID: "alice"}, config.AgentConfig{ID: "Bob", Default: true}), routing.NormalizeAgentID("Bob")},
		{"first agent", cfg(config.AgentConfig{ID: "Alice"}, config.AgentConfig{ID: "bob"}), routing.NormalizeAgentID("Alice")},
		{"first agent without id", cfg(config.AgentConfig{}), routing.DefaultAgentID},
	} {
		if got := defaultAgentID(tc.cfg); got != tc.want {
			t.Errorf("%s: defaultAgentID = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// An mcp_tools entry with doubled underscores matches as the runtime
// collapses it.
func TestMCPEntryReachCollapsesUnderscores(t *testing.T) {
	if desc, ok := mcpEntryReach("mail__send", "mail"); !ok || desc != "tools starting with send" {
		t.Errorf("mcpEntryReach = %q, %v", desc, ok)
	}
	if _, ok := mcpEntryReach("  ", "mail"); ok {
		t.Error("a blank entry reaches the server")
	}
}

func TestSmallFormatters(t *testing.T) {
	for _, tc := range []struct{ name, got, want string }{
		{"default prefix", prefixState(""), "default prefix"},
		{"custom prefix", prefixState("abc"), "custom prefix (3 characters)"},
		{"zero time", msTime(0), unknown},
		{"all interfaces", bindAddr(" ", 8080), "(all interfaces):8080"},
		{"bound host", bindAddr("127.0.0.1", 8080), "127.0.0.1:8080"},
		{"url without credentials", redactURL("http://proxy:3128"), "http://proxy:3128"},
		{"unparsable url", redactURL("http://[::1"), "http://[::1"},
		{"url with credentials", redactURL("http://alice:secret@proxy:3128"), "http://proxy:3128 (credentials redacted)"},
		{"host of url", hostOf("https://example.com/x"), "example.com"},
		{"host of non-url", hostOf("not a url"), "not a url"},
		{"webui", displayChannel("WebUI"), "WebUI"},
		{"empty channel", displayChannel(""), ""},
		{"other channel", displayChannel("telegram"), "Telegram"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}
