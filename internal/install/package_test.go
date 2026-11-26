package install

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agency/global"
	"go.bison9.com/agency/internal/admin"
	"go.bison9.com/agency/internal/testbinary"
)

// serviceAccount is the account the package creates, as resolveTargetUser
// reports it when run as root.
var serviceAccount = &TargetUser{Username: "agency", UID: "998", GID: "998", GroupName: "agency", HomeDir: "/home/agency", IsRoot: true}

// packageHost is a Linux machine as the package's postinst sees it: running
// as root, /usr/bin/agency installed, systemd running when withSystemd.
func packageHost(t *testing.T, withSystemd bool) (*host, *cmdLog) {
	t.Helper()
	testbinary.Set(t, "agency")
	h, log := svcHost(t, "linux")
	h.detect = h.detectInstall
	h.executable = func() (string, error) { return "/usr/bin/agency", nil }
	h.targetUser = func(explicit string) (*TargetUser, error) {
		if explicit != global.ServiceAccount {
			return nil, errors.New("unexpected account " + explicit)
		}
		tu := *serviceAccount
		return &tu, nil
	}
	if withSystemd {
		h.lookPath = func(file string) (string, error) {
			if file == "systemctl" {
				return "/usr/bin/systemctl", nil
			}
			return "", errors.New("not found")
		}
		mkHostDir(t, h, systemdRunDir)
	}
	mkHostDir(t, h, "/etc/systemd/system")
	t.Setenv(global.EnvVarHome, "")
	orig := geteuid
	geteuid = func() int { return 0 }
	t.Cleanup(func() { geteuid = orig })
	return h, log
}

func runPackageInstall(t *testing.T, h *host) (string, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() { err = h.installPackage(true) })
	return out, err
}

// TestInstallPackage_Fresh: the package's install writes the marked unit for
// /usr/bin/agency running as agency with AGENCY_HOME=/opt/agency and the
// hardening of an interactive install, starts it without an admin account,
// and says how to create one.
func TestInstallPackage_Fresh(t *testing.T) {
	h, log := packageHost(t, true)
	out, err := runPackageInstall(t, h)
	if err != nil {
		t.Fatalf("install --package: %v\n%s", err, out)
	}
	unit := readHostFile(t, h, systemUnitPath)
	if !strings.HasPrefix(unit, global.PackageUnitMarker+"\n[Unit]\n") {
		t.Errorf("unit does not start with the marker:\n%s", unit)
	}
	for _, want := range []string{
		"User=agency\n", "Group=agency\n", "WorkingDirectory=/home/agency\n",
		"ExecStart=/usr/bin/agency\n", "Restart=on-failure\n", "KillMode=mixed\n",
		"NoNewPrivileges=yes\n", "PrivateTmp=yes\n", "Environment=HOME=/home/agency\n",
		"Environment=PATH=/home/agency/.local/bin:/home/agency/bin:/usr/local/bin:/usr/bin:/bin\n",
		"Environment=AGENCY_HOME=/opt/agency\n", "WantedBy=multi-user.target\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if want := buildPackageUnit(serviceAccount, "/usr/bin/agency", "/usr/bin", "/opt/agency"); unit != want {
		t.Errorf("unit = %q, want %q", unit, want)
	}
	if got, want := joinLines(log.lines()), "systemctl daemon-reload\nsystemctl enable agency\nsystemctl restart agency"; got != want {
		t.Errorf("commands:\n%s\nwant:\n%s", got, want)
	}
	// The message, in order: what happened, the account and the files, where
	// the WebUI is, and last what to do next.
	at := 0
	for _, want := range []string{
		"Agency is installed and running.\n\n",
		"Service account:        agency (created)\n",
		"Program:                /usr/bin/agency\n",
		"Data and settings:      /opt/agency\n",
		"Service:                /etc/systemd/system/agency.service\n",
		"WebUI on this machine:  http://127.0.0.1:18790/\n",
		"Allowed networks:       localhost only\n",
		"\nNext steps:\n\n  1. Create the admin account:\n       sudo agency admin\n\n" +
			"  2. To use the WebUI from another computer, allow it, for example:\n" +
			"       sudo agency network 192.168.1.0/24    (one network)\n" +
			"       sudo agency network 192.168.1.50      (one computer)\n" +
			"       sudo agency network                   (all private networks)\n\n",
	} {
		i := strings.Index(out[at:], want)
		if i < 0 {
			t.Fatalf("output lacks %q after offset %d:\n%s", want, at, out)
		}
		at += i + len(want)
	}
	if at != len(out) {
		t.Errorf("the next steps are not last:\n%s", out)
	}
	if _, serr := os.Stat(h.path(admin.Path(global.SystemDataDir))); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("an admin account was created: %v", serr)
	}
}

