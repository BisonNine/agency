package install

import (
	"bytes"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"go.bison9.com/agency/global"
)

const (
	launchdLabel      = global.LaunchdLabel
	launchdPlistName  = global.LaunchdLabel + ".plist"
	systemLaunchdPath = "/Library/LaunchDaemons/" + launchdPlistName

	// The label and plists of earlier releases, kept only to find and move
	// such a service.
	legacyLaunchdLabel      = global.LegacyLaunchdLabel
	legacyLaunchdPlistName  = global.LegacyLaunchdLabel + ".plist"
	legacySystemLaunchdPath = "/Library/LaunchDaemons/" + legacyLaunchdPlistName
)

// userLaunchdPath returns the path to the LaunchAgent plist for the given user.
func userLaunchdPath(homeDir string) string {
	return filepath.Join(homeDir, "Library", "LaunchAgents", launchdPlistName)
}

// legacyUserLaunchdPath is userLaunchdPath for the legacy label.
func legacyUserLaunchdPath(homeDir string) string {
	return filepath.Join(homeDir, "Library", "LaunchAgents", legacyLaunchdPlistName)
}

// buildLaunchdPlist renders the launchd XML plist for macOS.
// If isSystem is true, UserName and GroupName are included to ensure Agency drops
// root privileges and runs as the target user.
func buildLaunchdPlist(label, username, groupname, execPath, homeDir, binDir, clawHome string, isSystem bool) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")
	fmt.Fprintf(&b, "\t<key>Label</key>\n\t<string>%s</string>\n", label)

	if isSystem && username != "" {
		fmt.Fprintf(&b, "\t<key>UserName</key>\n\t<string>%s</string>\n", username)
		if groupname != "" {
			fmt.Fprintf(&b, "\t<key>GroupName</key>\n\t<string>%s</string>\n", groupname)
		}
	}

	if homeDir != "" {
		fmt.Fprintf(&b, "\t<key>WorkingDirectory</key>\n\t<string>%s</string>\n", homeDir)
	}

	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	fmt.Fprintf(&b, "\t\t<string>%s</string>\n", execPath)
	b.WriteString("\t</array>\n")

	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>KeepAlive</key>\n\t<true/>\n")

	// Environment variables
	b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	if homeDir != "" {
		fmt.Fprintf(&b, "\t\t<key>HOME</key>\n\t\t<string>%s</string>\n", homeDir)
	}
	fmt.Fprintf(&b, "\t\t<key>PATH</key>\n\t\t<string>%s</string>\n", servicePATH(homeDir, binDir))
	if clawHome != "" {
		fmt.Fprintf(&b, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", global.EnvVarHome, clawHome)
	}
	b.WriteString("\t</dict>\n")

	// Standard log paths
	logFile := "/tmp/claw-launchd.log"
	if clawHome != "" {
		logFile = filepath.Join(clawHome, "logs", "claw-launchd.log")
	} else if homeDir != "" {
		logFile = filepath.Join(homeDir, ".claw", "logs", "claw-launchd.log")
	}
	fmt.Fprintf(&b, "\t<key>StandardOutPath</key>\n\t<string>%s</string>\n", logFile)
	fmt.Fprintf(&b, "\t<key>StandardErrorPath</key>\n\t<string>%s</string>\n", logFile)

	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// launchdDomain is where tu's service lives: the launchd domain target and
// the plist paths of the current and the legacy label in it. A root install
// is a system daemon; any other is the user's agent.
type launchdDomain struct {
	target     string // "system" or "gui/<uid>"
	path       string // plist of launchdLabel
	legacyPath string // plist of legacyLaunchdLabel
}

func launchdDomainFor(tu *TargetUser) launchdDomain {
	if tu.IsRoot {
		return launchdDomain{target: "system", path: systemLaunchdPath, legacyPath: legacySystemLaunchdPath}
	}
	return launchdDomain{
		target:     "gui/" + tu.UID,
		path:       userLaunchdPath(tu.HomeDir),
		legacyPath: legacyUserLaunchdPath(tu.HomeDir),
	}
}

// installLaunchd writes the launchd plist and loads the service via launchctl.
// A service registered under the legacy label in the same domain is unloaded
// and its plist removed first, so the install moves it to the current label.
func (h *host) installLaunchd(tu *TargetUser, targetBin, binDir, clawHome string) error {
	// Ensure user log directory exists
	logsDir := filepath.Join(clawHome, "logs")
	if err := os.MkdirAll(h.path(logsDir), 0o755); err != nil {
		return fmt.Errorf("creating logs directory %s: %w", logsDir, err)
	}

	d := launchdDomainFor(tu)
	if tu.IsRoot {
		// System Mode: writes to /Library/LaunchDaemons/<label>.plist
		plist := buildLaunchdPlist(launchdLabel, tu.Username, tu.GroupName, targetBin, tu.HomeDir, binDir, clawHome, true)
		if err := os.WriteFile(h.path(d.path), []byte(plist), 0o644); err != nil {
			return fmt.Errorf("writing launchd daemon %s: %w", d.path, err)
		}
		fmt.Printf("Registered launchd daemon: %s (running as %s:%s)\n", d.path, tu.Username, tu.GroupName)
	} else {
		// User Mode: writes to ~/Library/LaunchAgents/<label>.plist
		plist := buildLaunchdPlist(launchdLabel, tu.Username, tu.GroupName, targetBin, tu.HomeDir, binDir, clawHome, false)
		if err := os.MkdirAll(h.path(filepath.Dir(d.path)), 0o755); err != nil {
			return fmt.Errorf("creating directory %s: %w", filepath.Dir(d.path), err)
		}
		if err := os.WriteFile(h.path(d.path), []byte(plist), 0o644); err != nil {
			return fmt.Errorf("writing launchd agent %s: %w", d.path, err)
		}
		fmt.Printf("Registered launchd user agent: %s\n", d.path)
	}

	if _, err := h.retireLegacyLaunchd(d); err != nil {
		return err
	}
	if err := h.loadLaunchd(d); err != nil {
		return err
	}
	if tu.IsRoot {
		fmt.Printf("Loaded launchd system daemon %s\n", launchdLabel)
	} else {
		fmt.Printf("Loaded launchd user agent %s\n", launchdLabel)
	}
	return nil
}

// loadLaunchd unloads any running instance of the service in d and loads its
// plist again.
func (h *host) loadLaunchd(d launchdDomain) error {
	h.runBestEffort("launchctl", "bootout", d.target+"/"+launchdLabel)
	h.runBestEffort("launchctl", "unload", "-w", d.path)

	// Bootstrap service (modern launchctl fallback to load -w)
	if out, err := h.run("launchctl", "bootstrap", d.target, d.path); err != nil {
		// Fallback to legacy launchctl load
		if loadOut, loadErr := h.run("launchctl", "load", "-w", d.path); loadErr != nil {
			return fmt.Errorf("launchctl bootstrap %s: %s / %s (%w)", d.target, string(out), string(loadOut), loadErr)
		}
	}
	return nil
}

// retireLegacyLaunchd unloads a service registered in d under the legacy
// label and removes its plist. It reports whether there was one.
func (h *host) retireLegacyLaunchd(d launchdDomain) (bool, error) {
	if !h.fileExists(d.legacyPath) {
		return false, nil
	}
	h.runBestEffort("launchctl", "bootout", d.target+"/"+legacyLaunchdLabel)
	h.runBestEffort("launchctl", "unload", "-w", d.legacyPath)
	if err := os.Remove(h.path(d.legacyPath)); err != nil && !os.IsNotExist(err) {
		return true, fmt.Errorf("removing %s: %w", d.legacyPath, err)
	}
	fmt.Printf("Removed the service registered as %s: %s\n", legacyLaunchdLabel, d.legacyPath)
	return true, nil
}

// MigrateLegacyLaunchd moves a macOS service registered under the legacy
// launchd label to the current one: run as root it moves the system daemon,
// otherwise the current user's agent. The plist keeps everything but its
// label, so the binary, data directory and settings stay the same. Nothing
// happens when there is no legacy service, or on another system.
func MigrateLegacyLaunchd() error {
	h := realHost()
	if h.goos != "darwin" {
		return nil
	}
	tu := &TargetUser{IsRoot: geteuid() == 0}
	if !tu.IsRoot {
		u, err := user.Current()
		if err != nil {
			return fmt.Errorf("cannot determine current user: %w", err)
		}
		tu.UID, tu.HomeDir = u.Uid, u.HomeDir
	}
	return h.migrateLegacyLaunchd(tu)
}

// migrateLegacyLaunchd is MigrateLegacyLaunchd for tu's domain on h. When a
// plist for the current label is already there it is kept and the legacy one
// only removed.
func (h *host) migrateLegacyLaunchd(tu *TargetUser) error {
	if h.goos != "darwin" {
		return nil
	}
	d := launchdDomainFor(tu)
	if !h.fileExists(d.legacyPath) {
		return nil
	}
	if !h.fileExists(d.path) {
		data, err := os.ReadFile(h.path(d.legacyPath))
		if err != nil {
			return fmt.Errorf("reading %s: %w", d.legacyPath, err)
		}
		plist := strings.Replace(string(data),
			"<string>"+legacyLaunchdLabel+"</string>", "<string>"+launchdLabel+"</string>", 1)
		if err := os.WriteFile(h.path(d.path), []byte(plist), 0o644); err != nil { //nolint:gosec // the installer's own plist path, built from fixed names and the account's home
			return fmt.Errorf("writing %s: %w", d.path, err)
		}
	}
	if _, err := h.retireLegacyLaunchd(d); err != nil {
		return err
	}
	if err := h.loadLaunchd(d); err != nil {
		return err
	}
	fmt.Printf("The service is now registered as %s: %s\n", launchdLabel, d.path)
	return nil
}

// uninstallLaunchd unloads and removes the launchd service plist, under the
// current label and the legacy one.
func (h *host) uninstallLaunchd(tu *TargetUser) error {
	var errs []string
	d := launchdDomainFor(tu)
	h.runBestEffort("launchctl", "bootout", d.target+"/"+launchdLabel)
	h.runBestEffort("launchctl", "unload", "-w", d.path)
	if err := os.Remove(h.path(d.path)); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Sprintf("removing %s: %v", d.path, err))
	}
	if tu.IsRoot {
		fmt.Printf("Removed launchd daemon: %s\n", d.path)
	} else {
		fmt.Printf("Removed launchd agent: %s\n", d.path)
	}
	if _, err := h.retireLegacyLaunchd(d); err != nil {
		errs = append(errs, err.Error())
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// isLaunchdServiceActive checks if launchd is managing the service.
func (h *host) isLaunchdServiceActive(label string) bool {
	out, err := h.run("launchctl", "list")
	if err != nil {
		return false
	}
	return bytes.Contains(out, []byte(label))
}
