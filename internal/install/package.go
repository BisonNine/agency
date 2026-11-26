package install

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.bison9.com/agency/app"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/internal/admin"
)

// systemdRunDir exists while systemd is the running init system.
const systemdRunDir = "/run/systemd/system"

// existingInstallLeft is what the package prints when it finds an install it
// did not make and leaves it as it is.
const existingInstallLeft = "An existing %s install was found and left unchanged (%s)."

// packageRemoveHint is the command that removes a package-managed install.
func packageRemoveHint() string {
	return "sudo apt remove " + global.DebPackageName
}

// packageManagedErr refuses an interactive install or uninstall on an install
// the Debian package set up, so apt keeps track of what it made.
func packageManagedErr(existing *ExistingInstall) error {
	return fmt.Errorf("%s was set up by the %s package (%s); upgrade or remove it with apt: %s",
		app.ShortName(), global.DebPackageName, existing.ServicePath, packageRemoveHint())
}

// packagePATH is the PATH of the package's unit: the service account's own
// bin folders, where CLIs signed in with `sudo -u agency -i` install
// themselves, whether or not they exist yet, then the system folders.
func packagePATH(homeDir, binDir string) string {
	var parts []string
	seen := map[string]bool{}
	for _, p := range []string{
		filepath.Join(homeDir, ".local", "bin"),
		filepath.Join(homeDir, "bin"),
		"/usr/local/bin", "/usr/bin", "/bin",
		binDir,
	} {
		if p != "" && !seen[p] {
			seen[p] = true
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ":")
}

// buildPackageUnit is the unit the package's postinst has written: the system
// unit `install` writes, marked as the package's and with a fixed PATH.
func buildPackageUnit(tu *TargetUser, execPath, binDir, agencyHome string) string {
	return global.PackageUnitMarker + "\n" +
		renderSystemUnit(tu.Username, tu.GroupName, execPath, tu.HomeDir, packagePATH(tu.HomeDir, binDir), agencyHome)
}

// systemdRunning reports whether systemctl can manage services here.
func (h *host) systemdRunning() bool {
	if _, err := h.lookPath("systemctl"); err != nil {
		return false
	}
	return h.dirExists(systemdRunDir)
}

// installPackage is `install --package`, run by the Debian package's postinst
// after it has created the service account and its home: it writes the unit
// for the running binary (/usr/bin/agency) running as global.ServiceAccount
// with AGENCY_HOME (default /opt/agency), enables and (re)starts it. Unlike
// an interactive install it never asks anything and starts the service
// without an admin account. While there is none (a new install) it says which
// account runs the service (accountCreated: the postinst created it just
// now), where the files are, where the WebUI is, and last what to do next. A
// system unit it did not write is someone else's install, left unchanged.
func (h *host) installPackage(accountCreated bool) error {
	if h.goos != "linux" {
		return fmt.Errorf("`%s install --package` is only for the Debian package on Linux", app.BinaryName())
	}
	if geteuid() != 0 {
		return fmt.Errorf("`%s install --package` must run as root; the %s package runs it", app.BinaryName(), global.DebPackageName)
	}
	tu, err := h.targetUser(global.ServiceAccount)
	if err != nil {
		return err
	}
	agencyHome := os.Getenv(global.EnvVarHome)
	if agencyHome == "" {
		agencyHome = global.SystemDataDir
	}
	if where := foreignInstall(h.detect(tu.HomeDir), tu.Username); where != "" {
		fmt.Printf(existingInstallLeft+"\n", app.ShortName(), where)
		return nil
	}

	exePath, err := h.executable()
	if err != nil {
		return fmt.Errorf("cannot locate running binary: %w", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(exePath); rerr == nil {
		exePath = resolved
	}
	unit := buildPackageUnit(tu, exePath, filepath.Dir(exePath), agencyHome)
	if err := os.WriteFile(h.path(systemUnitPath), []byte(unit), 0o644); err != nil { //nolint:gosec // a systemd unit is world-readable by convention
		return fmt.Errorf("writing systemd unit %s: %w", systemUnitPath, err)
	}

	running := h.systemdRunning()
	if running {
		for _, args := range [][]string{{"daemon-reload"}, {"enable", serviceName}, {"restart", serviceName}} {
			if out, err := h.run("systemctl", args...); err != nil {
				return fmt.Errorf("systemctl %s: %s (%w)", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
			}
		}
		fmt.Printf("%s is installed and running.\n", app.ShortName())
	} else {
		fmt.Printf("%s is installed; systemd is not running, so the service was not started.\n", app.ShortName())
	}
	if !h.fileExists(admin.Path(agencyHome)) {
		gw := installedGateway(h.path(agencyHome))
		var b strings.Builder
		b.WriteString(packageLocations(tu.Username, accountCreated, exePath, agencyHome))
		printListenerSummary(&b, gw)
		b.WriteString(packageNextSteps(gw))
		fmt.Print(b.String())
	}
	return nil
}

// packageLocations says which account runs the service and where the package
// put things.
func packageLocations(account string, created bool, exePath, agencyHome string) string {
	state := "already existed"
	if created {
		state = "created"
	}
	var b strings.Builder
	line := func(label, value string) { fmt.Fprintf(&b, "%-24s%s\n", label, value) }
	b.WriteString("\n")
	line("Service account:", account+" ("+state+")")
	line("Program:", exePath)
	line("Data and settings:", agencyHome)
	line("Service:", systemUnitPath)
	return b.String()
}

// packageNextSteps is what a new install still needs, printed last, with a
// blank line around the heading, between the steps and at the end: the admin
// account, and, while the WebUI is served on the network but only localhost
// is allowed in, the command that allows a network.
func packageNextSteps(gw config.GatewayConfig) string {
	bin := app.BinaryName()
	var b strings.Builder
	b.WriteString("\nNext steps:\n\n")
	fmt.Fprintf(&b, "  1. Create the admin account:\n       sudo %s admin\n", bin)
	if (gw.HTTPOnNetwork() || gw.HTTPSOnNetwork()) && len(gw.EffectiveAllowedCIDRs()) == 0 {
		b.WriteString("\n  2. To use the WebUI from another computer, allow it, for example:\n")
		for _, c := range [][2]string{
			{"192.168.1.0/24", "one network"},
			{"192.168.1.50", "one computer"},
			{"", "all private networks"},
		} {
			fmt.Fprintf(&b, "       %-38s(%s)\n", strings.TrimSpace("sudo "+bin+" network "+c[0]), c[1])
		}
	}
	b.WriteString("\n") // set apart from what the package manager prints next
	return b.String()
}

// foreignInstall names an install the package did not make, or returns "":
// a system unit without the package's marker, or a data directory owned by
// another account and no unit at all.
func foreignInstall(existing *ExistingInstall, account string) string {
	switch {
	case existing == nil:
		return ""
	case existing.ServicePath == systemUnitPath:
		if existing.Package {
			return ""
		}
		return existing.ServicePath
	case existing.ServicePath == "" && existing.AgencyHome != "" && existing.User != "" && existing.User != account:
		return existing.AgencyHome
	}
	return ""
}

// runningBinary is the running executable with links resolved, or "" when
// it cannot be found.
func (h *host) runningBinary() string {
	p, err := h.executable()
	if err != nil {
		return ""
	}
	if resolved, rerr := filepath.EvalSymlinks(p); rerr == nil {
		return resolved
	}
	return p
}

// uninstallPackage is `uninstall --package`, run by the Debian package's
// prerm on removal: it stops, disables and removes the system unit when the
// package wrote it, or when it runs the package's binary, which the removal
// takes away. Any other install is left alone.
func (h *host) uninstallPackage() error {
	if h.goos != "linux" {
		return nil
	}
	existing := h.detect("")
	if existing == nil || existing.ServicePath != systemUnitPath {
		return nil
	}
	if !existing.Package && (existing.BinaryPath == "" || existing.BinaryPath != h.runningBinary()) {
		return nil
	}
	return h.uninstallSystemd(&TargetUser{Username: "root", UID: "0", GID: "0", IsRoot: true})
}
