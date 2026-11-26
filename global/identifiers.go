// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package global

// Fixed identifiers: the names of real files, services and protocol keys that
// installs, scripts and other programs already depend on. They are not the
// product's label — that is app.Name / app.ShortName — and they do not change
// when the product is renamed. Each is defined here once; the product-name
// guard (app/productname_guard_test.go) exempts this file and nothing else.
const (
	// InstalledBinaryName is the file name of the installed binary and of the
	// binary inside a release archive. It is not what the user typed; that is
	// app.BinaryName.
	InstalledBinaryName = "agency"

	// AuthHelperBinaryName is the companion OAuth helper shipped beside the
	// main binary.
	AuthHelperBinaryName = "agency-auth"

	// ServiceName is the systemd unit name `install` registers.
	ServiceName = "agency"

	// SystemdUnitFile is the unit file name for ServiceName.
	SystemdUnitFile = ServiceName + ".service"

	// LaunchdLabel is the launchd job label `install` registers on macOS.
	LaunchdLabel = "com.bison9.agency"

	// SystemDataDir is the data directory of a system-wide install.
	SystemDataDir = "/opt/agency"

	// LogFileName is the main log file under <AGENCY_HOME>/logs.
	LogFileName = "agency.log"

	// LockFileName is the single-instance lock file in the data directory.
	LockFileName = "agency.lock"

	// HostMCPServerKey is the name this host's MCP server is registered under
	// in each CLI's own configuration (`claude mcp add ... agency ...`).
	HostMCPServerKey = "agency"

	// HostMCPToolPrefix is the prefix CLI agents see on this host's MCP tools:
	// the MCP server is registered under HostMCPServerKey, and the CLIs name
	// its tools mcp__<key>__<tool>. Changing it would rename every host tool.
	HostMCPToolPrefix = "mcp__" + HostMCPServerKey + "__"

	// HostMCPOpenCodeToolPrefix is the prefix OpenCode shows this host's MCP
	// tools under: it names a server's tools <key>_<tool> (with the server's
	// Code Mode off), so the server registered as HostMCPServerKey gives
	// agency_<tool>.
	HostMCPOpenCodeToolPrefix = HostMCPServerKey + "_"

	// HostMCPRealm is the realm of the WWW-Authenticate challenge the host's
	// bearer MCP endpoint answers a refused request with.
	HostMCPRealm = "agency-mcp"

	// ReleaseRepoName is the GitHub repository `upgrade` downloads releases
	// from (owner BisonNine).
	ReleaseRepoName = "agency"

	// DebPackageName is the Debian/Ubuntu binary package name (`apt install agency`).
	DebPackageName = "agency"

	// ServiceAccount is the system user and group the Debian package creates
	// and runs the service as.
	ServiceAccount = "agency"

	// ServiceAccountHome is the home directory a new ServiceAccount gets. It
	// is not the data directory: file modes there are set at every start,
	// which would take the execute bits off programs installed in a home.
	ServiceAccountHome = "/home/agency"

	// PackageUnitMarker is the first line of the systemd unit the Debian
	// package has the binary write (`install --package`). The package's
	// maintainer scripts match it too: a unit without it is someone else's
	// install, which the package leaves alone.
	PackageUnitMarker = "# Managed by the agency Debian package"

	// MaestroHostModel is the model label Maestro records for a task the host
	// ran without naming a model; Maestro stores it and hands it back as a
	// task's llm_model_id.
	MaestroHostModel = "agency"

	// FolderWatchMarkerHeader is the first line of a folder-watch marker file.
	// The watcher parses it; a different header discards existing markers.
	FolderWatchMarkerHeader = "# agency seen files v1"

	// InstallRCMarker is the comment `install` writes above the lines it adds
	// to the user's shell rc file.
	InstallRCMarker = "# Added by agency install"

	// DefaultAgentID is the ID of the agent a new configuration is seeded
	// with. An agent ID is a directory name and a routing key, so it does not
	// follow the product's label; the agent's display name does.
	DefaultAgentID = "agency"

	// SelfSignedFallbackCN is the common name of a generated self-signed
	// certificate that has no host name or address to carry.
	SelfSignedFallbackCN = "agency"
)
