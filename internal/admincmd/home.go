// Agency
// License: MIT

package admincmd

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/internal/install"
)

// ResolveHome returns the AGENCY_HOME the running service uses, so the
// credentials file lands where the gateway will read it: the AGENCY_HOME
// environment variable, else the AGENCY_HOME of the installed service
// definition (systemd unit or launchd plist), else ~/.agency. The detected
// install, when there is one, is returned too so a root caller can hand the
// file to the service account.
func ResolveHome() (string, *install.ExistingInstall) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		userHome = "" // no home directory: install detection and the ~/.agency fallback go without it
	}
	inst := detectInstall(userHome)
	if home := os.Getenv(global.EnvVarHome); home != "" {
		return home, inst
	}
	if inst != nil && inst.AgencyHome != "" {
		return inst.AgencyHome, inst
	}
	return filepath.Join(userHome, global.DefaultDataDir), inst
}

// detectInstall finds the installed service; replaced in tests so they do not
// consult the real machine.
var detectInstall = install.DetectExistingInstall

// SecretsFileMode is security.permissions.secrets.file_mode from the config
// file in home (its built-in default when there is none), the mode the
// credentials file is written with. Only that block is read: the rest of the
// config may reference secrets this shell cannot resolve.
func SecretsFileMode(home string) (fs.FileMode, error) {
	p, err := config.ReadPermissions(filepath.Join(home, "config.json"))
	if err != nil {
		return 0, fmt.Errorf("read security.permissions from %s: %w", filepath.Join(home, "config.json"), err)
	}
	return p.Modes(config.AreaSecrets).File, nil
}
