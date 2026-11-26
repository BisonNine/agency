package install

import (
	"bufio"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/utils"
)

// ExistingInstall holds details of a previously installed Agency service or binary.
type ExistingInstall struct {
	BinaryPath  string
	ServiceType string // "systemd-system", "systemd-user", "launchd-daemon", "launchd-agent"
	ServicePath string
	User        string
	AgencyHome  string
	IsActive    bool
	// Package reports a systemd unit written for the Debian package (its
	// first line is global.PackageUnitMarker): apt manages that install.
	Package bool
}

// optAgencyHome is the data directory of the standard system install. Its
// binary and the openclaw alias live in its bin area, optAgencyBinDir.
const optAgencyHome = global.SystemDataDir

// optAgencyBinDir is where an /opt/agency install keeps the binary and the alias.
var optAgencyBinDir = filepath.Join(optAgencyHome, config.BinDirName)

// inOptAgency reports whether p is /opt/agency or a path inside it (not a
// sibling such as /opt/agency-old).
func inOptAgency(p string) bool {
	return p == optAgencyHome || strings.HasPrefix(p, optAgencyHome+"/")
}

// host is the machine the installer inspects. Every path the installer works
// with is the machine's own (/opt/agency, /etc/systemd/system/...); root, when
// set, is prefixed to them on disk, so tests build a fake install in a temp
// dir. The command hooks keep tests from consulting the real system.
type host struct {
	root       string
	goos       string
	executable func() (string, error)
	lookPath   func(file string) (string, error)
	commandOK  func(name string, args ...string) bool
	// run runs a service manager command (systemctl, launchctl, loginctl)
	// and returns its combined output.
	run func(name string, args ...string) ([]byte, error)
	// targetUser resolves the account the service runs as (resolveTargetUser).
	targetUser func(explicitUser string) (*TargetUser, error)
	// detect finds an existing install (DetectExistingInstall).
	detect func(homeDir string) *ExistingInstall
}

// realHost is the machine the installer runs on.
func realHost() *host {
	h := &host{
		goos:       runtime.GOOS,
		executable: os.Executable,
		lookPath:   exec.LookPath,
		commandOK: func(name string, args ...string) bool {
			return exec.Command(name, args...).Run() == nil //nolint:gosec // fixed systemctl binary and the installer's own unit name
		},
		run: func(name string, args ...string) ([]byte, error) {
			return exec.Command(name, args...).CombinedOutput() //nolint:gosec // fixed service manager binaries; args are the installer's own label, unit paths and target user
		},
		targetUser: resolveTargetUser,
	}
	h.detect = h.detectInstall
	return h
}

// path maps one of the machine's paths to where it lies on disk.
func (h *host) path(p string) string {
	if h.root == "" {
		return p
	}
	return filepath.Join(h.root, p)
}

func (h *host) fileExists(p string) bool { return fileExists(h.path(p)) }

func (h *host) dirExists(p string) bool { return dirExists(h.path(p)) }

// exists reports whether anything, a dangling symlink included, is at p.
func (h *host) exists(p string) bool {
	_, err := os.Lstat(h.path(p))
	return err == nil
}

