package install

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const plistDaemon = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>ProgramArguments</key>
	<array>
		<string>/usr/local/bin/agency</string>
		<string>gateway</string>
	</array>
	<key>UserName</key>
	<string>alice</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>AGENCY_HOME</key>
		<string>/Users/alice/.agency</string>
	</dict>
</dict>
</plist>
`

// The macOS detection order: LaunchDaemon, then the user's LaunchAgent, then
// /opt/agency, then nothing.
func TestDetectDarwinInstall_Matrix(t *testing.T) {
	cases := []struct {
		name     string
		files    map[string]string
		wantType string
		wantPath string
		wantBin  string
		wantHome string
		wantUser string
		wantNil  bool
	}{
		{
			name:     "launch daemon",
			files:    map[string]string{systemLaunchdPath: plistDaemon, userLaunchdPath("/Users/alice"): plistDaemon},
			wantType: "launchd-daemon", wantPath: systemLaunchdPath,
			wantBin: "/usr/local/bin/agency", wantHome: "/Users/alice/.agency", wantUser: "alice",
		},
		{
			name:     "launch agent",
			files:    map[string]string{userLaunchdPath("/Users/alice"): plistDaemon},
			wantType: "launchd-agent", wantPath: userLaunchdPath("/Users/alice"),
			wantBin: "/usr/local/bin/agency", wantHome: "/Users/alice/.agency", wantUser: "alice",
		},
		{
			name:    "opt agency bin layout",
			files:   map[string]string{"/opt/agency/bin/agency": "x"},
			wantBin: "/opt/agency/bin/agency", wantHome: "/opt/agency",
		},
		{name: "nothing", wantNil: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := fakeHost(t)
			for p, body := range tc.files {
				writeHostFile(t, h, p, body, 0o644)
			}
			inst := h.detectDarwinInstall("/Users/alice")
			if tc.wantNil {
				if inst != nil {
					t.Fatalf("detectDarwinInstall() = %+v, want nil", inst)
				}
				return
			}
			if inst == nil {
				t.Fatal("detectDarwinInstall() = nil")
			}
			if inst.ServiceType != tc.wantType || inst.ServicePath != tc.wantPath {
				t.Errorf("service = %q at %q, want %q at %q", inst.ServiceType, inst.ServicePath, tc.wantType, tc.wantPath)
			}
			if inst.BinaryPath != tc.wantBin || inst.AgencyHome != tc.wantHome || inst.User != tc.wantUser {
				t.Errorf("bin, home, user = %q, %q, %q, want %q, %q, %q",
					inst.BinaryPath, inst.AgencyHome, inst.User, tc.wantBin, tc.wantHome, tc.wantUser)
			}
		})
	}
}

// A plist that cannot be read, or that lacks the keys, gives only what is
// known; the data directory is inferred from the binary or /opt/agency.
func TestParseLaunchdPlist_MissingAndGarbled(t *testing.T) {
	h := fakeHost(t)
	inst := h.parseLaunchdPlist("/Library/LaunchDaemons/none.plist", "launchd-daemon")
	if inst.ServicePath != "/Library/LaunchDaemons/none.plist" || inst.BinaryPath != "" || inst.AgencyHome != "" {
		t.Errorf("missing plist = %+v, want only the service path", inst)
	}

	// Keys with no <string> after them, and an unterminated string.
	writeHostFile(t, h, "/p/garbled.plist", "<key>ProgramArguments</key><array>\n<key>UserName</key><string>bob", 0o644)
	inst = h.parseLaunchdPlist("/p/garbled.plist", "launchd-agent")
	if inst.BinaryPath != "" || inst.User != "" || inst.AgencyHome != "" {
		t.Errorf("garbled plist = %+v, want nothing parsed", inst)
	}

	// Binary in /opt/agency: data directory /opt/agency.
	writeHostFile(t, h, "/p/opt.plist", "<key>ProgramArguments</key><array><string>/opt/agency/bin/agency</string></array>", 0o644)
	if inst = h.parseLaunchdPlist("/p/opt.plist", "launchd-daemon"); inst.AgencyHome != "/opt/agency" {
		t.Errorf("AgencyHome = %q, want /opt/agency from the binary", inst.AgencyHome)
	}

	// Binary in a sibling folder: not an /opt/agency install.
	writeHostFile(t, h, "/p/sib.plist", "<key>ProgramArguments</key><array><string>/opt/agency-old/agency</string></array>", 0o644)
	if inst = h.parseLaunchdPlist("/p/sib.plist", "launchd-daemon"); inst.AgencyHome != "" {
		t.Errorf("AgencyHome = %q for /opt/agency-old/agency, want none", inst.AgencyHome)
	}

	// Binary elsewhere, but /opt/agency exists.
	writeHostFile(t, h, "/opt/agency/config.json", "{}", 0o600)
	if inst = h.parseLaunchdPlist("/p/sib.plist", "launchd-daemon"); inst.AgencyHome != "/opt/agency" {
		t.Errorf("AgencyHome = %q with /opt/agency present, want /opt/agency", inst.AgencyHome)
	}
}

// Every way a unit names, or lets the installer infer, its data directory
// and account.
func TestParseSystemdUnit_Inference(t *testing.T) {
	owner := ""
	cases := []struct {
		name      string
		unit      string
		optAgency bool
		wantHome  string
		wantUser  func(t *testing.T) string
	}{
		{name: "unterminated quote", unit: "Environment=AGENCY_HOME=\"/srv/agency\nExecStart=/usr/bin/agency\n", wantHome: "/srv/agency"},
		{name: "quoted value", unit: "Environment=AGENCY_HOME=\"/srv/my agency\" X=1\n", wantHome: "/srv/my agency"},
		{name: "single quoted", unit: "Environment='AGENCY_HOME=/srv/agency' 'X=1'\n", wantHome: "/srv/agency"},
		{name: "bare with others", unit: "Environment=AGENCY_HOME=/srv/agency OTHER=1\n", wantHome: "/srv/agency"},
		{name: "working directory", unit: "WorkingDirectory=\"/srv/data\"\nExecStart=/usr/bin/agency\n", wantHome: "/srv/data"},
		{name: "working directory root ignored", unit: "WorkingDirectory=/\nExecStart=/usr/bin/agency\n", wantHome: ""},
		{name: "working directory in home ignored", unit: "WorkingDirectory=/home/alice\nExecStart=/usr/bin/agency\n", wantHome: ""},
		{name: "PATH lists /opt/agency/bin", unit: "Environment=\"PATH=/usr/bin:/opt/agency/bin\"\nExecStart=/usr/bin/agency\n", wantHome: "/opt/agency"},
		{name: "PATH lists a sibling only", unit: "Environment=PATH=/usr/bin:/opt/agency-old/bin\nExecStart=/usr/bin/agency\n", wantHome: ""},
		{name: "binary in sibling", unit: "ExecStart=/opt/agencydev/agency gateway\n", wantHome: ""},
		{
			name: "opt agency dir exists", unit: "ExecStart=/usr/bin/agency\n", optAgency: true, wantHome: "/opt/agency",
			wantUser: currentOwner,
		},
		{name: "empty unit", unit: "", wantHome: ""},
		{name: "garbled unit", unit: "\x00\x01not a unit\nExecStart=\n", wantHome: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := fakeHost(t)
			writeHostFile(t, h, systemUnitPath, tc.unit, 0o644)
			if tc.optAgency {
				if err := os.MkdirAll(h.path("/opt/agency"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			inst := h.parseSystemdUnit(systemUnitPath, "systemd-system")
			if inst.AgencyHome != tc.wantHome {
				t.Errorf("AgencyHome = %q, want %q", inst.AgencyHome, tc.wantHome)
			}
			want := owner
			if tc.wantUser != nil {
				want = tc.wantUser(t)
			}
			if inst.User != want {
				t.Errorf("User = %q, want %q", inst.User, want)
			}
		})
	}
}

// With no User= and no data directory, the account is the binary's owner.
func TestParseSystemdUnit_UserFromBinaryOwner(t *testing.T) {
	h := fakeHost(t)
	// The binary path is the host's own; ownerName looks it up under the root.
	writeHostFile(t, h, "/usr/local/bin/agency", "x", 0o755)
	writeHostFile(t, h, systemUnitPath, "ExecStart=/usr/local/bin/agency gateway\n", 0o644)
	inst := h.parseSystemdUnit(systemUnitPath, "systemd-system")
	if want := currentOwner(t); inst.User != want {
		t.Errorf("User = %q, want %q (the binary's owner)", inst.User, want)
	}
}

// A unit that cannot be opened gives only the service path and type.
func TestParseSystemdUnit_Unreadable(t *testing.T) {
	h := fakeHost(t)
	inst := h.parseSystemdUnit("/etc/systemd/system/missing.service", "systemd-system")
	if inst.ServicePath != "/etc/systemd/system/missing.service" || inst.ServiceType != "systemd-system" ||
		inst.BinaryPath != "" || inst.User != "" || inst.AgencyHome != "" {
		t.Errorf("parseSystemdUnit(missing) = %+v", inst)
	}
}

// ownerName gives no account for a missing path or a root-owned one.
func TestOwnerName_MissingAndRoot(t *testing.T) {
	h := fakeHost(t)
	if got := h.ownerName("/does/not/exist"); got != "" {
		t.Errorf("ownerName(missing) = %q, want none", got)
	}
	system := &host{}
	if got := system.ownerName("/"); got != "" {
		t.Errorf("ownerName(/) = %q, want none (root)", got)
	}
}

// detectFromBinary takes the running executable, else agency on PATH, and
// skips a path that does not resolve or is a build or temporary location.
func TestDetectFromBinary(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal("no sh on PATH")
	}
	resolvedSh, err := filepath.EvalSymlinks(sh)
	if err != nil || isBuildOrTempPath(resolvedSh) {
		t.Fatalf("sh resolves to %q, not an installed location", resolvedSh)
	}
	tempBin := filepath.Join(t.TempDir(), "agency")
	if err := os.WriteFile(tempBin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	fail := func() (string, error) { return "", errors.New("none") }
	cases := []struct {
		name string
		exe  func() (string, error)
		look func(string) (string, error)
		want string
	}{
		{"executable", func() (string, error) { return sh, nil }, func(string) (string, error) { return "", exec.ErrNotFound }, resolvedSh},
		{"executable fails, PATH used", fail, func(string) (string, error) { return sh, nil }, resolvedSh},
		{
			"temporary executable skipped, PATH used", func() (string, error) { return tempBin, nil },
			func(string) (string, error) { return sh, nil }, resolvedSh,
		},
		{
			"missing executable skipped", func() (string, error) { return "/no/such/agency", nil },
			func(string) (string, error) { return "", exec.ErrNotFound }, "",
		},
		{
			"temporary only", func() (string, error) { return tempBin, nil },
			func(string) (string, error) { return tempBin, nil }, "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := fakeHost(t)
			h.executable, h.lookPath = tc.exe, tc.look
			inst := h.detectFromBinary()
			if tc.want == "" {
				if inst != nil {
					t.Fatalf("detectFromBinary() = %+v, want nil", inst)
				}
				return
			}
			if inst == nil || inst.BinaryPath != tc.want || inst.AgencyHome != "" {
				t.Fatalf("detectFromBinary() = %+v, want binary %q and no data directory", inst, tc.want)
			}
		})
	}
}

// inOptAgency matches /opt/agency and what is inside it, never a sibling.
func TestInOptAgency(t *testing.T) {
	for p, want := range map[string]bool{
		"/opt/agency":             true,
		"/opt/agency/bin/agency":  true,
		"/opt/agency/agency":      true,
		"/opt/agency-old/agency":  false,
		"/opt/agencydev":          false,
		"/opt":                    false,
		"":                        false,
		"/usr/local/bin/agency":   false,
		"/srv/opt/agency/agency":  false,
		"/opt/agency/../x/agency": true, // lexical, as the unit writes it
		"/opt/agency_backup/bin":  false,
	} {
		if got := inOptAgency(p); got != want {
			t.Errorf("inOptAgency(%q) = %v, want %v", p, got, want)
		}
	}
}

// The linux detector falls back to the binary when there is no service and
// no /opt/agency.
func TestDetectLinuxInstall_FallsBackToBinary(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal("no sh on PATH")
	}
	resolvedSh, err := filepath.EvalSymlinks(sh)
	if err != nil || isBuildOrTempPath(resolvedSh) {
		t.Fatalf("sh resolves to %q, not an installed location", resolvedSh)
	}
	h := fakeHost(t)
	h.lookPath = func(string) (string, error) { return sh, nil }
	inst := h.detectLinuxInstall("/home/alice")
	if inst == nil || inst.BinaryPath != resolvedSh || inst.ServiceType != "" {
		t.Fatalf("detectLinuxInstall() = %+v, want the binary on PATH", inst)
	}
}

// A user unit wins over /opt/agency; its activity comes from systemctl --user.
func TestDetectLinuxInstall_UserUnitBeatsOptAgency(t *testing.T) {
	h := fakeHost(t)
	writeHostFile(t, h, userUnitPath("/home/alice"), "Environment=AGENCY_HOME=/home/alice/.agency\nExecStart=/home/alice/bin/agency\n", 0o644)
	writeHostFile(t, h, "/opt/agency/bin/agency", "x", 0o700)
	var asked []string
	h.commandOK = func(name string, args ...string) bool {
		asked = append([]string{name}, args...)
		return true
	}
	inst := h.detectLinuxInstall("/home/alice")
	if inst == nil || inst.ServiceType != "systemd-user" || inst.AgencyHome != "/home/alice/.agency" {
		t.Fatalf("detectLinuxInstall() = %+v, want the user service", inst)
	}
	if !inst.IsActive || len(asked) < 2 || asked[1] != "--user" {
		t.Errorf("IsActive = %v from %q, want true from systemctl --user", inst.IsActive, asked)
	}
}
