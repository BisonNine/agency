// Agency
// License: MIT

package runas

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"testing"

	"go.bison9.com/agency/global"
	"go.bison9.com/agency/internal/install"
)

// stub makes the process look like root with the install detectInstall
// finds, and records the switch instead of making it.
func stub(t *testing.T, euid int, inst *install.ExistingInstall) *[]*Account {
	t.Helper()
	var switched []*Account
	origEUID, origDetect, origSwitch := geteuid, detectInstall, switchTo
	t.Cleanup(func() { geteuid, detectInstall, switchTo = origEUID, origDetect, origSwitch })
	geteuid = func() int { return euid }
	detectInstall = func(string) *install.ExistingInstall { return inst }
	switchTo = func(a *Account) error { switched = append(switched, a); return nil }
	for _, k := range []string{global.EnvVarHome, "HOME", "USER", "LOGNAME"} {
		t.Setenv(k, os.Getenv(k))
	}
	return &switched
}

func me(t *testing.T) *user.User {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	if u.Uid == "0" {
		t.Skip("running as root: a temp dir belongs to root")
	}
	return u
}

// TestDrop_ToDataDirOwner: root running a data command switches to the
// account owning the installed service's data directory, with AGENCY_HOME,
// HOME, USER and LOGNAME set to match.
func TestDrop_ToDataDirOwner(t *testing.T) {
	u := me(t)
	dir := t.TempDir()
	switched := stub(t, 0, &install.ExistingInstall{AgencyHome: dir})
	t.Setenv(global.EnvVarHome, "")
	for _, command := range []string{"admin", "status", "backup", "restore", "network", "token", "cron", ""} {
		*switched = nil
		acct, err := Drop(command)
		if err != nil {
			t.Fatalf("%q: %v", command, err)
		}
		if acct == nil || len(*switched) != 1 || (*switched)[0] != acct {
			t.Fatalf("%q: switched %v, returned %+v", command, *switched, acct)
		}
		if strconv.Itoa(acct.UID) != u.Uid || strconv.Itoa(acct.GID) != u.Gid || acct.Name != u.Username || acct.Home != u.HomeDir {
			t.Errorf("%q: account %+v, want %s (%s:%s)", command, acct, u.Username, u.Uid, u.Gid)
		}
		if len(acct.Groups) == 0 {
			t.Errorf("%q: no supplementary groups", command)
		}
		if os.Getenv(global.EnvVarHome) != dir || os.Getenv("HOME") != u.HomeDir || os.Getenv("USER") != u.Username || os.Getenv("LOGNAME") != u.Username {
			t.Errorf("%q: env AGENCY_HOME=%q HOME=%q USER=%q LOGNAME=%q", command,
				os.Getenv(global.EnvVarHome), os.Getenv("HOME"), os.Getenv("USER"), os.Getenv("LOGNAME"))
		}
		t.Setenv(global.EnvVarHome, "")
	}
}

// TestDrop_AgencyHomeWins: an explicit AGENCY_HOME decides the directory.
func TestDrop_AgencyHomeWins(t *testing.T) {
	me(t)
	dir := t.TempDir()
	switched := stub(t, 0, &install.ExistingInstall{AgencyHome: "/nonexistent/elsewhere"})
	t.Setenv(global.EnvVarHome, dir)
	if _, err := Drop("admin"); err != nil || len(*switched) != 1 {
		t.Fatalf("switched %v, err %v", *switched, err)
	}
}

// TestDrop_NoSwitch: nothing changes when the process is not root, for the
// commands that keep root, without a known data directory, for one that does
// not exist, and for one root owns.
func TestDrop_NoSwitch(t *testing.T) {
	me(t)
	dir := t.TempDir()
	cases := []struct {
		name    string
		euid    int
		command string
		inst    *install.ExistingInstall
	}{
		{"not root", 1000, "admin", &install.ExistingInstall{AgencyHome: dir}},
		{"install", 0, "install", &install.ExistingInstall{AgencyHome: dir}},
		{"uninstall", 0, "uninstall", &install.ExistingInstall{AgencyHome: dir}},
		{"upgrade", 0, "upgrade", &install.ExistingInstall{AgencyHome: dir}},
		{"version", 0, "version", &install.ExistingInstall{AgencyHome: dir}},
		{"no install", 0, "admin", nil},
		{"install without data directory", 0, "admin", &install.ExistingInstall{BinaryPath: "/usr/local/bin/agency"}},
		{"missing data directory", 0, "admin", &install.ExistingInstall{AgencyHome: filepath.Join(dir, "missing")}},
		{"root-owned data directory", 0, "admin", &install.ExistingInstall{AgencyHome: "/"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			switched := stub(t, tc.euid, tc.inst)
			t.Setenv(global.EnvVarHome, "")
			acct, err := Drop(tc.command)
			if err != nil || acct != nil || len(*switched) != 0 {
				t.Errorf("acct %+v, switched %v, err %v", acct, *switched, err)
			}
			if os.Getenv(global.EnvVarHome) != "" {
				t.Errorf("AGENCY_HOME set to %q", os.Getenv(global.EnvVarHome))
			}
		})
	}
}
