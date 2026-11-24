package install

import (
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSystemdUnit(t *testing.T) {
	h := fakeHost(t)
	unitFile := "/etc/systemd/system/claw.service"
	unitContent := `[Unit]
Description=ClawEh — Personal AI Assistant
After=network-online.target

[Service]
Type=simple
User=clawuser
ExecStart=/opt/agency/claw --some-flag
Environment=AGENCY_HOME=/opt/agency
Restart=on-failure

[Install]
WantedBy=multi-user.target
`
	writeHostFile(t, h, unitFile, unitContent, 0o644)

	inst := h.parseSystemdUnit(unitFile, "systemd-system")
	if inst.BinaryPath != "/opt/agency/claw" {
		t.Errorf("BinaryPath = %q, want /opt/agency/claw", inst.BinaryPath)
	}
	if inst.User != "clawuser" {
		t.Errorf("User = %q, want clawuser", inst.User)
	}
	if inst.AgencyHome != "/opt/agency" {
		t.Errorf("AgencyHome = %q, want /opt/agency", inst.AgencyHome)
	}
	if inst.ServiceType != "systemd-system" {
		t.Errorf("ServiceType = %q, want systemd-system", inst.ServiceType)
	}
}

func TestParseSystemdUnit_QuotedAndInferred(t *testing.T) {
	h := fakeHost(t)

	// 1. Quoted Environment="AGENCY_HOME=/opt/agency"
	unit1 := "/units/quoted.service"
	content1 := `[Unit]
Description=ClawEh

[Service]
ExecStart=/opt/agency/claw
Environment="AGENCY_HOME=/opt/agency"
`
	writeHostFile(t, h, unit1, content1, 0o644)
	inst1 := h.parseSystemdUnit(unit1, "systemd-system")
	if inst1.AgencyHome != "/opt/agency" {
		t.Errorf("quoted AGENCY_HOME: got %q, want /opt/agency", inst1.AgencyHome)
	}

	// 2. No AGENCY_HOME env var, but ExecStart=/opt/agency/claw
	unit2 := "/units/inferred.service"
	content2 := `[Unit]
Description=ClawEh

[Service]
ExecStart=/opt/agency/claw
Environment=PATH=/opt/agency:/usr/bin
`
	writeHostFile(t, h, unit2, content2, 0o644)
	inst2 := h.parseSystemdUnit(unit2, "systemd-system")
	if inst2.AgencyHome != "/opt/agency" {
		t.Errorf("inferred AGENCY_HOME: got %q, want /opt/agency", inst2.AgencyHome)
	}
}

func TestParseLaunchdPlist(t *testing.T) {
	h := fakeHost(t)
	plistFile := systemLaunchdPath
	plistContent := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.pivotllm.claweh</string>
	<key>UserName</key>
	<string>macclaw</string>
	<key>ProgramArguments</key>
	<array>
		<string>/opt/agency/bin/claw</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>AGENCY_HOME</key>
		<string>/opt/agency</string>
	</dict>
</dict>
</plist>
`
	writeHostFile(t, h, plistFile, plistContent, 0o644)

	inst := h.parseLaunchdPlist(plistFile, "launchd-daemon")
	if inst.BinaryPath != "/opt/agency/bin/claw" {
		t.Errorf("BinaryPath = %q, want /opt/agency/bin/claw", inst.BinaryPath)
	}
	if inst.User != "macclaw" {
		t.Errorf("User = %q, want macclaw", inst.User)
	}
	if inst.AgencyHome != "/opt/agency" {
		t.Errorf("AgencyHome = %q, want /opt/agency", inst.AgencyHome)
	}
}

func TestIsBuildOrTempPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/home/user/claw/build/claw", true},
		{"/tmp/claw", true},
		{"/tmp/claw-upgrade-123/claw", true},
		{"/var/tmp/claw", true},
		{"/opt/agency/claw", false},
		{"/opt/agency", false},
		{"/usr/local/bin/claw", false},
		{"/home/user/.local/bin/claw", false},
		{"/home/user/bin/claw", false},
	}

	for _, tc := range cases {
		got := isBuildOrTempPath(tc.path)
		if got != tc.want {
			t.Errorf("isBuildOrTempPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// fakeHost is a host rooted in a temp dir whose commands all fail, so a test
// never reads the real /opt/agency, /etc/systemd or home directory, nor finds
// the test binary or a claw on PATH.
func fakeHost(t *testing.T) *host {
	t.Helper()
	return &host{
		root:       t.TempDir(),
		executable: func() (string, error) { return "", errors.New("no executable in tests") },
		lookPath:   func(string) (string, error) { return "", exec.ErrNotFound },
		commandOK:  func(string, ...string) bool { return false },
		run:        func(string, ...string) ([]byte, error) { return nil, errors.New("no commands in tests") },
	}
}

// writeHostFile writes content at the host path p, creating its folders.
func writeHostFile(t *testing.T, h *host, p, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(h.path(p)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.path(p), []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// currentOwner is the name ownerName reports for files the test creates: the
// test's account, or "" when the tests run as root.
func currentOwner(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if u.Username == "root" {
		return ""
	}
	return u.Username
}

// An /opt/agency install is found by its data directory; its binary is
// /opt/agency/bin/claw, else the older /opt/agency/claw, and the folder's owner is
// the account the service runs as.
func TestDetectLinuxInstall_OptAgency(t *testing.T) {
	cases := []struct {
		name    string
		files   []string
		wantBin string
	}{
		{"bin only", []string{"/opt/agency/bin/claw"}, "/opt/agency/bin/claw"},
		{"old location only", []string{"/opt/agency/claw"}, "/opt/agency/claw"},
		{"both", []string{"/opt/agency/claw", "/opt/agency/bin/claw"}, "/opt/agency/bin/claw"},
		{"no binary", []string{"/opt/agency/config.json"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := fakeHost(t)
			for _, f := range tc.files {
				writeHostFile(t, h, f, "x", 0o700)
			}
			inst := h.detectLinuxInstall("/home/alice")
			if inst == nil {
				t.Fatal("detectLinuxInstall() = nil, want the /opt/agency install")
			}
			if inst.AgencyHome != "/opt/agency" {
				t.Errorf("AgencyHome = %q, want /opt/agency", inst.AgencyHome)
			}
			if inst.BinaryPath != tc.wantBin {
				t.Errorf("BinaryPath = %q, want %q", inst.BinaryPath, tc.wantBin)
			}
			if want := currentOwner(t); inst.User != want {
				t.Errorf("User = %q, want %q (the owner of /opt/agency)", inst.User, want)
			}
			if inst.ServicePath != "" {
				t.Errorf("ServicePath = %q, want none", inst.ServicePath)
			}
		})
	}
}

// A system unit wins over everything else and gives the binary, account and
// data directory; whether it is active comes from systemctl.
func TestDetectLinuxInstall_SystemUnit(t *testing.T) {
	h := fakeHost(t)
	writeHostFile(t, h, systemUnitPath, "[Service]\nUser=alice\nExecStart=/opt/agency/bin/claw\n", 0o644)
	writeHostFile(t, h, "/opt/agency/claw", "x", 0o700)
	var asked []string
	h.commandOK = func(name string, args ...string) bool {
		asked = append([]string{name}, args...)
		return true
	}

	inst := h.detectLinuxInstall("/home/alice")
	if inst == nil {
		t.Fatal("detectLinuxInstall() = nil, want the system service")
	}
	if inst.ServiceType != "systemd-system" || inst.ServicePath != systemUnitPath {
		t.Errorf("service = %q at %q, want systemd-system at %s", inst.ServiceType, inst.ServicePath, systemUnitPath)
	}
	if inst.BinaryPath != "/opt/agency/bin/claw" {
		t.Errorf("BinaryPath = %q, want the unit's ExecStart /opt/agency/bin/claw", inst.BinaryPath)
	}
	if inst.User != "alice" || inst.AgencyHome != "/opt/agency" {
		t.Errorf("User, AgencyHome = %q, %q, want alice, /opt/agency", inst.User, inst.AgencyHome)
	}
	if !inst.IsActive || strings.Join(asked, " ") != "systemctl is-active --quiet claw" {
		t.Errorf("IsActive = %v from %q, want true from systemctl is-active", inst.IsActive, asked)
	}
}

// A user unit is found under the account's home; with no AGENCY_HOME in it the
// data directory is inferred from the binary.
func TestDetectLinuxInstall_UserUnit(t *testing.T) {
	h := fakeHost(t)
	writeHostFile(t, h, userUnitPath("/home/alice"), "[Service]\nExecStart=/opt/agency/bin/claw\n", 0o644)

	inst := h.detectLinuxInstall("/home/alice")
	if inst == nil || inst.ServiceType != "systemd-user" {
		t.Fatalf("detectLinuxInstall() = %+v, want the user service", inst)
	}
	if inst.BinaryPath != "/opt/agency/bin/claw" || inst.AgencyHome != "/opt/agency" {
		t.Errorf("BinaryPath, AgencyHome = %q, %q, want /opt/agency/bin/claw, /opt/agency", inst.BinaryPath, inst.AgencyHome)
	}
	if inst.IsActive {
		t.Error("IsActive = true, want false (systemctl reports it inactive)")
	}
}

// With no service, no /opt/agency and no installed binary, nothing is found.
func TestDetectLinuxInstall_Nothing(t *testing.T) {
	if inst := fakeHost(t).detectLinuxInstall("/home/alice"); inst != nil {
		t.Errorf("detectLinuxInstall() = %+v, want nil", inst)
	}
}
