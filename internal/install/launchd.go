package install

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.bison9.com/agency/global"
)

const (
	launchdLabel      = global.LaunchdLabel
	launchdPlistName  = global.LaunchdLabel + ".plist"
	systemLaunchdPath = "/Library/LaunchDaemons/" + launchdPlistName
)

// userLaunchdPath returns the path to the LaunchAgent plist for the given user.
func userLaunchdPath(homeDir string) string {
	return filepath.Join(homeDir, "Library", "LaunchAgents", launchdPlistName)
}

// buildLaunchdPlist renders the launchd XML plist for macOS.
// If isSystem is true, UserName and GroupName are included to ensure Agency drops
// root privileges and runs as the target user.
func buildLaunchdPlist(label, username, groupname, execPath, homeDir, binDir, agencyHome string, isSystem bool) string {
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
	if agencyHome != "" {
		fmt.Fprintf(&b, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", global.EnvVarHome, agencyHome)
	}
	b.WriteString("\t</dict>\n")

	// Standard log paths
	logFile := "/tmp/claw-launchd.log"
	if agencyHome != "" {
		logFile = filepath.Join(agencyHome, "logs", "claw-launchd.log")
	} else if homeDir != "" {
		logFile = filepath.Join(homeDir, ".agency", "logs", "claw-launchd.log")
	}
	fmt.Fprintf(&b, "\t<key>StandardOutPath</key>\n\t<string>%s</string>\n", logFile)
	fmt.Fprintf(&b, "\t<key>StandardErrorPath</key>\n\t<string>%s</string>\n", logFile)

	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// launchdDomain is where tu's service lives: the launchd domain target and
// the plist path in it. A root install is a system daemon; any other is the
// user's agent.
type launchdDomain struct {
	target string // "system" or "gui/<uid>"
	path   string // plist of launchdLabel
}

func launchdDomainFor(tu *TargetUser) launchdDomain {
	if tu.IsRoot {
		return launchdDomain{target: "system", path: systemLaunchdPath}
	}
	return launchdDomain{target: "gui/" + tu.UID, path: userLaunchdPath(tu.HomeDir)}
}

// installLaunchd writes the launchd plist and loads the service via launchctl.
func (h *host) installLaunchd(tu *TargetUser, targetBin, binDir, agencyHome string) error {
	// Ensure user log directory exists
	logsDir := filepath.Join(agencyHome, "logs")
	if err := os.MkdirAll(h.path(logsDir), 0o755); err != nil {
		return fmt.Errorf("creating logs directory %s: %w", logsDir, err)
	}

	d := launchdDomainFor(tu)
	if tu.IsRoot {
		// System Mode: writes to /Library/LaunchDaemons/<label>.plist
		plist := buildLaunchdPlist(launchdLabel, tu.Username, tu.GroupName, targetBin, tu.HomeDir, binDir, agencyHome, true)
		if err := os.WriteFile(h.path(d.path), []byte(plist), 0o644); err != nil {
			return fmt.Errorf("writing launchd daemon %s: %w", d.path, err)
		}
		fmt.Printf("Registered launchd daemon: %s (running as %s:%s)\n", d.path, tu.Username, tu.GroupName)
	} else {
		// User Mode: writes to ~/Library/LaunchAgents/<label>.plist
		plist := buildLaunchdPlist(launchdLabel, tu.Username, tu.GroupName, targetBin, tu.HomeDir, binDir, agencyHome, false)
		if err := os.MkdirAll(h.path(filepath.Dir(d.path)), 0o755); err != nil {
			return fmt.Errorf("creating directory %s: %w", filepath.Dir(d.path), err)
		}
		if err := os.WriteFile(h.path(d.path), []byte(plist), 0o644); err != nil {
			return fmt.Errorf("writing launchd agent %s: %w", d.path, err)
		}
		fmt.Printf("Registered launchd user agent: %s\n", d.path)
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

// uninstallLaunchd unloads and removes the launchd service plist.
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
