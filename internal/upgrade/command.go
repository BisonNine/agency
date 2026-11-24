// Package upgrade provides the `agency upgrade` subcommand, which downloads
// the latest release binary from GitHub, verifies it against the release's
// minisign-signed checksum list (keys in app/keys.go), atomically updates the
// running executable, and restarts any active background service.
package upgrade

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"go.bison9.com/agency/app"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/internal/install"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/utils"
)

const (
	repoOwner   = "BisonNine"
	repoName    = global.ReleaseRepoName
	httpTimeout = 60 * time.Second

	// Every release ships one signed checksum list covering all its archives.
	checksumsAssetName = "checksums.txt"
	signatureAssetName = checksumsAssetName + ".minisig"
)

// The release source and the machine the upgrade acts on. Variables only so
// tests can point an upgrade at a fake release server, binary and service
// manager instead of GitHub and the real system.
var (
	apiBaseURL    = "https://api.github.com/repos/" + repoOwner + "/" + repoName
	executable    = os.Executable
	detectInstall = install.DetectExistingInstall
	releaseKeys   = app.ReleasePublicKeys
	goos          = runtime.GOOS
	geteuid       = os.Geteuid
	getuid        = os.Getuid
	// runCommand runs a service manager command (systemctl, launchctl).
	runCommand = func(name string, args ...string) error {
		return exec.Command(name, args...).Run() //nolint:gosec // fixed service manager binaries with arguments built by the upgrader
	}
	// commandOutput runs a service manager command and returns its stdout.
	commandOutput = func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).Output() //nolint:gosec // fixed service manager binaries with arguments built by the upgrader
	}
)

// GitHubRelease represents the GitHub releases API response.
type GitHubRelease struct {
	TagName string        `json:"tag_name"`
	Name    string        `json:"name"`
	Assets  []GitHubAsset `json:"assets"`
}

// GitHubAsset represents an asset attached to a release.
type GitHubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// NewUpgradeCommand returns the `agency upgrade` subcommand.
func NewUpgradeCommand() *cobra.Command {
	var (
		checkOnly     bool
		force         bool
		targetVersion string
		yes           bool
	)

	cmd := &cobra.Command{
		Use:     "upgrade",
		Aliases: []string{"update"},
		Short:   "Download the latest release binary from GitHub and install it",
		Long: "Checks for the latest release on GitHub (" + repoOwner + "/" + repoName + "),\n" +
			"verifies the release's signed checksum list (" + signatureAssetName + ") with the\n" +
			"publisher key built into this binary, checks the archive against it, atomically\n" +
			"replaces the currently running binary, and restarts the background service if\n" +
			"one is active. A release without a valid signature is refused.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runUpgrade(checkOnly, force, targetVersion, yes)
		},
	}

	cmd.Flags().BoolVar(&checkOnly, "check", false, "Check for available updates without downloading or installing.")
	cmd.Flags().BoolVar(&force, "force", false, "Force download and reinstall even if already running the target version.")
	cmd.Flags().StringVar(&targetVersion, "version", "", "Install a specific version tag instead of the latest release.")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip interactive confirmation prompt.")

	return cmd
}

func isPackageManagedBinary(osName, binPath string) bool {
	if osName != "linux" || binPath == "" {
		return false
	}
	dir := filepath.Dir(filepath.Clean(binPath))
	return dir == "/usr/bin" || dir == "/bin"
}

func packageManagedUpgradeErr(binPath string) error {
	return fmt.Errorf(
		"%s at %s was installed by the system package manager.\n"+
			"Upgrade it with apt instead:\n"+
			"  sudo apt update && sudo apt install --only-upgrade %s",
		app.ShortName(), binPath, global.DebPackageName)
}

func runningPackageManagedBinary() string {
	p, err := executable()
	if err != nil {
		return ""
	}
	if resolved, rerr := filepath.EvalSymlinks(p); rerr == nil {
		p = resolved
	}
	if isPackageManagedBinary(goos, p) {
		return p
	}
	return ""
}

