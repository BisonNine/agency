#!/bin/sh
# `make test-deb`: installs the Debian package built from this tree with apt
# in disposable containers and checks its maintainer scripts and the
# root-run commands on its install (tests/deb/harness.sh). Nothing runs on
# this machine but the container tool; every container is removed.
#
# Usage: tests/deb/run.sh <dir with linux agency [and agency-auth]> [image...]
set -eu

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
bins=${1:?usage: run.sh <binary dir> [image...]}
shift
images=${*:-debian:trixie-slim ubuntu:24.04}

if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    ct=docker
elif command -v podman >/dev/null 2>&1 && podman info >/dev/null 2>&1; then
    ct=podman
else
    echo "test-deb: neither Docker nor Podman is available; nothing was tested."
    exit 2
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/agency-test-deb.XXXXXX")
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/scripts" "$work/logs"
cp "$bins/agency" "$work/agency"
[ -f "$bins/agency-auth" ] && cp "$bins/agency-auth" "$work/agency-auth"
for s in preinst postinst prerm postrm; do
    [ -f "$repo/packaging/deb/$s" ] && cp "$repo/packaging/deb/$s" "$work/scripts/$s"
done
cp "$here/harness.sh" "$here/systemctl" "$work/"
chmod -R a+rX "$work"

failed=0
summary=""
for img in $images; do
    log="$work/logs/$(echo "$img" | tr '/:' '__').log"
    echo "test-deb: $img ..."
    if "$ct" run --rm -v "$work:/work:ro" "$img" bash /work/harness.sh >"$log" 2>&1; then
        result="passed"
    else
        result="FAILED"
        failed=1
        grep -B8 -E '^(FAIL|ABORT)' "$log" | sed 's/^/  /'
    fi
    counts=$(grep -E '^RESULT' "$log" | tail -1 | sed 's/^RESULT: //')
    summary="$summary $img: $result (${counts:-no result line});"
    if [ "$result" = FAILED ]; then
        echo "  last lines of the $img log:"
        tail -n 40 "$log" | sed 's/^/  | /'
    fi
done

echo "test-deb:$summary"
exit "$failed"
