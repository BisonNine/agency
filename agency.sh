#!/bin/sh
# Agency installer — downloads the prebuilt `agency` binary (and license) for your
# platform from GitHub Releases and installs it.
#
#   curl -fsSL https://raw.githubusercontent.com/BisonNine/agency/main/agency.sh | sh
#
# Environment overrides:
#   AGENCY_VERSION       release tag to install (e.g. 0.6.6); default: latest
#   AGENCY_INSTALL_DIR   directory to install the binary into; default:
#                        /usr/local/bin (falls back to ~/.local/bin if unwritable)
#
# Only the `agency` binary (and license notices) is installed — the data directory
# (~/.agency) is never touched. To try a build without disturbing an existing
# install, point it at a throwaway dir and check the version:
#
#   AGENCY_INSTALL_DIR="$HOME/agency-test/bin" sh -c "$(curl -fsSL .../agency.sh)"
#   ~/agency-test/bin/agency version
#
# Supports Linux and macOS on amd64 (x86_64) and arm64 (aarch64 / Apple Silicon).
set -eu

REPO="BisonNine/agency"
BINARY="agency"

VERSION="${AGENCY_VERSION:-latest}"

err() { printf '%s\n' "$*" >&2; }
die() { err "error: $*"; exit 1; }

# --- detect OS -------------------------------------------------------------
os="$(uname -s)"
case "$os" in
	Linux)  os="linux" ;;
	Darwin) os="darwin" ;;
	*) die "unsupported OS '$os' (this installer supports Linux and macOS)" ;;
esac

# --- detect architecture ---------------------------------------------------
arch="$(uname -m)"
case "$arch" in
	x86_64 | amd64)  arch="amd64" ;;
	aarch64 | arm64) arch="arm64" ;;
	*) die "unsupported architecture '$arch' (supported: amd64, arm64)" ;;
esac

rel_url() { # $1 = asset filename → full release download URL
	if [ "$VERSION" = "latest" ]; then
		printf 'https://github.com/%s/releases/latest/download/%s' "$REPO" "$1"
	else
		printf 'https://github.com/%s/releases/download/%s/%s' "$REPO" "$VERSION" "$1"
	fi
}

# The per-platform archive each release publishes.
platform_asset="${BINARY}-${os}-${arch}.tar.gz"

# --- pick a downloader -----------------------------------------------------
if command -v curl >/dev/null 2>&1; then
	download() { curl -fSL --retry 3 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
	download() { wget -O "$2" "$1"; }
else
	die "need 'curl' or 'wget' to download the release"
fi

# --- download + extract ----------------------------------------------------
tmpdir="$(mktemp -d 2>/dev/null || mktemp -d -t agency)" || die "cannot create temp dir"
trap 'rm -rf "$tmpdir"' EXIT INT TERM

printf 'Downloading agency for %s/%s (%s)...\n' "$os" "$arch" "$VERSION"
download "$(rel_url "$platform_asset")" "$tmpdir/pkg.tar.gz" ||
	die "could not download a release package for ${os}/${arch}; see https://github.com/${REPO}/releases"

mkdir -p "$tmpdir/x"
tar -xzf "$tmpdir/pkg.tar.gz" -C "$tmpdir/x" || die "could not extract $platform_asset"

# Locate the agency binary inside the archive — named 'agency' or 'agency-<os>-<arch>'
# (never agency-auth). Search so the archive layout (flat or nested) doesn't matter.
bin="$(find "$tmpdir/x" -type f \( -name "$BINARY" -o -name "${BINARY}-${os}-${arch}" \) 2>/dev/null | head -n 1)"
[ -n "$bin" ] || die "no '$BINARY' binary found inside $platform_asset"
chmod +x "$bin"

# --- install the binary ----------------------------------------------------
# Try the target dir directly, then with sudo, then (only for the default) a
# per-user bin dir — so the install works with or without root.
dest_dir="${AGENCY_INSTALL_DIR:-/usr/local/bin}"

install_bin() { # $1=dir  $2=sudo-cmd
	$2 mkdir -p "$1" 2>/dev/null || return 1
	$2 cp "$bin" "$1/$BINARY" 2>/dev/null || return 1
	$2 chmod +x "$1/$BINARY" 2>/dev/null || return 1
	return 0
}

printf 'Installing to %s...\n' "$dest_dir"
sudo=""
if install_bin "$dest_dir" ""; then
	:
elif command -v sudo >/dev/null 2>&1 && install_bin "$dest_dir" "sudo"; then
	sudo="sudo"
elif [ -z "${AGENCY_INSTALL_DIR:-}" ] && install_bin "$HOME/.local/bin" ""; then
	dest_dir="$HOME/.local/bin"
else
	die "could not install to $dest_dir — set AGENCY_INSTALL_DIR to a writable dir, or re-run with sudo"
fi
dest="$dest_dir/$BINARY"

# --- install the license/notices so they travel with the software ----------
# MIT (and the bundled third-party notices, incl. MPL-2.0) must accompany the
# binary. Try a system doc dir, then a per-user one, so they always land on disk.
licenses="$(find "$tmpdir/x" -type f \( -iname "LICENSE*" -o -iname "*LICENSES*" \) 2>/dev/null)"
license_dest=""
if [ -n "$licenses" ]; then
	for d in "${dest_dir%/bin}/share/doc/agency" "$HOME/.local/share/doc/agency"; do
		$sudo mkdir -p "$d" 2>/dev/null || continue
		for f in $licenses; do $sudo cp "$f" "$d/" 2>/dev/null || true; done
		if ls "$d"/LICENSE* >/dev/null 2>&1; then license_dest="$d"; break; fi
	done
fi

# --- verify + PATH hint ----------------------------------------------------
if "$dest" version >/dev/null 2>&1; then
	printf 'Installed: %s\n' "$("$dest" version 2>/dev/null | head -n 1)"
else
	printf 'Installed agency -> %s\n' "$dest"
fi

case ":$PATH:" in
	*":$dest_dir:"*) : ;;
	*)
		err ""
		err "note: $dest_dir is not on your PATH. Add it, e.g.:"
		err "  export PATH=\"$dest_dir:\$PATH\""
		;;
esac

# Always advise where the license and third-party notices are.
printf '\nAgency is MIT-licensed and bundles third-party open-source components.\n'
if [ -n "$license_dest" ]; then
	printf 'License and third-party notices: %s\n' "$license_dest"
else
	printf 'License and third-party notices: in the release archive and at https://github.com/%s\n' "$REPO"
fi

printf '\nDone. Next: run `agency install` to set up the service and create the WebUI admin\n'
printf 'account (Linux), or `agency --help`. Running `agency` without the service? Create the\n'
printf 'account with `agency admin` first; the WebUI requires it.\n'