// TestInstallPackage_ExistingAccountAndNetwork: an account the package found
// is reported as such, and with a network already allowed the only step left
// is the admin account.
func TestInstallPackage_ExistingAccountAndNetwork(t *testing.T) {
	h, _ := packageHost(t, true)
	if _, err := runPackageInstall(t, h); err != nil { // the unit marks the install as the package's
		t.Fatal(err)
	}
	writeHostFile(t, h, filepath.Join(global.SystemDataDir, "config.json"),
		`{"gateway":{"allowed_cidrs":["192.168.1.0/24"]}}`, 0o600)
	var err error
	out := captureStdout(t, func() { err = h.installPackage(false) })
	if err != nil {
		t.Fatalf("install --package: %v\n%s", err, out)
	}
	for _, want := range []string{
		"Service account:        agency (already existed)\n",
		"Allowed networks:       192.168.1.0/24\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.HasSuffix(out, "\n\nNext steps:\n\n  1. Create the admin account:\n       sudo agency admin\n\n") {
		t.Errorf("the admin account is not the last and only step:\n%s", out)
	}
}

// TestInstallPackage_Rerun: running it again (an upgrade, a retried
// configure) rewrites the same unit and restarts the service; with the admin
// account in place it prints only that it is running.
func TestInstallPackage_Rerun(t *testing.T) {
	h, log := packageHost(t, true)
	if _, err := runPackageInstall(t, h); err != nil {
		t.Fatal(err)
	}
	first := readHostFile(t, h, systemUnitPath)
	writeHostFile(t, h, admin.Path(global.SystemDataDir), "{}", 0o600)
	out, err := runPackageInstall(t, h)
	if err != nil {
		t.Fatal(err)
	}
	if got := readHostFile(t, h, systemUnitPath); got != first {
		t.Errorf("unit changed on a re-run:\n%s\nwas:\n%s", got, first)
	}
	if out != "Agency is installed and running.\n" {
		t.Errorf("output = %q", out)
	}
	if n := strings.Count(joinLines(log.lines()), "systemctl restart agency"); n != 2 {
		t.Errorf("restarts = %d, want 2: %v", n, log.lines())
	}
}

// TestInstallPackage_NoSystemd: without a running systemd the unit is
// written and nothing is started.
func TestInstallPackage_NoSystemd(t *testing.T) {
	h, log := packageHost(t, false)
	out, err := runPackageInstall(t, h)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(readHostFile(t, h, systemUnitPath), global.PackageUnitMarker) {
		t.Error("unit not written")
	}
	if len(log.lines()) != 0 {
		t.Errorf("commands run without systemd: %v", log.lines())
	}
	if !strings.Contains(out, "systemd is not running, so the service was not started.") {
		t.Errorf("output = %q", out)
	}
}

// TestInstallPackage_LeavesExistingInstall: a system unit the package did not
// write, or an /opt/agency owned by another account, is someone else's
// install: nothing is written or run, and one line says so.
func TestInstallPackage_LeavesExistingInstall(t *testing.T) {
	t.Run("unit", func(t *testing.T) {
		h, log := packageHost(t, true)
		manual := "[Service]\nUser=alice\nExecStart=/usr/local/bin/agency\nEnvironment=AGENCY_HOME=/home/alice/.agency\n"
		writeHostFile(t, h, systemUnitPath, manual, 0o644)
		out, err := runPackageInstall(t, h)
		if err != nil {
			t.Fatal(err)
		}
		if got := readHostFile(t, h, systemUnitPath); got != manual {
			t.Errorf("unit changed:\n%s", got)
		}
		if len(log.lines()) != 0 {
			t.Errorf("commands: %v", log.lines())
		}
		if want := "An existing Agency install was found and left unchanged (" + systemUnitPath + ").\n"; out != want {
			t.Errorf("output = %q, want %q", out, want)
		}
	})
	t.Run("data directory of another account", func(t *testing.T) {
		if currentOwner(t) == "" {
			t.Skip("running as root: the temp data directory belongs to root")
		}
		h, log := packageHost(t, true)
		mkHostDir(t, h, global.SystemDataDir) // owned by the test's account, not agency
		out, err := runPackageInstall(t, h)
		if err != nil {
			t.Fatal(err)
		}
		if h.fileExists(systemUnitPath) || len(log.lines()) != 0 {
			t.Errorf("install went ahead: %v", log.lines())
		}
		if want := "An existing Agency install was found and left unchanged (" + global.SystemDataDir + ").\n"; out != want {
			t.Errorf("output = %q, want %q", out, want)
		}
	})
}