// ownerName is the name of the account owning p, or "" for root or when it
// cannot be found.
func (h *host) ownerName(p string) string {
	fi, err := os.Stat(h.path(p))
	if err != nil {
		return ""
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	u, err := user.LookupId(strconv.FormatUint(uint64(stat.Uid), 10))
	if err != nil || u.Username == "root" {
		return ""
	}
	return u.Username
}

// optAgencyBinary is the binary of an /opt/agency install, bin/agency, or ""
// when there is none.
func (h *host) optAgencyBinary() string {
	if p := TargetBinaryPath(optAgencyBinDir); h.fileExists(p) {
		return p
	}
	return ""
}

// DetectExistingInstall searches for an existing Agency installation on the system.
// It inspects systemd unit files, launchd plists, running process paths, and PATH.
func DetectExistingInstall(homeDir string) *ExistingInstall {
	return realHost().detectInstall(homeDir)
}

// detectInstall is DetectExistingInstall on h.
func (h *host) detectInstall(homeDir string) *ExistingInstall {
	switch h.goos {
	case "linux":
		return h.detectLinuxInstall(homeDir)
	case "darwin":
		return h.detectDarwinInstall(homeDir)
	}
	return nil
}

func (h *host) detectLinuxInstall(homeDir string) *ExistingInstall {
	// 1. Check systemd system service (/etc/systemd/system/agency.service)
	if h.fileExists(systemUnitPath) {
		inst := h.parseSystemdUnit(systemUnitPath, "systemd-system")
		inst.IsActive = h.commandOK("systemctl", "is-active", "--quiet", serviceName)
		return inst
	}

	// 2. Check systemd user service (~/.config/systemd/user/agency.service)
	userPath := userUnitPath(homeDir)
	if homeDir != "" && h.fileExists(userPath) {
		inst := h.parseSystemdUnit(userPath, "systemd-user")
		inst.IsActive = h.commandOK("systemctl", "--user", "is-active", "--quiet", serviceName)
		return inst
	}

	// 3. Check /opt/agency (standard production / system install location)
	if h.dirExists(optAgencyHome) {
		return &ExistingInstall{
			AgencyHome: optAgencyHome,
			BinaryPath: h.optAgencyBinary(),
			User:       h.ownerName(optAgencyHome),
		}
	}

	// 4. Check running executable, then the system PATH
	return h.detectFromBinary()
}

func (h *host) detectDarwinInstall(homeDir string) *ExistingInstall {
	// 1. Check LaunchDaemon (/Library/LaunchDaemons/<label>.plist), then
	// 2. LaunchAgent (~/Library/LaunchAgents/<label>.plist).
	plists := []struct{ path, serviceType string }{
		{systemLaunchdPath, "launchd-daemon"},
		{userLaunchdPath(homeDir), "launchd-agent"},
	}
	for _, p := range plists {
		if h.fileExists(p.path) {
			inst := h.parseLaunchdPlist(p.path, p.serviceType)
			inst.IsActive = h.isLaunchdServiceActive(launchdLabel)
			return inst
		}
	}

	// 3. Check /opt/agency
	if h.dirExists(optAgencyHome) {
		return &ExistingInstall{
			AgencyHome: optAgencyHome,
			BinaryPath: h.optAgencyBinary(),
		}
	}

	// 4. Check running executable, then the system PATH
	return h.detectFromBinary()
}

// detectFromBinary finds an install by the running executable, else by agency
// on PATH, ignoring build and temporary locations.
func (h *host) detectFromBinary() *ExistingInstall {
	candidates := []func() (string, error){
		h.executable,
		func() (string, error) { return h.lookPath(global.InstalledBinaryName) },
	}
	for _, find := range candidates {
		p, err := find()
		if err != nil {
			continue
		}
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil || isBuildOrTempPath(resolved) {
			continue
		}
		inst := &ExistingInstall{BinaryPath: resolved}
		if inOptAgency(resolved) {
			inst.AgencyHome = optAgencyHome
		}
		return inst
	}
	return nil
}

func (h *host) parseSystemdUnit(path, serviceType string) *ExistingInstall {
	inst := &ExistingInstall{
		ServicePath: path,
		ServiceType: serviceType,
	}

	f, err := os.Open(h.path(path))
	if err != nil {
		return inst
	}
	defer utils.CloseQuietly(f)

	var workingDir, pathEnv string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == global.PackageUnitMarker {
			inst.Package = true
		} else if after, ok := strings.CutPrefix(line, "ExecStart="); ok {
			val := after
			parts := strings.Fields(val)
			if len(parts) > 0 {
				inst.BinaryPath = parts[0]
			}
		} else if after, ok := strings.CutPrefix(line, "User="); ok {
			inst.User = after
		} else if after, ok := strings.CutPrefix(line, "WorkingDirectory="); ok {
			workingDir = strings.Trim(after, "\"' \t")
		} else if after, ok := strings.CutPrefix(line, "Environment="); ok {
			envVal := after
			if _, after, ok := strings.Cut(envVal, "AGENCY_HOME="); ok {
				sub := after
				if len(sub) > 0 && (sub[0] == '"' || sub[0] == '\'') {
					q := sub[0]
					if end := strings.IndexByte(sub[1:], q); end != -1 {
						inst.AgencyHome = sub[1 : 1+end]
					} else {
						inst.AgencyHome = strings.Trim(sub, "\"' \t")
					}
				} else {
					fields := strings.Fields(sub)
					if len(fields) > 0 {
						inst.AgencyHome = strings.Trim(fields[0], "\"' \t")
					}
				}
			}
			if _, after, ok := strings.Cut(envVal, "PATH="); ok {
				pathEnv = after
			}
		}
	}

	// Fallback inference for AgencyHome
	if inst.AgencyHome == "" {
		if inOptAgency(inst.BinaryPath) {
			inst.AgencyHome = optAgencyHome
		} else if workingDir != "" && workingDir != "/" && !strings.HasPrefix(workingDir, "/home") {
			inst.AgencyHome = workingDir
		} else if pathHasOptAgency(pathEnv) {
			inst.AgencyHome = optAgencyHome
		} else if h.dirExists(optAgencyHome) {
			inst.AgencyHome = optAgencyHome
		}
	}

	// Fallback inference for User
	if inst.User == "" && inst.AgencyHome != "" {
		inst.User = h.ownerName(inst.AgencyHome)
	}
	if inst.User == "" && inst.BinaryPath != "" {
		inst.User = h.ownerName(inst.BinaryPath)
	}

	return inst
}