func runUpgrade(checkOnly, force bool, targetVersion string, autoYes bool) error {
	if !checkOnly {
		if pkgBin := runningPackageManagedBinary(); pkgBin != "" {
			return packageManagedUpgradeErr(pkgBin)
		}
	}

	currentVer := app.SemVer()
	fmt.Printf("Current %s version: v%s\n", app.Name(), currentVer)
	fmt.Println("Checking for releases on GitHub...")

	release, err := fetchRelease(targetVersion)
	if err != nil {
		return fmt.Errorf("fetching release information: %w", err)
	}

	latestVer := strings.TrimPrefix(release.TagName, "v")
	cmp := compareSemVer(currentVer, latestVer)

	if checkOnly {
		if cmp < 0 {
			fmt.Printf("A newer version of %s is available: v%s -> v%s\n", app.ShortName(), currentVer, latestVer)
			if runningPackageManagedBinary() != "" {
				fmt.Printf("Run `sudo apt update && sudo apt install --only-upgrade %s` to install it.\n", global.DebPackageName)
			} else {
				fmt.Printf("Run `%s upgrade` to install it.\n", app.BinaryName())
			}
		} else if cmp > 0 {
			fmt.Printf("%s is running a newer version than latest release (v%s vs release v%s).\n", app.ShortName(), currentVer, latestVer)
		} else {
			fmt.Printf("%s is already up to date (v%s).\n", app.ShortName(), currentVer)
		}
		return nil
	}

	if cmp >= 0 && !force && targetVersion == "" {
		fmt.Printf("%s is already up to date (v%s).\nUse `%s upgrade --force` to reinstall.\n", app.ShortName(), currentVer, app.BinaryName())
		return nil
	}

	archiveName := fmt.Sprintf("%s-%s-%s.tar.gz", global.InstalledBinaryName, runtime.GOOS, runtime.GOARCH)
	assets, err := findReleaseAssets(release, archiveName)
	if err != nil {
		return err
	}

	// Locate running executable
	exePath, err := executable()
	if err != nil {
		return fmt.Errorf("locating current executable: %w", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(exePath); rerr == nil {
		exePath = resolved
	}

	// Check if an existing installation exists on the system
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("determining home directory: %w", err)
	}
	existing := detectInstall(homeDir)
	if existing != nil && existing.BinaryPath != "" && (isBuildOrTempDir(exePath) || !fileExists(exePath)) {
		fmt.Printf("Detected installed %s at: %s\n", app.ShortName(), existing.BinaryPath)
		exePath = existing.BinaryPath
	}

	// Set AGENCY_HOME if custom install path is detected
	if existing != nil && existing.AgencyHome != "" {
		if envErr := setAgencyHome(existing.AgencyHome); envErr != nil {
			return envErr
		}
	} else if strings.HasPrefix(exePath, global.SystemDataDir) {
		if envErr := setAgencyHome(global.SystemDataDir); envErr != nil {
			return envErr
		}
	}

	// Verify write permission on target executable directory
	if isPackageManagedBinary(goos, exePath) {
		return packageManagedUpgradeErr(exePath)
	}
	binDir := filepath.Dir(exePath)
	if wErr := checkDirWritable(binDir); wErr != nil {
		return fmt.Errorf("permission denied writing to %s.\nPlease run with sudo: sudo %s upgrade", binDir, app.BinaryName())
	}

	// Summary & confirmation
	fmt.Printf("\n%s Upgrade Summary:\n", app.Name())
	fmt.Printf("  Current Version: v%s\n", currentVer)
	fmt.Printf("  Target Version:  v%s (tag: %s)\n", latestVer, release.TagName)
	fmt.Printf("  Release Asset:   %s\n", assets.archive.Name)
	fmt.Printf("  Target Binary:   %s\n", exePath)
	if existing != nil && existing.AgencyHome != "" {
		fmt.Printf("  Data Directory:  %s\n", existing.AgencyHome)
	} else if strings.HasPrefix(exePath, global.SystemDataDir) {
		fmt.Printf("  Data Directory:  %s\n", global.SystemDataDir)
	}
	if existing != nil && existing.ServicePath != "" {
		activeStr := "inactive"
		if existing.IsActive {
			activeStr = "active"
		}
		fmt.Printf("  Existing Service: %s (%s, %s)\n", existing.ServicePath, existing.ServiceType, activeStr)
	}
	fmt.Println()

	if !autoYes {
		confirmed, promptErr := confirmPrompt("Do you want to proceed with the upgrade?")
		if promptErr != nil || !confirmed {
			fmt.Println("Upgrade cancelled.")
			return nil
		}
	}

	// Create temp directory for download
	tmpDir, err := os.MkdirTemp("", "agency-upgrade-*")
	if err != nil {
		return fmt.Errorf("creating temp directory: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(tmpDir); rmErr != nil {
			fmt.Printf("Warning: could not remove temp directory %s (%v).\n", tmpDir, rmErr)
		}
	}()

	// Download archive
	archivePath := filepath.Join(tmpDir, archiveName)
	fmt.Printf("Downloading %s...\n", assets.archive.Name)
	if dlErr := downloadFile(assets.archive.BrowserDownloadURL, archivePath); dlErr != nil {
		return fmt.Errorf("downloading %s: %w", assets.archive.Name, dlErr)
	}

	// Verify the signed checksum list, then the archive against it. Both the
	// list and its signature are mandatory: a checksum from the same untrusted
	// download location proves nothing on its own.
	fmt.Println("Verifying release signature and SHA256 checksum...")
	checksums, err := downloadBytes(assets.checksums.BrowserDownloadURL)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", checksumsAssetName, err)
	}
	signature, err := downloadBytes(assets.signature.BrowserDownloadURL)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", signatureAssetName, err)
	}
	if err = verifySignedArchive(releaseKeys(), checksums, signature, archiveName, archivePath); err != nil {
		return fmt.Errorf("release verification failed: %w", err)
	}
	fmt.Println("Signature and checksum verified.")

	// Extract binary from tar.gz
	fmt.Println("Extracting binary...")
	extractedBinPath := filepath.Join(tmpDir, "extracted-agency")
	extractedAuthPath := filepath.Join(tmpDir, "extracted-agency-auth")
	hasAuth, err := extractBinariesFromTarGz(archivePath, extractedBinPath, extractedAuthPath)
	if err != nil {
		return fmt.Errorf("extracting binary from archive: %w", err)
	}

	// Atomically replace executable
	fmt.Printf("Replacing %s...\n", exePath)
	if err := atomicReplace(extractedBinPath, exePath); err != nil {
		return fmt.Errorf("replacing binary at %s: %w", exePath, err)
	}

	// If agency-auth was in the archive and exists alongside agency, update it too
	authDest := filepath.Join(binDir, global.AuthHelperBinaryName)
	if hasAuth && fileExists(authDest) {
		if err := atomicReplace(extractedAuthPath, authDest); err == nil {
			fmt.Printf("Updated companion tool: %s\n", authDest)
		}
	}

	fmt.Printf("Successfully upgraded %s to v%s!\n", app.Name(), latestVer)

	// Restart active background service
	restartActiveService()

	return nil
}

