// Agency
// License: MIT

package config

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"go.bison9.com/agency/global"
)

func TestFlexibleStringSliceUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    FlexibleStringSlice
		wantErr bool
	}{
		{name: "strings", in: `["a","b"]`, want: FlexibleStringSlice{"a", "b"}},
		{name: "mixed", in: `["a",123,true,null]`, want: FlexibleStringSlice{"a", "123", "true", "<nil>"}},
		{name: "large number", in: `[1234567890123]`, want: FlexibleStringSlice{"1234567890123"}},
		{name: "not a list", in: `{"a":1}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got FlexibleStringSlice
			err := json.Unmarshal([]byte(tt.in), &got)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConfigPathAccessors(t *testing.T) {
	c := &Config{dataDir: "/data"}
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"cron", c.CronPath(), "/data/cron"},
		{"fusion", c.FusionPath(), "/data/fusion"},
		{"bin", c.BinPath(), filepath.Join("/data", BinDirName)},
		{"backup default", c.BackupDir(), "/data/backup"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
	c.Backup.Dest = "  /mnt/off-host  "
	if got := c.BackupDir(); got != "/mnt/off-host" {
		t.Errorf("BackupDir with dest = %q, want /mnt/off-host", got)
	}
}

func TestBaseDirExplicit(t *testing.T) {
	c := &Config{dataDir: "/data"}
	c.Agents.BaseDir = "/srv/agents"
	if got := c.BaseDir(); got != "/srv/agents" {
		t.Errorf("BaseDir = %q, want /srv/agents", got)
	}
}

func TestAgentSessionDirs(t *testing.T) {
	c := &Config{dataDir: "/data"}
	c.Agents.List = []AgentConfig{
		{ID: "alice"},
		{ID: "bob", Workspace: "/ws/bob"},
		{ID: "carol", Enabled: new(false)},
		{ID: "dave", Workspace: "/ws/bob"}, // shares bob's workspace: deduped
	}
	got := c.AgentSessionDirs()
	want := []string{"/data/agents/alice/sessions", "/ws/bob/sessions"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AgentSessionDirs = %q, want %q", got, want)
	}
}

func TestAgentDirsDedupesWorkspaces(t *testing.T) {
	c := &Config{dataDir: "/data"}
	c.Agents.List = []AgentConfig{
		{ID: "alice", Workspace: "/ws/shared/"},
		{ID: "bob", Workspace: "/ws/shared"},
		{ID: "carol"},
	}
	want := []string{"/data/agents", "/ws/shared"}
	if got := c.AgentDirs(); !reflect.DeepEqual(got, want) {
		t.Errorf("AgentDirs = %q, want %q", got, want)
	}
}

func TestMaestroDataDir(t *testing.T) {
	if got := MaestroDataDir("/ws/alice"); got != "/ws/alice/maestro" {
		t.Errorf("MaestroDataDir = %q", got)
	}
}

func TestSharesCommon(t *testing.T) {
	var nilAgent *AgentConfig
	tests := []struct {
		name string
		a    *AgentConfig
		want bool
	}{
		{"nil agent", nilAgent, true},
		{"unset", &AgentConfig{}, true},
		{"true", &AgentConfig{ShareCommon: new(true)}, true},
		{"false", &AgentConfig{ShareCommon: new(false)}, false},
	}
	for _, tt := range tests {
		if got := tt.a.SharesCommon(); got != tt.want {
			t.Errorf("%s: SharesCommon = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestAgentIdentity(t *testing.T) {
	a := &AgentConfig{ID: "Alice", Name: "Alice A."}
	got := a.Identity()
	want := global.Identity{Agent: NormalizeAgentID("Alice"), Name: "Alice A.", Project: global.Project}
	if got != want {
		t.Errorf("Identity = %+v, want %+v", got, want)
	}
}

func TestAgentHasFusionAndSuites(t *testing.T) {
	c := &Config{}
	c.Agents.List = []AgentConfig{
		{
			ID: "alice", Fusion: true, Forum: true, CCv3: true,
			Maestro:     &MaestroConfig{Enabled: true},
			Classifiers: &AgentClassifiersConfig{Enabled: true},
			Cogmem:      new(false),
		},
		{ID: "bob"},
	}
	if !c.AgentHasFusion("ALICE") {
		t.Error("AgentHasFusion(alice) = false")
	}
	if c.AgentHasFusion("bob") || c.AgentHasFusion("nobody") {
		t.Error("AgentHasFusion true for an agent without fusion")
	}
	for _, s := range []string{"maestro", "fusion", "forum", "ccv3", "classifiers"} {
		if !c.AgentSuiteEnabled("alice", s) {
			t.Errorf("alice suite %s off", s)
		}
		if c.AgentSuiteEnabled("bob", s) {
			t.Errorf("bob suite %s on", s)
		}
		if c.AgentSuiteEnabled("nobody", s) {
			t.Errorf("unknown agent suite %s on", s)
		}
	}
	if c.AgentSuiteEnabled("alice", "cogmem") {
		t.Error("alice cogmem on despite cogmem=false")
	}
	if !c.AgentSuiteEnabled("bob", "cogmem") || !c.AgentSuiteEnabled("nobody", "cogmem") {
		t.Error("cogmem default is not on")
	}
	if c.AgentSuiteEnabled("alice", "unknown-suite") {
		t.Error("unknown suite on")
	}
}

func TestCognitiveMemoryEnabledNilAgent(t *testing.T) {
	var a *AgentConfig
	if a.CognitiveMemoryEnabled() {
		t.Error("nil agent has cognitive memory")
	}
}

func TestGetMaxMediaSize(t *testing.T) {
	d := &AgentDefaults{}
	if got := d.GetMaxMediaSize(); got != DefaultMaxMediaSize {
		t.Errorf("default = %d", got)
	}
	d.MaxMediaSize = 1024
	if got := d.GetMaxMediaSize(); got != 1024 {
		t.Errorf("set = %d", got)
	}
}

func TestCoalesceConfigDefaults(t *testing.T) {
	var zero CoalesceConfig
	if got := zero.Window(); got != DefaultCoalesceWindowMS*time.Millisecond {
		t.Errorf("Window = %v", got)
	}
	if got := zero.MaxWait(); got != DefaultCoalesceMaxWaitMS*time.Millisecond {
		t.Errorf("MaxWait = %v", got)
	}
	if got := zero.MaxMessageCount(); got != DefaultCoalesceMaxMessages {
		t.Errorf("MaxMessageCount = %d", got)
	}
	if !zero.IsEnabled() {
		t.Error("unset Enabled is off")
	}
	set := CoalesceConfig{Enabled: new(false), WindowMS: 250, MaxWaitMS: 5000, MaxMessages: 7}
	if set.Window() != 250*time.Millisecond || set.MaxWait() != 5*time.Second || set.MaxMessageCount() != 7 {
		t.Errorf("explicit values not honoured: %v %v %d", set.Window(), set.MaxWait(), set.MaxMessageCount())
	}
	if set.IsEnabled() {
		t.Error("explicit false is on")
	}
}

func TestTelegramBotChannelName(t *testing.T) {
	for id, want := range map[string]string{"": "telegram", "default": "telegram", "alice": "telegram-alice"} {
		if got := (TelegramBotConfig{ID: id}).ChannelName(); got != want {
			t.Errorf("ChannelName(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestTelegramBotUnmarshalBadJSON(t *testing.T) {
	var b TelegramBotConfig
	if err := json.Unmarshal([]byte(`{"enabled":"yes"}`), &b); err == nil {
		t.Error("bad bot JSON accepted")
	}
}

func TestMergeAPIKeys(t *testing.T) {
	tests := []struct {
		name   string
		key    string
		keys   []string
		expect []string
	}{
		{"none", "", nil, nil},
		{"single", " k1 ", nil, []string{"k1"}},
		{"merged and deduped", "k1", []string{"k2", " k1", "", "  ", "k2", "k3"}, []string{"k1", "k2", "k3"}},
		{"list only", "", []string{"k2"}, []string{"k2"}},
	}
	for _, tt := range tests {
		if got := MergeAPIKeys(tt.key, tt.keys); !reflect.DeepEqual(got, tt.expect) {
			t.Errorf("%s: MergeAPIKeys = %q, want %q", tt.name, got, tt.expect)
		}
	}
}

func TestTaskUseLabelAndKindName(t *testing.T) {
	labels := map[TaskUse]string{
		TaskUseText: "Generate text", TaskUseImage: "Generate images", TaskUseTTS: "Text to speech",
		TaskUseSTT: "Transcription", TaskUseVision: "Describe images", TaskUseClassify: "Classification",
		TaskUse("other"): "other",
	}
	for u, want := range labels {
		if got := u.Label(); got != want {
			t.Errorf("Label(%q) = %q, want %q", u, got, want)
		}
	}
	kinds := map[TaskUse]string{
		TaskUseText: "text", TaskUseImage: "images", TaskUseTTS: "speech", TaskUseSTT: "transcription",
		TaskUseVision: string(TaskUseVision),
	}
	for u, want := range kinds {
		if got := TaskSpawnKindName(u); got != want {
			t.Errorf("TaskSpawnKindName(%q) = %q, want %q", u, got, want)
		}
	}
}

func TestAlwaysShownNamespaces(t *testing.T) {
	var nilCfg *Config
	if nilCfg.AlwaysShownNamespaces() != nil {
		t.Error("nil config returned namespaces")
	}
	c := &Config{}
	c.Tools.Discovery.AlwaysShownNamespaces = []string{"file", "web"}
	if got := c.AlwaysShownNamespaces(); !slices.Equal(got, []string{"file", "web"}) {
		t.Errorf("AlwaysShownNamespaces = %q", got)
	}
}

func TestClassifierModelAndAgentName(t *testing.T) {
	if got := (&AgentConfig{}).ClassifierModel(); got != "" {
		t.Errorf("ClassifierModel without block = %q", got)
	}
	a := &AgentConfig{Classifiers: &AgentClassifiersConfig{Model: " small "}}
	if got := a.ClassifierModel(); got != "small" {
		t.Errorf("ClassifierModel = %q", got)
	}
	c := &Config{}
	c.Agents.List = []AgentConfig{{ID: "alice", Name: "Alice"}}
	if got := c.agentName("alice"); got != "Alice" {
		t.Errorf("agentName(alice) = %q", got)
	}
	if got := c.agentName("bob"); got != "bob" {
		t.Errorf("agentName(bob) = %q", got)
	}
}

func TestHumanProblemKeySharedName(t *testing.T) {
	a := HumanProblem{Kind: HumanSharedName, Model: "m1"}
	b := HumanProblem{Kind: HumanSharedName, Model: "m2"}
	if a.key() == b.key() {
		t.Error("shared-name problems for different models share a key")
	}
}

func TestChatBoundElsewhereUnknownAgent(t *testing.T) {
	c := &Config{Bindings: []AgentBinding{
		{AgentID: "bob", Match: BindingMatch{Channel: "telegram"}, DeliverTo: "42"},
	}}
	name, ok := c.chatBoundElsewhere("alice", "telegram", "42")
	if !ok || name != "bob" {
		t.Errorf("chatBoundElsewhere = %q, %v; want bob, true", name, ok)
	}
	if _, ok := c.chatBoundElsewhere("alice", "slack", "42"); ok {
		t.Error("binding on another channel matched")
	}
}

func TestChannelKindsInUseSecMsg(t *testing.T) {
	c := &Config{}
	c.Channels.SecMsg = []SecMsgConfig{{Enabled: false}, {Enabled: true}}
	if got := c.ChannelKindsInUse(); !slices.Contains(got, ChannelKindSecMsg) {
		t.Errorf("ChannelKindsInUse = %q, want secmsg", got)
	}
}

func TestEffectiveCompressionOverlaysEveryField(t *testing.T) {
	defaults := &CompressionConfig{
		TargetPercent: new(50),
		Trigger:       &CompressionTriggerConfig{MinPercent: new(40), Days: new(9)},
		Retain:        &CompressionRetainConfig{TokenPercent: new(20), MinMessages: new(4)},
		Estimate:      &CompressionEstimateConfig{CharsPerToken: new(4.0)},
	}
	agent := &AgentConfig{Compression: &CompressionConfig{
		Trigger: &CompressionTriggerConfig{
			NormalPercent: new(70), SafetyPercent: new(90),
			MessageCount: new(200), Days: new(3),
		},
		Retain:   &CompressionRetainConfig{MaxTokens: new(8000), MaxAgeDays: new(14)},
		Estimate: &CompressionEstimateConfig{TokenSafetyMargin: new(1.1)},
	}}
	got := agent.EffectiveCompression(defaults)
	tr, r, e := got.Trigger, got.Retain, got.Estimate
	checks := []struct {
		name      string
		got, want any
	}{
		{"target", *got.TargetPercent, 50},
		{"min", *tr.MinPercent, 40},
		{"normal", *tr.NormalPercent, 70},
		{"safety", *tr.SafetyPercent, 90},
		{"count", *tr.MessageCount, 200},
		{"days (agent wins)", *tr.Days, 3},
		{"token percent", *r.TokenPercent, 20},
		{"max tokens", *r.MaxTokens, 8000},
		{"max age", *r.MaxAgeDays, 14},
		{"min messages", *r.MinMessages, 4},
		{"chars", *e.CharsPerToken, 4.0},
		{"margin", *e.TokenSafetyMargin, 1.1},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if *defaults.Trigger.Days != 9 {
		t.Error("overlay modified the defaults")
	}
	var nilAgent *AgentConfig
	if got := nilAgent.EffectiveCompression(nil); got == nil || got.Trigger != nil {
		t.Errorf("nil agent and defaults = %+v, want empty", got)
	}
}

func TestQuoteAgentIDEscapes(t *testing.T) {
	tests := map[string]string{
		"a\nb":        `"a\nb"`,
		"a\rb":        `"a\rb"`,
		"a\tb":        `"a\tb"`,
		`a"b\`:        `"a\"b\\"`,
		"a\u200bb":    `"a\u200bb"`,
		"a\U000E0001": `"a\U000e0001"`,
		"a b":         `"a b"`,
	}
	for in, want := range tests {
		if got := quoteAgentID(in); got != want {
			t.Errorf("quoteAgentID(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestCronTarget(t *testing.T) {
	c := &Config{Bindings: []AgentBinding{
		{AgentID: "alice", Default: true, Match: BindingMatch{Channel: "slack", Peer: &PeerMatch{Kind: "channel", ID: "C1"}}},
		{AgentID: "bob", Default: true, Match: BindingMatch{Channel: "telegram"}, DeliverTo: "42"},
		{AgentID: "carol", Default: true, Match: BindingMatch{Channel: "telegram"}, DeliverTo: "7", DeliverPeerKind: "group"},
	}}
	tests := []struct {
		agent               string
		channel, chat, kind string
		ok                  bool
	}{
		{"alice", "slack", "C1", "channel", true},
		{"bob", "telegram", "42", "direct", true},
		{"carol", "telegram", "7", "group", true},
		{"dave", "", "", "", false},
	}
	for _, tt := range tests {
		ch, chat, kind, ok := c.CronTarget(tt.agent)
		if ch != tt.channel || chat != tt.chat || kind != tt.kind || ok != tt.ok {
			t.Errorf("CronTarget(%s) = %q %q %q %v", tt.agent, ch, chat, kind, ok)
		}
	}
}