// pathHasOptAgency reports whether a unit's PATH value lists a folder in
// /opt/agency.
func pathHasOptAgency(pathEnv string) bool {
	dirs := strings.FieldsFunc(pathEnv, func(r rune) bool {
		return r == ':' || r == '"' || r == '\'' || r == ' ' || r == '\t'
	})
	return slices.ContainsFunc(dirs, inOptAgency)
}

func (h *host) parseLaunchdPlist(path, serviceType string) *ExistingInstall {
	inst := &ExistingInstall{
		ServicePath: path,
		ServiceType: serviceType,
	}

	data, err := os.ReadFile(h.path(path))
	if err != nil {
		return inst
	}
	content := string(data)

	// Extract binary from ProgramArguments
	if idx := strings.Index(content, "<key>ProgramArguments</key>"); idx != -1 {
		sub := content[idx:]
		if start := strings.Index(sub, "<string>"); start != -1 {
			if end := strings.Index(sub[start+8:], "</string>"); end != -1 {
				inst.BinaryPath = sub[start+8 : start+8+end]
			}
		}
	}

	// Extract UserName
	if idx := strings.Index(content, "<key>UserName</key>"); idx != -1 {
		sub := content[idx:]
		if start := strings.Index(sub, "<string>"); start != -1 {
			if end := strings.Index(sub[start+8:], "</string>"); end != -1 {
				inst.User = sub[start+8 : start+8+end]
			}
		}
	}

	// Extract AGENCY_HOME
	if idx := strings.Index(content, "<key>AGENCY_HOME</key>"); idx != -1 {
		sub := content[idx:]
		if start := strings.Index(sub, "<string>"); start != -1 {
			if end := strings.Index(sub[start+8:], "</string>"); end != -1 {
				inst.AgencyHome = sub[start+8 : start+8+end]
			}
		}
	}

	// Fallback inference for AgencyHome
	if inst.AgencyHome == "" {
		if inOptAgency(inst.BinaryPath) {
			inst.AgencyHome = optAgencyHome
		} else if h.dirExists(optAgencyHome) {
			inst.AgencyHome = optAgencyHome
		}
	}

	return inst
}

// isBuildOrTempPath returns true if the path appears to be a build artifact or temp file.
func isBuildOrTempPath(p string) bool {
	clean := filepath.Clean(p)
	parts := strings.SplitSeq(clean, string(filepath.Separator))
	for part := range parts {
		if part == "build" || part == "tmp" || part == "temp" || strings.HasPrefix(part, "agency-upgrade-") {
			return true
		}
	}
	return underTempDir(clean)
}

// underTempDir reports whether p is inside the system temporary directory.
// On macOS that directory has no "tmp" component (/var/folders/...), and it
// is reached through a symlink, so both its spellings are checked.
func underTempDir(p string) bool {
	roots := []string{filepath.Clean(os.TempDir())}
	if resolved, err := filepath.EvalSymlinks(roots[0]); err == nil {
		roots = append(roots, resolved)
	}
	for _, root := range roots {
		if strings.HasPrefix(p, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func fileExists(p string) bool {
	info, err := os.Stat(p) // #nosec G703 -- install paths: the machine's own and AGENCY_HOME chosen by the operator
	return err == nil && !info.IsDir()
}