// releaseAssets are the three assets an upgrade needs from a release.
type releaseAssets struct {
	archive   *GitHubAsset
	checksums *GitHubAsset
	signature *GitHubAsset
}

// findReleaseAssets picks the platform archive, the checksum list and its
// signature out of a release. All three must be present; a release missing
// the signed list cannot be verified and is not installable.
func findReleaseAssets(release *GitHubRelease, archiveName string) (*releaseAssets, error) {
	var found releaseAssets
	for i := range release.Assets {
		asset := &release.Assets[i]
		switch asset.Name {
		case archiveName:
			found.archive = asset
		case checksumsAssetName:
			found.checksums = asset
		case signatureAssetName:
			found.signature = asset
		}
	}
	if found.archive == nil {
		return nil, fmt.Errorf("no release binary found for platform %s/%s (%s)", runtime.GOOS, runtime.GOARCH, archiveName)
	}
	if found.checksums == nil || found.signature == nil {
		return nil, fmt.Errorf("release %s does not ship %s and %s; refusing to install an unverifiable release", release.TagName, checksumsAssetName, signatureAssetName)
	}
	return &found, nil
}

func fetchRelease(version string) (*GitHubRelease, error) {
	client := &http.Client{Timeout: httpTimeout}

	url := apiBaseURL + "/releases/latest"
	if version != "" {
		tag := version
		if !strings.HasPrefix(tag, "v") {
			tag = "v" + tag
		}
		url = apiBaseURL + "/releases/tags/" + tag
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", fmt.Sprintf("%s/%s (%s; %s)", app.Name(), app.SemVer(), runtime.GOOS, runtime.GOARCH))

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer closeBody(resp)

	// Fallback without "v" prefix if 404 when querying specific version
	if resp.StatusCode == http.StatusNotFound && version != "" && strings.HasPrefix(url, apiBaseURL+"/releases/tags/v") {
		bareURL := apiBaseURL + "/releases/tags/" + strings.TrimPrefix(version, "v")
		if bareReq, bErr := http.NewRequest(http.MethodGet, bareURL, nil); bErr == nil {
			bareReq.Header.Set("Accept", "application/vnd.github.v3+json")
			bareReq.Header.Set("User-Agent", req.Header.Get("User-Agent"))
			if bareResp, brErr := client.Do(bareReq); brErr == nil && bareResp.StatusCode == http.StatusOK {
				defer closeBody(bareResp)
				var rel GitHubRelease
				if decErr := json.NewDecoder(bareResp.Body).Decode(&rel); decErr == nil {
					return &rel, nil
				}
			}
		}
	}

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("GitHub API returned HTTP %d (reading body: %w)", resp.StatusCode, readErr)
		}
		return nil, fmt.Errorf("GitHub API returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var rel GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("parsing release JSON: %w", err)
	}
	return &rel, nil
}

