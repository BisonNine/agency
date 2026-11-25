// Agency
// License: MIT

// Package runas makes a command run as root (`sudo agency admin`, `sudo
// agency backup`, ...) act as the account that owns the data directory, so
// everything it creates or rewrites there (credentials, config, databases
// with their -wal/-shm files, logs, backups, temporary files renamed into
// place) belongs to the service account and keeps the modes the service
// expects. The process switches once, before the command runs, so no file
// can be missed; only the commands that need root for their own work
// (writing a unit, replacing a system binary) keep it.
package runas

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"go.bison9.com/agency/global"
	"go.bison9.com/agency/internal/install"
)

// keepRoot lists the top-level commands that keep root. install, uninstall
// and upgrade write units and system binaries and hand what they create in
// the data directory to the service account themselves; the others touch
// no data.
var keepRoot = map[string]bool{
	"install":          true,
	"uninstall":        true,
	"upgrade":          true,
	"version":          true,
	"help":             true,
	"completion":       true,
	"__complete":       true,
	"__completeNoDesc": true,
}

// Account is who a root-run command becomes.
type Account struct {
	Name   string
	UID    int
	GID    int
	Groups []int
	Home   string
}

// Seams replaced in tests.
var (
	geteuid       = os.Geteuid
	detectInstall = install.DetectExistingInstall
	switchTo      = setIDs
)

// DataDir is the data directory a root-run command works on, the way the
// commands resolve it: AGENCY_HOME, else the installed service's (its unit,
// or /opt/agency). "" means none is known, and root's own ~/.agency is used.
func DataDir() string {
	if home := os.Getenv(global.EnvVarHome); home != "" {
		return home
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		userHome = ""
	}
	if inst := detectInstall(userHome); inst != nil {
		return inst.AgencyHome
	}
	return ""
}

// Owner is the account owning dir, or nil when dir does not exist or root
// owns it.
func Owner(dir string) (*Account, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("data directory %s: %w", dir, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid == 0 {
		return nil, nil
	}
	uid := strconv.FormatUint(uint64(st.Uid), 10)
	u, err := user.LookupId(uid)
	if err != nil {
		return nil, fmt.Errorf("data directory %s: no account has uid %s: %w", dir, uid, err)
	}
	acct := &Account{Name: u.Username, UID: int(st.Uid), Home: u.HomeDir}
	if acct.GID, err = strconv.Atoi(u.Gid); err != nil {
		return nil, fmt.Errorf("account %s: gid %q: %w", u.Username, u.Gid, err)
	}
	ids, err := u.GroupIds()
	if err != nil {
		return nil, fmt.Errorf("account %s: groups: %w", u.Username, err)
	}
	for _, id := range ids {
		g, err := strconv.Atoi(id)
		if err != nil {
			return nil, fmt.Errorf("account %s: group id %q: %w", u.Username, id, err)
		}
		acct.Groups = append(acct.Groups, g)
	}
	return acct, nil
}

// Drop switches a root process about to run the top-level command named
// command ("" for the default, the service itself) to the account owning the
// data directory, and returns it; nil means nothing changed (not root, a
// command that keeps root, no data directory, or one root owns). AGENCY_HOME
// is set to that directory and HOME, USER and LOGNAME to the account's, so
// the command finds the same data directory as the service.
func Drop(command string) (*Account, error) {
	if geteuid() != 0 || keepRoot[command] {
		return nil, nil
	}
	dir := DataDir()
	if dir == "" {
		return nil, nil
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	acct, err := Owner(dir)
	if err != nil || acct == nil {
		return nil, err
	}
	for k, v := range map[string]string{global.EnvVarHome: dir, "HOME": acct.Home, "USER": acct.Name, "LOGNAME": acct.Name} {
		if err := os.Setenv(k, v); err != nil {
			return nil, fmt.Errorf("set %s: %w", k, err)
		}
	}
	if err := switchTo(acct); err != nil {
		return nil, fmt.Errorf("switch to %s, the owner of %s: %w", acct.Name, dir, err)
	}
	return acct, nil
}
