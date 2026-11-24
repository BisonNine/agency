package internal

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/internal/perms"
	"go.bison9.com/agency/logger"
)

// GetAgencyHome returns the agency home directory.
// Priority:
// 1. $AGENCY_HOME environment variable
// 2. Existing service definition (systemd unit or launchd plist)
// 3. /opt/agency if running from /opt/agency, if /opt/agency/config.json exists, or if /opt/agency exists and ~/.agency does not
// 4. ~/.agency (default data directory)
func GetAgencyHome() string {
	if home := os.Getenv(global.EnvVarHome); home != "" {
		return home
	}

	if detected := detectInstalledAgencyHome(); detected != "" {
		if err := os.Setenv(global.EnvVarHome, detected); err != nil {
			logger.WarnCF("config", "failed to set AGENCY_HOME", map[string]any{"path": detected, "error": err.Error()})
		}
		return detected
	}

	home, err := os.UserHomeDir()
	if err != nil {
		logger.WarnCF("config", "cannot determine home directory; using relative .agency", map[string]any{"error": err.Error()})
	}
	return filepath.Join(home, global.DefaultDataDir)
}

// installRoot is the system install location detectInstalledAgencyHome checks.
// A variable only so tests can point it away from a real /opt/agency.
var installRoot = global.SystemDataDir

func detectInstalledAgencyHome() string {
	// 1. Check if running executable is inside /opt/agency
	if exe, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		if strings.HasPrefix(exe, installRoot) {
			return installRoot
		}
	}

	// 2. If the user already has ~/.agency/config.json, respect their user directory
	if userHome, err := os.UserHomeDir(); err == nil && userHome != "" {
		userCfg := filepath.Join(userHome, global.DefaultDataDir, "config.json")
		if fi, err := os.Stat(userCfg); err == nil && !fi.IsDir() {
			return "" // User explicitly has ~/.agency configured
		}
	}

	// 3. Check if /opt/agency/config.json exists and is owned by the current user
	if fi, err := os.Stat(filepath.Join(installRoot, "config.json")); err == nil && !fi.IsDir() {
		// If current user is the owner of /opt/agency, or running as that user
		if stat, ok := fi.Sys().(*syscall.Stat_t); ok {
			curUID := os.Getuid()
			if curUID == int(stat.Uid) {
				return installRoot
			}
		}
	}

	return ""
}

func GetConfigPath() string {
	return filepath.Join(GetAgencyHome(), "config.json")
}

// LoadConfig seeds and loads the config, and sets the process default
// secrets modes (perms.SetSecretModes) from it, so the secret stores a
// command opens keep security.permissions.secrets.
func LoadConfig() (*config.Config, error) {
	path := GetConfigPath()
	SeedConfig(path)
	cfg, err := config.LoadConfig(path)
	if err != nil {
		return nil, err
	}
	perms.SetPolicy(perms.PolicyFor(cfg))
	return cfg, nil
}

// SeedConfig writes a default config to path when no file exists there yet.
// Best-effort: a failure is logged, and loading the path reports the real
// problem.
func SeedConfig(path string) {
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		if mkdirErr := os.MkdirAll(filepath.Dir(path), 0o700); mkdirErr == nil {
			defaultCfg := config.DefaultConfig()
			// Keeps the default_config marker.
			if seedErr := config.SeedDefaultConfig(path, defaultCfg); seedErr != nil {
				logger.WarnCF("config", "failed to seed default config", map[string]any{"path": path, "error": seedErr.Error()})
			}
		}
	}
}