func downloadFile(url, dstPath string) error {
	client := &http.Client{Timeout: httpTimeout}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", app.Name()+"/"+app.SemVer())

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer closeBody(resp)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d downloading %s", resp.StatusCode, url)
	}

	out, err := os.Create(dstPath) //nolint:gosec // file inside the upgrader's own temp dir
	if err != nil {
		return err
	}

	if _, err = io.Copy(out, resp.Body); err != nil {
		utils.CloseQuietly(out)
		return err
	}
	return out.Close()
}

// downloadBytes fetches a small release asset (the checksum list and its
// signature) into memory.
func downloadBytes(url string) ([]byte, error) {
	client := &http.Client{Timeout: httpTimeout}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", app.Name()+"/"+app.SemVer())

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer closeBody(resp)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d downloading %s", resp.StatusCode, url)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func computeSHA256(filePath string) (string, error) {
	f, err := os.Open(filePath) //nolint:gosec // file inside the upgrader's own temp dir
	if err != nil {
		return "", err
	}
	defer utils.CloseQuietly(f)

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func extractBinariesFromTarGz(archivePath, agencyDst, agencyAuthDst string) (hasAuth bool, err error) {
	f, err := os.Open(archivePath) //nolint:gosec // file inside the upgrader's own temp dir
	if err != nil {
		return false, err
	}
	defer utils.CloseQuietly(f)

	gz, err := gzip.NewReader(f)
	if err != nil {
		return false, err
	}
	defer utils.CloseQuietly(gz)

	tr := tar.NewReader(gz)
	foundClaw := false

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, err
		}

		cleanName := filepath.Base(hdr.Name)
		if cleanName == global.InstalledBinaryName && (hdr.Typeflag == tar.TypeReg) {
			out, err := os.OpenFile(agencyDst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755) //nolint:gosec // installed binary must be executable
			if err != nil {
				return false, err
			}
			if _, err := io.Copy(out, tr); err != nil { //nolint:gosec // release tarball whose SHA-256 was verified against the minisign-signed checksum list
				utils.CloseQuietly(out)
				return false, err
			}
			if closeErr := out.Close(); closeErr != nil {
				return false, closeErr
			}
			foundClaw = true
		} else if cleanName == global.AuthHelperBinaryName && (hdr.Typeflag == tar.TypeReg) {
			out, err := os.OpenFile(agencyAuthDst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755) //nolint:gosec // installed binary must be executable
			if err != nil {
				return false, err
			}
			if _, err := io.Copy(out, tr); err != nil { //nolint:gosec // release tarball whose SHA-256 was verified against the minisign-signed checksum list
				utils.CloseQuietly(out)
				return false, err
			}
			if closeErr := out.Close(); closeErr != nil {
				return false, closeErr
			}
			hasAuth = true
		}
	}

	if !foundClaw {
		return false, fmt.Errorf("`%s` binary was not found inside archive %s", global.InstalledBinaryName, archivePath)
	}
	return hasAuth, nil
}

func atomicReplace(srcPath, dstPath string) error {
	tmpDst := dstPath + ".new." + strconv.Itoa(os.Getpid())
	data, err := os.ReadFile(srcPath) //nolint:gosec // srcPath is the binary extracted into the upgrader's temp dir
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmpDst, data, 0o755); err != nil { //nolint:gosec // installed binary must be executable
		return err
	}
	// Preserve existing file ownership if dstPath exists
	if origInfo, statErr := os.Stat(dstPath); statErr == nil {
		if stat, ok := origInfo.Sys().(*syscall.Stat_t); ok {
			if chownErr := os.Chown(tmpDst, int(stat.Uid), int(stat.Gid)); chownErr != nil {
				fmt.Printf("Warning: could not preserve ownership of %s (%v).\n", dstPath, chownErr)
			}
		}
	}
	return os.Rename(tmpDst, dstPath)
}

