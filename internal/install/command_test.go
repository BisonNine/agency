package install

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/internal"
	"go.bison9.com/agency/internal/testbinary"
)

// An existing service decides the mode: re-running the installer at the wrong
// privilege level is refused instead of writing a second unit of the other
// kind for the same install.
func TestCheckExistingMode(t *testing.T) {
	testbinary.Set(t, "alice-claw")
	system := &ExistingInstall{ServiceType: "systemd-system", ServicePath: "/etc/systemd/system/claw.service", User: "alice"}
	userSvc := &ExistingInstall{ServiceType: "systemd-user", ServicePath: "/home/alice/.config/systemd/user/claw.service", User: "alice"}
	daemon := &ExistingInstall{ServiceType: "launchd-daemon", ServicePath: "/Library/LaunchDaemons/" + launchdPlistName}
	agent := &ExistingInstall{ServiceType: "launchd-agent", ServicePath: "/Users/alice/Library/LaunchAgents/" + launchdPlistName, User: "alice"}
	binaryOnly := &ExistingInstall{BinaryPath: "/opt/agency/claw"}

	cases := []struct {
		name     string
		existing *ExistingInstall
		isRoot   bool
		wantErr  string
	}{
		{"system service without sudo", system, false, "run `sudo alice-claw install`"},
		{"system service with sudo", system, true, ""},
		{"user service with sudo", userSvc, true, "as alice, without sudo"},
		{"user service without sudo", userSvc, false, ""},
		{"launchd daemon without sudo", daemon, false, "run `sudo alice-claw install`"},
		{"launchd agent with sudo", agent, true, "without sudo"},
		{"binary only, either way", binaryOnly, false, ""},
		{"nothing installed", nil, true, ""},
	}
	for _, c := range cases {
		err := checkExistingMode(c.existing, c.isRoot)
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: err = %v, want it to contain %q", c.name, err, c.wantErr)
		}
	}
}

func TestBuildUnit_RunsAsUserAndStartsAtBoot(t *testing.T) {
	t.Setenv(global.EnvVarHome, "") // defaults to user's .agency
	t.Setenv("PATH", "/home/alice/.local/bin:/home/alice/.nvm/versions/node/v24/bin:/usr/bin")
	unit := buildUnit("alice", "alice", "/home/alice/bin/claw", "/home/alice/bin")

	wants := []string{
		"User=alice",
		"Group=alice",
		"ExecStart=/home/alice/bin/claw",
		"WantedBy=multi-user.target",
		"KillMode=mixed",
		"TimeoutStopSec=60",
		"NoNewPrivileges=yes",
		"PrivateTmp=yes",
		"Environment=AGENCY_HOME=/home/alice/.agency",
		"Environment=PATH=/home/alice/bin:/home/alice/.local/bin:/home/alice/.nvm/versions/node/v24/bin:/usr/local/bin:/usr/bin:/bin",
	}
	for _, w := range wants {
		if !strings.Contains(unit, w) {
			t.Errorf("unit missing %q:\n%s", w, unit)
		}
	}
}

func TestBuildUnit_IncludesAgencyHomeWhenSet(t *testing.T) {
	t.Setenv(global.EnvVarHome, "/srv/claw-data")
	unit := buildUnit("bob", "staff", "/home/bob/.local/bin/claw", "/home/bob/.local/bin")

	if !strings.Contains(unit, "Environment="+global.EnvVarHome+"=/srv/claw-data") {
		t.Errorf("unit should carry AGENCY_HOME when set:\n%s", unit)
	}
}

func TestServicePATH_PrependsBinDirAndDedups(t *testing.T) {
	t.Setenv("PATH", "/home/bob/bin:/usr/bin:/home/bob/.local/bin")
	got := servicePATH("/home/bob", "/home/bob/bin")
	want := "/home/bob/bin:/home/bob/.local/bin:/usr/local/bin:/usr/bin:/bin"
	if got != want {
		t.Errorf("servicePATH = %q, want %q", got, want)
	}
}