// TestInstallPackage_Refusals: only root on Linux.
func TestInstallPackage_Refusals(t *testing.T) {
	h, _ := packageHost(t, true)
	geteuid = func() int { return 1000 }
	if _, err := runPackageInstall(t, h); err == nil || !strings.Contains(err.Error(), "must run as root") {
		t.Errorf("non-root err = %v", err)
	}
	h.goos = "darwin"
	if _, err := runPackageInstall(t, h); err == nil || !strings.Contains(err.Error(), "only for the Debian package") {
		t.Errorf("darwin err = %v", err)
	}
}

// TestInteractive_OnPackageInstall: `install` and `uninstall` on an install
// the package set up point at apt and change nothing.
func TestInteractive_OnPackageInstall(t *testing.T) {
	h, log := packageHost(t, true)
	if _, err := runPackageInstall(t, h); err != nil {
		t.Fatal(err)
	}
	before := len(log.lines())
	unit := readHostFile(t, h, systemUnitPath)
	h.targetUser = func(string) (*TargetUser, error) { tu := *aliceRoot; return &tu, nil }
	var err error
	captureStdout(t, func() { err = h.install("", 0, "", "", "", true) })
	if err == nil || !strings.Contains(err.Error(), "set up by the agency package") || !strings.Contains(err.Error(), "sudo apt remove agency") {
		t.Errorf("install err = %v", err)
	}
	captureStdout(t, func() { err = h.uninstall("", true) })
	if err == nil || !strings.Contains(err.Error(), "sudo apt remove agency") {
		t.Errorf("uninstall err = %v", err)
	}
	if readHostFile(t, h, systemUnitPath) != unit || len(log.lines()) != before {
		t.Errorf("the package's install was changed: %v", log.lines()[before:])
	}
}

// TestUninstallPackage: the prerm's uninstall removes the package's unit and
// a unit that runs the package's binary, and leaves any other install alone.
func TestUninstallPackage(t *testing.T) {
	cases := []struct {
		name    string
		unit    string // "" for none
		removed bool
	}{
		{"package unit", global.PackageUnitMarker + "\n[Service]\nUser=agency\nExecStart=/usr/bin/agency\n", true},
		{"manual unit on the package binary", "[Service]\nUser=alice\nExecStart=/usr/bin/agency\n", true},
		{"manual unit on its own binary", "[Service]\nUser=alice\nExecStart=/usr/local/bin/agency\n", false},
		{"no unit", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, log := packageHost(t, true)
			if tc.unit != "" {
				writeHostFile(t, h, systemUnitPath, tc.unit, 0o644)
			}
			var err error
			captureStdout(t, func() { err = h.uninstallPackage() })
			if err != nil {
				t.Fatal(err)
			}
			gone := !h.fileExists(systemUnitPath)
			if tc.unit != "" && gone != tc.removed {
				t.Errorf("unit removed = %v, want %v", gone, tc.removed)
			}
			wantCmds := ""
			if tc.removed {
				wantCmds = "systemctl disable --now agency\nsystemctl daemon-reload"
			}
			if got := joinLines(log.lines()); got != wantCmds {
				t.Errorf("commands = %q, want %q", got, wantCmds)
			}
		})
	}
}

// TestDetect_PackageUnit: detection marks the package's unit.
func TestDetect_PackageUnit(t *testing.T) {
	h, _ := packageHost(t, true)
	writeHostFile(t, h, systemUnitPath, global.PackageUnitMarker+"\n[Service]\nUser=agency\nExecStart=/usr/bin/agency\nEnvironment=AGENCY_HOME=/opt/agency\n", 0o644)
	inst := h.detectInstall("")
	if inst == nil || !inst.Package || inst.User != "agency" || inst.AgencyHome != "/opt/agency" {
		t.Errorf("detected %+v", inst)
	}
	writeHostFile(t, h, systemUnitPath, "[Service]\nUser=alice\n", 0o644)
	if inst := h.detectInstall(""); inst == nil || inst.Package {
		t.Errorf("a manual unit detected as the package's: %+v", inst)
	}
}