func checkDirWritable(dir string) error {
	testFile := filepath.Join(dir, fmt.Sprintf(".agency-perm-test-%d", os.Getpid()))
	f, err := os.OpenFile(testFile, os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // probe file with a generated name in the install dir
	if err != nil {
		return err
	}
	utils.CloseQuietly(f)
	if rmErr := os.Remove(testFile); rmErr != nil {
		fmt.Printf("Warning: could not remove %s (%v).\n", testFile, rmErr)
	}
	return nil
}

// setAgencyHome exports dir as the data directory.
func setAgencyHome(dir string) error {
	if err := os.Setenv(global.EnvVarHome, dir); err != nil {
		return fmt.Errorf("setting %s: %w", global.EnvVarHome, err)
	}
	return nil
}

// closeBody closes an HTTP response body, logging a failure at debug level
// since nothing the caller does depends on it.
func closeBody(resp *http.Response) {
	if err := resp.Body.Close(); err != nil {
		logger.DebugCF("upgrade", "response body close failed", map[string]any{"error": err.Error()})
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func isBuildOrTempDir(path string) bool {
	clean := filepath.Clean(path)
	parts := strings.SplitSeq(clean, string(filepath.Separator))
	for p := range parts {
		if p == "build" || p == "tmp" || p == "temp" || strings.HasPrefix(p, "agency-upgrade-") {
			return true
		}
	}
	return underTempDir(clean)
}

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

func confirmPrompt(prompt string) (bool, error) {
	fmt.Printf("%s [y/N]: ", prompt)
	reader := bufio.NewReader(os.Stdin)
	response, err := reader.ReadString('\n')
	if err != nil {
		return false, nil //nolint:nilerr // an unreadable answer means "no"
	}
	response = strings.TrimSpace(strings.ToLower(response))
	return response == "y" || response == "yes", nil
}

// compareSemVer compares two SemVer strings (e.g. "0.5.3" and "0.5.4").
// Returns -1 if v1 < v2, 0 if v1 == v2, 1 if v1 > v2.
func compareSemVer(v1, v2 string) int {
	clean1 := strings.TrimPrefix(strings.Split(v1, "+")[0], "v")
	clean2 := strings.TrimPrefix(strings.Split(v2, "+")[0], "v")

	parts1 := strings.Split(clean1, ".")
	parts2 := strings.Split(clean2, ".")

	maxParts := max(len(parts2), len(parts1))

	for i := range maxParts {
		var n1, n2 int
		if i < len(parts1) {
			if n, convErr := strconv.Atoi(parts1[i]); convErr == nil {
				n1 = n
			}
		}
		if i < len(parts2) {
			if n, convErr := strconv.Atoi(parts2[i]); convErr == nil {
				n2 = n
			}
		}
		if n1 < n2 {
			return -1
		}
		if n1 > n2 {
			return 1
		}
	}
	return 0
}

// upgradeServiceName is the systemd unit `install` registers; a constant, so
// the systemctl arguments are fixed.
const upgradeServiceName = global.ServiceName

func restartActiveService() {
	switch goos {
	case "linux":
		// Check user service
		if err := runCommand("systemctl", "--user", "is-active", "--quiet", upgradeServiceName); err == nil {
			fmt.Printf("Restarting systemd user service %s...\n", upgradeServiceName)
			if rErr := runCommand("systemctl", "--user", "restart", upgradeServiceName); rErr == nil {
				fmt.Println("User service restarted successfully.")
				return
			}
		}
		// Check system service
		if err := runCommand("systemctl", "is-active", "--quiet", upgradeServiceName); err == nil {
			if geteuid() == 0 {
				fmt.Printf("Restarting systemd system service %s...\n", upgradeServiceName)
				if rErr := runCommand("systemctl", "restart", upgradeServiceName); rErr == nil {
					fmt.Println("System service restarted successfully.")
					return
				}
			} else {
				fmt.Printf("Note: System service %s is active. Run `sudo systemctl restart %s` to apply the update.\n", upgradeServiceName, upgradeServiceName)
			}
		}
	case "darwin":
		out, err := commandOutput("launchctl", "list")
		if err != nil || !strings.Contains(string(out), global.LaunchdLabel) {
			return
		}
		label := global.LaunchdLabel
		fmt.Printf("Restarting launchd service %s...\n", label)
		uid := strconv.Itoa(getuid())
		if kErr := runCommand("launchctl", "kickstart", "-k", "gui/"+uid+"/"+label); kErr == nil {
			fmt.Println("Launchd service restarted successfully.")
		} else if sysErr := runCommand("launchctl", "kickstart", "-k", "system/"+label); sysErr != nil {
			fmt.Printf("Warning: could not restart launchd service %s (%v). Restart it manually.\n", label, sysErr)
		}
	}
}
