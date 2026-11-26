package install

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agency/global"
)

// accountLookup makes e resolve accounts like a Linux machine where root
// runs the installer: "" is the invoking user, global.ServiceAccount exists
// once useradd has run (or from the start with exists), any other name is
// that account.
func accountLookup(e *installEnv, exists bool) {
	e.h.targetUser = func(explicit string) (*TargetUser, error) {
		e.lookups = append(e.lookups, explicit)
		if explicit == global.ServiceAccount && !exists {
			created := false
			for _, line := range e.log.lines() {
				created = created || strings.HasPrefix(line, "useradd ")
			}
			if !created {
				return nil, errors.New(`lookup user "agency": unknown user`)
			}
		}
		tu := *e.tu
		if explicit != "" {
			tu.Username = explicit
			tu.HomeDir = global.ServiceAccountHome
		}
		return &tu, nil
	}
}

// TestInstall_LinuxSystemCreatesServiceAccount: a new system install on Linux
// runs as the service account, created after the confirmation with the
// package's settings, never as the user who ran sudo, and leaves that
// account's shell alone.
func TestInstall_LinuxSystemCreatesServiceAccount(t *testing.T) {
	e := newInstallEnv(t, "linux", true)
	accountLookup(e, false)
	binDir := t.TempDir()
	out, err := e.install(t, "", 0, "", "", binDir)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	for _, want := range []string{
		"Run As User:     agency (system account, will be created)",
		"Home Directory:  /home/agency",
		"Created system account: agency",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	const useradd = "useradd --system --home-dir /home/agency --create-home --shell /bin/bash --user-group agency"
	if got := e.log.lines(); len(got) != 1 || got[0] != useradd {
		t.Errorf("commands = %q, want only %q", got, useradd)
	}
	if strings.Contains(out, "Run As User:     "+e.tu.Username+" ") {
		t.Errorf("the service runs as the invoking user:\n%s", out)
	}
	for _, rc := range []string{".bashrc", ".profile", ".zshrc"} {
		if _, err := os.Stat(filepath.Join(e.tu.HomeDir, rc)); err == nil {
			t.Errorf("%s was written for the invoking user", rc)
		}
	}
	if len(e.register) != 1 || e.register[0] != filepath.Join(binDir, "agency") {
		t.Errorf("registrations = %v", e.register)
	}
}

// TestInstall_LinuxSystemExistingAccount: an account that is already there,
// and a group without one, are used as they are.
func TestInstall_LinuxSystemExistingAccount(t *testing.T) {
	t.Run("account exists", func(t *testing.T) {
		e := newInstallEnv(t, "linux", true)
		accountLookup(e, true)
		out, err := e.install(t, "", 0, "", "", t.TempDir())
		if err != nil {
			t.Fatalf("install: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Run As User:     agency (UID: ") || strings.Contains(out, "Created system account") {
			t.Errorf("output:\n%s", out)
		}
		if got := e.log.lines(); len(got) != 0 {
			t.Errorf("commands = %q, want none", got)
		}
	})
	t.Run("group exists", func(t *testing.T) {
		e := newInstallEnv(t, "linux", true)
		accountLookup(e, false)
		e.h.commandOK = func(name string, args ...string) bool {
			return name == "getent" && strings.Join(args, " ") == "group agency"
		}
		if out, err := e.install(t, "", 0, "", "", t.TempDir()); err != nil {
			t.Fatalf("install: %v\n%s", err, out)
		}
		const useradd = "useradd --system --home-dir /home/agency --create-home --shell /bin/bash --gid agency agency"
		if got := e.log.lines(); len(got) != 1 || got[0] != useradd {
			t.Errorf("commands = %q, want only %q", got, useradd)
		}
	})
	t.Run("useradd fails", func(t *testing.T) {
		e := newInstallEnv(t, "linux", true)
		accountLookup(e, false)
		e.h.run = func(string, ...string) ([]byte, error) {
			return []byte("useradd: not found\n"), errors.New("exit status 127")
		}
		out, err := e.install(t, "", 0, "", "", t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "creating the agency account: useradd: useradd: not found") {
			t.Fatalf("err = %v\n%s", err, out)
		}
		if len(e.register) != 0 {
			t.Errorf("the service was registered: %v", e.register)
		}
	})
}

// TestInstall_LinuxSystemRefusesUser: --user no longer chooses the account of
// a system install on Linux; a user install and macOS are unaffected.
func TestInstall_LinuxSystemRefusesUser(t *testing.T) {
	e := newInstallEnv(t, "linux", true)
	out, err := e.install(t, "", 0, "", "bob", t.TempDir())
	if err == nil || err.Error() != "--user is not used on Linux: a system install runs as the agency account" {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if len(e.register) != 0 {
		t.Errorf("the service was registered: %v", e.register)
	}

	mac := newInstallEnv(t, "darwin", true)
	if out, err := mac.install(t, "", 0, "", "bob", t.TempDir()); err != nil || !strings.Contains(out, "Run As User:     bob") {
		t.Fatalf("darwin --user: %v\n%s", err, out)
	}
}

// TestServiceAccountPlaces: where a system install on Linux puts its data and
// binary, and the PATH its unit gets.
func TestServiceAccountPlaces(t *testing.T) {
	t.Setenv(global.EnvVarHome, "")
	acct := &TargetUser{Username: "agency", UID: "999", GID: "999", GroupName: "agency", HomeDir: global.ServiceAccountHome, IsRoot: true}
	h, _ := svcHost(t, "linux")

	if got := h.resolveAgencyHome(acct, "/usr/local/bin", nil); got != "/opt/agency" {
		t.Errorf("data directory = %q, want /opt/agency", got)
	}
	// A user's own install of that name is not the service account.
	own := *acct
	own.IsRoot = false
	own.HomeDir = "/home/agency"
	if got := h.resolveAgencyHome(&own, "/home/agency/bin", nil); got != "/home/agency/.agency" {
		t.Errorf("user install data directory = %q", got)
	}

	// /opt/agency left by an earlier install: the binary still goes where
	// other accounts can run it.
	left := &ExistingInstall{AgencyHome: "/opt/agency", User: "agency"}
	if got, err := h.resolveBinDir(acct, "", left); err != nil || got != "/usr/local/bin" {
		t.Errorf("bin dir = %q, %v, want /usr/local/bin", got, err)
	}
	if got, err := h.resolveBinDir(&own, "", left); err != nil || got != "/opt/agency/bin" {
		t.Errorf("user install bin dir = %q, %v, want /opt/agency/bin", got, err)
	}

	t.Setenv("PATH", "/home/carol/.nvm/bin:/usr/bin")
	var err error
	captureStdout(t, func() {
		if err = os.MkdirAll(filepath.Dir(h.path(systemUnitPath)), 0o755); err == nil {
			err = h.installSystemd(acct, "/usr/local/bin/agency", "/usr/local/bin", "/opt/agency")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(h.path(systemUnitPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"User=agency\n", "Group=agency\n", "WorkingDirectory=/home/agency\n", "Environment=HOME=/home/agency\n",
		"Environment=PATH=/home/agency/.local/bin:/home/agency/bin:/usr/local/bin:/usr/bin:/bin\n",
		"Environment=AGENCY_HOME=/opt/agency\n",
	} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if strings.Contains(string(unit), "carol") || strings.HasPrefix(string(unit), global.PackageUnitMarker) {
		t.Errorf("unit carries the installer's PATH or the package's marker:\n%s", unit)
	}
}

// TestDetect_NoInvokingHome: root without an invoking user has no user
// service to look for.
func TestDetect_NoInvokingHome(t *testing.T) {
	h, _ := svcHost(t, "linux")
	if got := h.detectInstall(""); got != nil {
		t.Errorf("detectInstall(\"\") = %+v, want nil", got)
	}
}

// TestInstall_LinuxSystemBinaryNotLeftInHome: a binary found only where the
// installer was started (an unpacked download in someone's home) is copied
// to the system bin folder, since the service account cannot run it there.
func TestInstall_LinuxSystemBinaryNotLeftInHome(t *testing.T) {
	acct := &TargetUser{Username: "agency", UID: "999", GID: "999", GroupName: "agency", HomeDir: global.ServiceAccountHome, IsRoot: true}
	h, _ := svcHost(t, "linux")
	writeHostFile(t, h, "/home/carol/agency", "binary", 0o755)
	fromDownload := &ExistingInstall{BinaryPath: "/home/carol/agency"}
	if got, err := h.resolveBinDir(acct, "", fromDownload); err != nil || got != "/usr/local/bin" {
		t.Errorf("bin dir = %q, %v, want /usr/local/bin", got, err)
	}
	// A registered service's binary keeps its place.
	writeHostFile(t, h, "/srv/tools/agency", "binary", 0o755)
	registered := &ExistingInstall{BinaryPath: "/srv/tools/agency", ServicePath: systemUnitPath, ServiceType: "systemd-system"}
	if got, err := h.resolveBinDir(acct, "", registered); err != nil || got != "/srv/tools" {
		t.Errorf("registered bin dir = %q, %v, want /srv/tools", got, err)
	}
	// Any other account's install is as before.
	carol := &TargetUser{Username: "carol", UID: "1001", GID: "1001", HomeDir: "/home/carol", IsRoot: true}
	if got, err := h.resolveBinDir(carol, "", fromDownload); err != nil || got != "/home/carol" {
		t.Errorf("carol's bin dir = %q, %v, want /home/carol", got, err)
	}
}

// TestInstall_LinuxSystemForeignDataDir: a data directory the service account
// does not own is refused before anything is changed, with the way out.
func TestInstall_LinuxSystemForeignDataDir(t *testing.T) {
	for _, tc := range []struct {
		name    string
		exists  bool
		uid     string
		want    string
		refused bool
	}{
		{"account missing", false, "", "Move it away or remove it, then run the installer again", true},
		{"another owner", true, "4294967294", "give it to that account (sudo chown -R agency: ", true},
		{"its own", true, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newInstallEnv(t, "linux", true)
			if tc.uid != "" {
				e.tu.UID = tc.uid
			}
			accountLookup(e, tc.exists)
			if err := os.MkdirAll(e.agencyHome, 0o700); err != nil {
				t.Fatal(err)
			}
			out, err := e.install(t, "", 0, "", "", t.TempDir())
			if !tc.refused {
				if err != nil {
					t.Fatalf("install: %v\n%s", err, out)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "does not belong to the agency account") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v\n%s", err, out)
			}
			if len(e.register) != 0 || len(e.log.lines()) != 0 || strings.Contains(out, "Installation Summary") {
				t.Errorf("something was done before the refusal: %v %v\n%s", e.register, e.log.lines(), out)
			}
		})
	}
}