func TestApplyServerSettings_WritesHostAndPort(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(global.EnvVarHome, dir) // AGENCY_HOME → config under dir

	if err := applyServerSettings("0.0.0.0", 12345); err != nil {
		t.Fatalf("applyServerSettings: %v", err)
	}
	cfg, err := config.LoadConfig(internal.GetConfigPath())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Gateway.Host != "0.0.0.0" || cfg.Gateway.Port != 12345 {
		t.Fatalf("got %s:%d, want 0.0.0.0:12345", cfg.Gateway.Host, cfg.Gateway.Port)
	}

	// A blank host / zero port must leave the stored values untouched.
	if err := applyServerSettings("", 0); err != nil {
		t.Fatalf("applyServerSettings (no-op): %v", err)
	}
	cfg, loadErr := config.LoadConfig(internal.GetConfigPath())
	if loadErr != nil {
		t.Fatalf("LoadConfig: %v", loadErr)
	}
	if cfg.Gateway.Host != "0.0.0.0" || cfg.Gateway.Port != 12345 {
		t.Fatalf("blank args changed bind: got %s:%d", cfg.Gateway.Host, cfg.Gateway.Port)
	}
}

func TestApplyAllowlist_WritesCIDRs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(global.EnvVarHome, dir)

	if err := applyAllowlist("192.168.5.0/24, 10.1.0.0/16 ,"); err != nil {
		t.Fatalf("applyAllowlist: %v", err)
	}
	cfg, err := config.LoadConfig(internal.GetConfigPath())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Gateway.AllowedCIDRs) != 2 ||
		cfg.Gateway.AllowedCIDRs[0] != "192.168.5.0/24" || cfg.Gateway.AllowedCIDRs[1] != "10.1.0.0/16" {
		t.Fatalf("Gateway.AllowedCIDRs = %v, want [192.168.5.0/24 10.1.0.0/16]", cfg.Gateway.AllowedCIDRs)
	}
}

