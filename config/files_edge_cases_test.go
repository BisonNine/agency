// ClawEh
// License: MIT

package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReadPermissions(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file gives defaults", func(t *testing.T) {
		got, err := ReadPermissions(filepath.Join(dir, "absent.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, DefaultPermissions()) {
			t.Errorf("ReadPermissions = %+v, want defaults", got)
		}
	})

	t.Run("unreadable path", func(t *testing.T) {
		if _, err := ReadPermissions(dir); err == nil {
			t.Error("reading a directory succeeded")
		}
	})

	t.Run("invalid mode", func(t *testing.T) {
		path := filepath.Join(dir, "bad.json")
		body := `{"security":{"permissions":{"root":{"dir_mode":"9999"}}}}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := ReadPermissions(path)
		if err == nil || !strings.Contains(err.Error(), "security.permissions.root.dir_mode") {
			t.Errorf("err = %v, want a refusal naming root.dir_mode", err)
		}
	})

	t.Run("file modes completed with defaults", func(t *testing.T) {
		path := filepath.Join(dir, "ok.json")
		body := `{"security":{"permissions":{"root":{"file_mode":"0640"}}}}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := ReadPermissions(path)
		if err != nil {
			t.Fatal(err)
		}
		if m := got.Modes(AreaRoot); m.File != 0o640 || m.Dir != DefaultPermissions().Modes(AreaRoot).Dir {
			t.Errorf("root modes = %v", m)
		}
	})
}

func TestDecodeRawObjectErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ``},
		{"array", `[1]`},
		{"bad key token", `{"a":1,}`},
		{"bad value", `{"a":}`},
		{"unterminated", `{"a":1`},
	}
	for _, tt := range tests {
		if _, err := decodeRawObject([]byte(tt.in)); err == nil {
			t.Errorf("%s: decodeRawObject(%q) succeeded", tt.name, tt.in)
		}
	}
	members, err := decodeRawObject([]byte(`{"a":1,"b":{"c":[2]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if v := rawMemberValue(members, "b"); string(v) != `{"c":[2]}` {
		t.Errorf("b = %s", v)
	}
	if v := rawMemberValue(members, "z"); v != nil {
		t.Errorf("missing key = %s, want nil", v)
	}
}

func TestWritePermissionsBlockErrors(t *testing.T) {
	dir := t.TempDir()
	if err := writePermissionsBlock(filepath.Join(dir, "absent.json"), DefaultPermissions()); err == nil {
		t.Error("missing file accepted")
	}
	for name, body := range map[string]string{
		"not an object":       `[1,2]`,
		"security not object": `{"security":[1]}`,
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		err := writePermissionsBlock(path, DefaultPermissions())
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("%s: err = %v, want an error naming the file", name, err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != body {
			t.Errorf("%s: file rewritten to %s", name, got)
		}
	}
}

func TestWritePermissionsBlockAddsSecurity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"alice":1,"security":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writePermissionsBlock(path, DefaultPermissions()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Alice    int `json:"alice"`
		Security struct {
			Permissions PermissionsConfig `json:"permissions"`
		} `json:"security"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Alice != 1 || !reflect.DeepEqual(doc.Security.Permissions, DefaultPermissions()) {
		t.Errorf("written document = %s", data)
	}
}

func TestIsSecretKey(t *testing.T) {
	for key, want := range map[string]bool{
		"api_key": true, "openai_api_key": true, "token": true, "bot_token": true,
		"secret": true, "client_secret": true, "password": true, "smtp_password": true,
		"password_hash": true, "PASSWORD": true, "name": false, "tokens": false,
	} {
		if got := IsSecretKey(key); got != want {
			t.Errorf("IsSecretKey(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestResolveSecretRefErrors(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name string
		ref  string
		want string
	}{
		{"env without name", "env: ", "names no environment variable"},
		{"missing file", "file:" + filepath.Join(dir, "absent"), "no such file"},
		{"directory", "file:" + dir, "is a directory"},
	}
	for _, tt := range tests {
		_, err := resolveSecretRef(tt.ref, 0o600)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
		}
	}
}

func TestResolveSecretRefUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("s3cret"), 0o200); err != nil {
		t.Fatal(err)
	}
	_, err := resolveSecretRef("file:"+path, 0o600)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("err = %v, want permission denied", err)
	}
}

