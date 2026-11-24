#!/bin/sh
# The installer is now agency.sh. This runs it with the same arguments and
# environment, so existing instructions keep working.
set -eu

case "$0" in
*claweh.sh)
	here="$(dirname "$0")"
	if [ -f "$here/agency.sh" ]; then
		exec sh "$here/agency.sh" "$@"
	fi
	;;
esac

url="https://raw.githubusercontent.com/BisonNine/agency/main/agency.sh"
tmp="$(mktemp 2>/dev/null || mktemp -t agency)" || { echo "error: cannot create temp file" >&2; exit 1; }
trap 'rm -f "$tmp"' EXIT INT TERM
if command -v curl >/dev/null 2>&1; then
	curl -fsSL -o "$tmp" "$url"
elif command -v wget >/dev/null 2>&1; then
	wget -qO "$tmp" "$url"
else
	echo "error: need 'curl' or 'wget' to download $url" >&2
	exit 1
fi
sh "$tmp" "$@"