func TestPrintListenerSummary(t *testing.T) {
	var b strings.Builder
	printListenerSummary(&b, config.GatewayConfig{Host: "127.0.0.1", Port: 18790})
	got := b.String()
	for _, want := range []string{
		"WebUI on this machine:  http://127.0.0.1:18790/",
		"HTTPS on the network:   https://",
		"Allowed networks:       localhost only",
		"Certificate:            self-signed",
		"Network and certificate settings can be changed in the WebUI or config.json.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("default summary missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "unencrypted") {
		t.Errorf("loopback HTTP reported as network HTTP:\n%s", got)
	}
	// No paragraph: every line is one short labelled fact.
	for l := range strings.SplitSeq(strings.TrimSpace(got), "\n") {
		if len(l) > 100 {
			t.Errorf("summary line too long: %q", l)
		}
	}

	b.Reset()
	printListenerSummary(&b, config.GatewayConfig{Host: "192.168.1.5", Port: 9000, AllowedCIDRs: []string{"192.168.1.0/24"}, TLS: config.TLSConfig{Mode: config.TLSModeOff}})
	got = b.String()
	for _, want := range []string{"WebUI on the network:   http://192.168.1.5:9000/  (HTTP, unencrypted, not recommended)", "HTTPS:                  off", "Allowed networks:       192.168.1.0/24"} {
		if !strings.Contains(got, want) {
			t.Errorf("network HTTP summary missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Certificate:") {
		t.Errorf("certificate line with HTTPS off:\n%s", got)
	}

	b.Reset()
	printListenerSummary(&b, config.GatewayConfig{TLSPort: 9443, TLS: config.TLSConfig{Mode: config.TLSModeLocalhost, CertFile: "/etc/claw/my.crt"}})
	got = b.String()
	if !strings.Contains(got, "HTTPS on this machine:  https://127.0.0.1:9443/") || !strings.Contains(got, "Certificate:            /etc/claw/my.crt") {
		t.Errorf("localhost HTTPS summary:\n%s", got)
	}
	if strings.Contains(got, "Allowed networks") {
		t.Errorf("allowlist shown with nothing on the network:\n%s", got)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"":              "''",
		"/etc/claw":     "'/etc/claw'",
		"it's":          `'it'"'"'s'`,
		"/tmp/a b/claw": "'/tmp/a b/claw'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestIsNetworkBind pins which bind addresses count as off-box, since that is
// what gates the install-time refusal.
func TestIsNetworkBind(t *testing.T) {
	for _, h := range []string{"", "127.0.0.1", "localhost", "::1", "[::1]", " 127.0.0.1 "} {
		if isNetworkBind(h) {
			t.Errorf("isNetworkBind(%q) = true, want false", h)
		}
	}
	for _, h := range []string{"0.0.0.0", "::", "192.168.1.10", "10.0.0.5"} {
		if !isNetworkBind(h) {
			t.Errorf("isNetworkBind(%q) = false, want true", h)
		}
	}
}

// TestLinkOpenClawAlias covers the symlink the Rabbit R1 needs: rabbit-agent
// spawns `openclaw acp`, so an install that omits it leaves the R1 unable to
// connect even though everything else works.
func TestLinkOpenClawAlias(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claw"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := linkOpenClawAlias(dir, "claw"); err != nil {
		t.Fatalf("linkOpenClawAlias() error = %v", err)
	}
	link := filepath.Join(dir, "openclaw")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("openclaw is not a symlink: %v", err)
	}
	if target != "claw" {
		t.Fatalf("openclaw -> %q, want %q (relative, so moving the dir keeps it valid)", target, "claw")
	}

	// Re-running install must keep or replace an existing link rather than failing.
	if err := linkOpenClawAlias(dir, "claw"); err != nil {
		t.Fatalf("second linkOpenClawAlias() error = %v", err)
	}

	// A read-only directory (such as /usr/bin for a non-root user after `apt install`)
	// whose openclaw link already points at claw must succeed without unlinking.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cerr := os.Chmod(dir, 0o755); cerr != nil {
			t.Log(cerr)
		}
	})
	if err := linkOpenClawAlias(dir, "claw"); err != nil {
		t.Fatalf("linkOpenClawAlias() on read-only dir with matching link = %v, want nil", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// A link pointing elsewhere in a writable directory is replaced.
	if err := linkOpenClawAlias(dir, "claw-other"); err != nil {
		t.Fatalf("linkOpenClawAlias() replacing target error = %v", err)
	}
	if got, err := os.Readlink(link); err != nil || got != "claw-other" {
		t.Fatalf("openclaw -> %q, %v, want claw-other", got, err)
	}
}

func TestIsPackageManagedBinDir(t *testing.T) {
	if !isPackageManagedBinDir("linux", "/usr/bin") || !isPackageManagedBinDir("linux", "/bin") {
		t.Error("expected /usr/bin and /bin on linux to be package-managed")
	}
	if isPackageManagedBinDir("darwin", "/usr/bin") || isPackageManagedBinDir("linux", "/usr/local/bin") {
		t.Error("unexpected package-managed classification")
	}
}

func TestBuildUserUnit(t *testing.T) {
	unit := buildUserUnit("/home/alice/.local/bin/claw", "/home/alice", "/home/alice/.local/bin", "/home/alice/.agency")
	if strings.Contains(unit, "User=") {
		t.Errorf("user unit must NOT contain User= directive:\n%s", unit)
	}
	if strings.Contains(unit, "Group=") {
		t.Errorf("user unit must NOT contain Group= directive:\n%s", unit)
	}
	if !strings.Contains(unit, "WantedBy=default.target") {
		t.Errorf("user unit must specify WantedBy=default.target:\n%s", unit)
	}
	if !strings.Contains(unit, "ExecStart=/home/alice/.local/bin/claw") {
		t.Errorf("user unit missing ExecStart:\n%s", unit)
	}
	if !strings.Contains(unit, "WorkingDirectory=/home/alice") {
		t.Errorf("user unit missing WorkingDirectory:\n%s", unit)
	}
	if !strings.Contains(unit, "TimeoutStopSec=60") {
		t.Errorf("user unit missing TimeoutStopSec=60:\n%s", unit)
	}
	if !strings.Contains(unit, "KillMode=mixed") {
		t.Errorf("user unit missing KillMode=mixed:\n%s", unit)
	}
	if !strings.Contains(unit, "NoNewPrivileges=yes") {
		t.Errorf("user unit missing NoNewPrivileges=yes:\n%s", unit)
	}
	if strings.Contains(unit, "PrivateTmp=") {
		t.Errorf("user unit must not set PrivateTmp= (needs unprivileged user namespaces in a per-user manager):\n%s", unit)
	}
	if !strings.Contains(unit, "Environment=AGENCY_HOME=/home/alice/.agency") {
		t.Errorf("user unit missing Environment=AGENCY_HOME:\n%s", unit)
	}
}

func TestBuildLaunchdPlist_UserMode(t *testing.T) {
	plist := buildLaunchdPlist(launchdLabel, "alice", "staff", "/Users/alice/.local/bin/claw", "/Users/alice", "/Users/alice/.local/bin", "/Users/alice/.agency", false)
	if strings.Contains(plist, "<key>UserName</key>") {
		t.Errorf("LaunchAgent plist in user mode must NOT contain UserName:\n%s", plist)
	}
	if strings.Contains(plist, "<key>GroupName</key>") {
		t.Errorf("LaunchAgent plist in user mode must NOT contain GroupName:\n%s", plist)
	}
	if !strings.Contains(plist, "<string>/Users/alice/.local/bin/claw</string>") {
		t.Errorf("LaunchAgent missing binary path:\n%s", plist)
	}
	if !strings.Contains(plist, "<key>RunAtLoad</key>\n\t<true/>") {
		t.Errorf("LaunchAgent missing RunAtLoad:\n%s", plist)
	}
	if !strings.Contains(plist, "<string>/Users/alice</string>") {
		t.Errorf("LaunchAgent missing WorkingDirectory:\n%s", plist)
	}
	if !strings.Contains(plist, "<string>/Users/alice/.agency</string>") {
		t.Errorf("LaunchAgent missing AGENCY_HOME:\n%s", plist)
	}
}

func TestBuildLaunchdPlist_SystemMode(t *testing.T) {
	plist := buildLaunchdPlist(launchdLabel, "alice", "staff", "/usr/local/bin/claw", "/Users/alice", "/usr/local/bin", "/opt/agency", true)
	if !strings.Contains(plist, "<key>UserName</key>\n\t<string>alice</string>") {
		t.Errorf("LaunchDaemon plist in system mode missing UserName:\n%s", plist)
	}
	if !strings.Contains(plist, "<key>GroupName</key>\n\t<string>staff</string>") {
		t.Errorf("LaunchDaemon plist in system mode missing GroupName:\n%s", plist)
	}
	if !strings.Contains(plist, "<string>/usr/local/bin/claw</string>") {
		t.Errorf("LaunchDaemon missing binary path:\n%s", plist)
	}
	if !strings.Contains(plist, "<string>/opt/agency</string>") {
		t.Errorf("LaunchDaemon missing AGENCY_HOME:\n%s", plist)
	}
}

func TestResolveBinDir(t *testing.T) {
	alice := func(root bool) *TargetUser { return &TargetUser{HomeDir: "/home/alice", IsRoot: root} }
	cases := []struct {
		name     string
		files    []string // regular files to create first
		dirs     []string // folders to create first
		tu       *TargetUser
		custom   string
		existing *ExistingInstall
		want     string
	}{
		{name: "custom dir", tu: alice(false), custom: "/srv/tools", want: "/srv/tools"},
		{
			name: "existing binary dir kept", dirs: []string{"/usr/local/sbin"}, tu: alice(true),
			existing: &ExistingInstall{BinaryPath: "/usr/local/sbin/claw"}, want: "/usr/local/sbin",
		},
		{
			name: "opt claw with bin binary", files: []string{"/opt/agency/bin/claw"}, tu: alice(true),
			existing: &ExistingInstall{AgencyHome: "/opt/agency", BinaryPath: "/opt/agency/bin/claw"}, want: "/opt/agency/bin",
		},
		{
			name: "opt claw with old binary moves to bin", files: []string{"/opt/agency/claw"}, tu: alice(true),
			existing: &ExistingInstall{AgencyHome: "/opt/agency", BinaryPath: "/opt/agency/claw"}, want: "/opt/agency/bin",
		},
		{
			name: "opt claw unit naming the old binary", tu: alice(true),
			existing: &ExistingInstall{BinaryPath: "/opt/agency/claw"}, want: "/opt/agency/bin",
		},
		{
			name: "opt claw without a binary", dirs: []string{"/opt/agency"}, tu: alice(true),
			existing: &ExistingInstall{AgencyHome: "/opt/agency"}, want: "/opt/agency/bin",
		},
		{name: "system mode", tu: alice(true), want: "/usr/local/bin"},
		{name: "user mode without ~/bin", tu: alice(false), want: "/home/alice/.local/bin"},
		{name: "user mode with ~/bin", dirs: []string{"/home/alice/bin"}, tu: alice(false), want: "/home/alice/bin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := fakeHost(t)
			for _, d := range tc.dirs {
				if err := os.MkdirAll(h.path(d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, f := range tc.files {
				writeHostFile(t, h, f, "x", 0o700)
			}
			got, err := h.resolveBinDir(tc.tu, tc.custom, tc.existing)
			if err != nil || got != tc.want {
				t.Fatalf("resolveBinDir() = %q, %v, want %q", got, err, tc.want)
			}
			if !h.dirExists(got) {
				t.Errorf("%s was not created", got)
			}
		})
	}
}

// An /opt/agency install puts the binary and the openclaw alias in /opt/agency/bin,
// and points at an older binary and alias in /opt/agency without removing them.
func TestOptAgencyInstall_AliasInBinAndLeftovers(t *testing.T) {
	h := fakeHost(t)
	writeHostFile(t, h, "/opt/agency/claw", "x", 0o700)
	if err := os.Symlink("claw", h.path("/opt/agency/openclaw")); err != nil {
		t.Fatal(err)
	}
	existing := &ExistingInstall{AgencyHome: "/opt/agency", BinaryPath: "/opt/agency/claw"}

	binDir, err := h.resolveBinDir(&TargetUser{HomeDir: "/home/alice", IsRoot: true}, "", existing)
	if err != nil || binDir != "/opt/agency/bin" {
		t.Fatalf("resolveBinDir() = %q, %v, want /opt/agency/bin", binDir, err)
	}
	if info, err := os.Stat(h.path(binDir)); err != nil || info.Mode().Perm() != config.DefaultDirMode {
		t.Errorf("/opt/agency/bin mode = %v, %v, want %v", info.Mode().Perm(), err, config.DefaultDirMode)
	}
	writeHostFile(t, h, filepath.Join(binDir, serviceName), "x", 0o700)
	if err := linkOpenClawAlias(h.path(binDir), serviceName); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(h.path("/opt/agency/bin/openclaw")); err != nil || target != "claw" {
		t.Errorf("/opt/agency/bin/openclaw -> %q, %v, want claw", target, err)
	}
	if unit := buildSystemUnit("alice", "alice", filepath.Join(binDir, serviceName), "/home/alice", binDir, "/opt/agency"); !strings.Contains(unit, "ExecStart=/opt/agency/bin/claw") {
		t.Errorf("unit does not start /opt/agency/bin/claw:\n%s", unit)
	}

	leftovers := h.leftoverOptAgencyFiles(binDir)
	if strings.Join(leftovers, " ") != "/opt/agency/claw /opt/agency/openclaw" {
		t.Errorf("leftoverOptAgencyFiles() = %q, want the old binary and alias", leftovers)
	}
	for _, p := range leftovers {
		if !h.exists(p) {
			t.Errorf("%s was removed, want it left in place", p)
		}
	}
	if got := h.leftoverOptAgencyFiles("/usr/local/bin"); got != nil {
		t.Errorf("leftoverOptAgencyFiles(/usr/local/bin) = %q, want none", got)
	}
}

func TestResolveAgencyHome(t *testing.T) {
	tempHome := t.TempDir()
	tu := &TargetUser{HomeDir: tempHome}

	// 1. Explicit AGENCY_HOME in env
	t.Setenv(global.EnvVarHome, "/custom/claw/home")
	got := fakeHost(t).resolveAgencyHome(tu, "/usr/local/bin", nil)
	if got != "/custom/claw/home" {
		t.Errorf("resolveAgencyHome with env = %q, want /custom/claw/home", got)
	}
	t.Setenv(global.EnvVarHome, "")

	// 2. Existing install with AgencyHome
	existing := &ExistingInstall{AgencyHome: "/opt/agency"}
	got = fakeHost(t).resolveAgencyHome(tu, "/usr/local/bin", existing)
	if got != "/opt/agency" {
		t.Errorf("resolveAgencyHome with existing.AgencyHome = %q, want /opt/agency", got)
	}

	// 3. Existing install with BinaryPath in /opt/agency
	existing = &ExistingInstall{BinaryPath: "/opt/agency/claw"}
	got = fakeHost(t).resolveAgencyHome(tu, "/usr/local/bin", existing)
	if got != "/opt/agency" {
		t.Errorf("resolveAgencyHome with existing.BinaryPath in /opt/agency = %q, want /opt/agency", got)
	}

	// 4. binDir == /opt/agency/bin
	got = fakeHost(t).resolveAgencyHome(tu, "/opt/agency/bin", nil)
	if got != "/opt/agency" {
		t.Errorf("resolveAgencyHome with binDir = /opt/agency/bin = %q, want /opt/agency", got)
	}
}

// The uninstaller names the system-wide data directory only for root or the
// account that owns it, never because of an account's name.
func TestUninstallDataDir(t *testing.T) {
	t.Setenv(global.EnvVarHome, "")
	systemDir := t.TempDir()
	me := strconv.Itoa(os.Getuid())
	home := t.TempDir()
	own := filepath.Join(home, global.DefaultDataDir)

	cases := []struct {
		name      string
		tu        TargetUser
		existing  *ExistingInstall
		systemDir string
		want      string
	}{
		{
			"detected install wins",
			TargetUser{Username: "alice", UID: me, HomeDir: home},
			&ExistingInstall{AgencyHome: "/srv/detected"}, systemDir, "/srv/detected",
		},
		{"owner of the system directory", TargetUser{Username: "alice", UID: me, HomeDir: home}, nil, systemDir, systemDir},
		{"root", TargetUser{Username: "root", UID: "0", HomeDir: home, IsRoot: true}, nil, systemDir, systemDir},
		{"another account", TargetUser{Username: "bob", UID: "4294967294", HomeDir: home}, nil, systemDir, own},
		{"no system directory", TargetUser{Username: "alice", UID: me, HomeDir: home}, nil, filepath.Join(systemDir, "missing"), own},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := uninstallDataDir(&tc.tu, tc.existing, tc.systemDir); got != tc.want {
				t.Errorf("uninstallDataDir = %q, want %q", got, tc.want)
			}
		})
	}
}