func TestMergeSecretRefs(t *testing.T) {
	old := []secretRef{{Path: "a", Ref: "env:A"}, {Path: "b", Ref: "env:B"}}
	if got := mergeSecretRefs(old, nil); !reflect.DeepEqual(got, old) {
		t.Errorf("no newer refs changed old: %+v", got)
	}
	newer := []secretRef{{Path: "b", Ref: "env:B2"}, {Path: "c", Ref: "env:C"}}
	want := []secretRef{{Path: "a", Ref: "env:A"}, {Path: "b", Ref: "env:B2"}, {Path: "c", Ref: "env:C"}}
	if got := mergeSecretRefs(old, newer); !reflect.DeepEqual(got, want) {
		t.Errorf("mergeSecretRefs = %+v, want %+v", got, want)
	}
}

func TestResolveConfigSecretsRefusesMissingEnv(t *testing.T) {
	t.Setenv("CLAW_HOME", t.TempDir())
	t.Setenv("CLAW_TEST_ABSENT_KEY", "")
	if err := os.Unsetenv("CLAW_TEST_ABSENT_KEY"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Channels.Discord.Token = "env:CLAW_TEST_ABSENT_KEY"
	if _, err := resolveConfigSecrets(cfg); err == nil || !strings.Contains(err.Error(), "CLAW_TEST_ABSENT_KEY") {
		t.Errorf("err = %v, want one naming the variable", err)
	}
}

func TestCloneNil(t *testing.T) {
	var c *Config
	if _, err := c.Clone(); err == nil {
		t.Error("cloning nil config succeeded")
	}
}

func TestStoreLoadErrors(t *testing.T) {
	t.Setenv("CLAW_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(path); err == nil {
		t.Error("NewStore accepted an unparsable file")
	}

	good := filepath.Join(t.TempDir(), "config.json")
	s, err := NewStore(good)
	if err != nil {
		t.Fatal(err)
	}
	before := s.Current()
	if err := os.WriteFile(good, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reload(); err == nil {
		t.Error("Reload accepted an unparsable file")
	}
	if s.Current() != before {
		t.Error("failed Reload replaced the current config")
	}
}

func TestLoadConfigReadAndDecodeErrors(t *testing.T) {
	t.Setenv("CLAW_HOME", t.TempDir())
	dir := t.TempDir()
	if _, err := LoadConfig(dir); err == nil {
		t.Error("loading a directory succeeded")
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"models":"alice"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Error("a models string was accepted")
	}
}

func TestLoadConfigMigratesLegacyCompressionKeys(t *testing.T) {
	t.Setenv("CLAW_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"agents":{"defaults":{"compress_normal_percent":60,"compress_safety_percent":90,"compress_token_safety_margin":1.2}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Agents.Defaults.Compression
	if c == nil || c.Trigger == nil || c.Estimate == nil {
		t.Fatalf("compression not migrated: %+v", c)
	}
	if *c.Trigger.NormalPercent != 60 || *c.Trigger.SafetyPercent != 90 || *c.Estimate.TokenSafetyMargin != 1.2 {
		t.Errorf("migrated values = %d %d %v", *c.Trigger.NormalPercent, *c.Trigger.SafetyPercent, *c.Estimate.TokenSafetyMargin)
	}
}

func TestMigrateLegacyMountsMergesNotify(t *testing.T) {
	c := &Config{}
	c.Agents.List = []AgentConfig{{
		ID:             "alice",
		AllowedFolders: []AllowedFolderConfig{{Path: "/srv/shared"}},
		LegacyMounts:   []LegacyMountConfig{{Path: " /srv/shared ", Notify: true}},
	}}
	c.migrateLegacyMounts()
	a := c.Agents.List[0]
	if len(a.AllowedFolders) != 1 || !a.AllowedFolders[0].Notify {
		t.Errorf("AllowedFolders = %+v, want one folder with notify", a.AllowedFolders)
	}
	if a.LegacyMounts != nil {
		t.Error("legacy mounts not cleared")
	}
}

func TestIgnoredAllowedFoldersReasons(t *testing.T) {
	existing := t.TempDir()
	c := &Config{}
	c.Agents.List = []AgentConfig{{ID: "alice", AllowedFolders: []AllowedFolderConfig{
		{Path: existing},
		{Path: "relative/dir"},
		{Path: filepath.Join(existing, "absent")},
	}}}
	got := c.IgnoredAllowedFolders()
	want := []IgnoredAllowedFolder{
		{Agent: "alice", Path: "relative/dir", Reason: FolderNotAbsolute},
		{Agent: "alice", Path: filepath.Join(existing, "absent"), Reason: FolderNotFound},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("IgnoredAllowedFolders = %+v, want %+v", got, want)
	}
}
