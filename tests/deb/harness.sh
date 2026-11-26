#!/bin/bash
# Runs inside a disposable Debian or Ubuntu container, as root (see run.sh):
# builds the agency package from /work (the binaries, the maintainer scripts
# in /work/scripts), installs, upgrades, removes and purges it with apt, and
# runs the operator's root-run commands on its install. /work/systemctl
# stands in for systemd: it runs the unit's ExecStart as its User=.
# Prints PASS/FAIL per check and a RESULT line; exits non-zero on a failure.
set -u
PASSES=0
FAILS=0
pass() { echo "PASS: $*"; PASSES=$((PASSES + 1)); }
fail() { echo "FAIL: $*"; FAILS=$((FAILS + 1)); }
check() { local what=$1; shift; if "$@"; then pass "$what"; else fail "$what"; fi; }
finish() { echo; echo "RESULT: $PASSES passed, $FAILS failed"; [ "$FAILS" = 0 ]; exit $?; }

export DEBIAN_FRONTEND=noninteractive
if ! { apt-get update -qq >/dev/null && apt-get install -y -qq adduser curl procps ca-certificates >/dev/null; }; then
    echo "ABORT: apt-get could not install the harness's tools"
    fail "harness setup"
    finish
fi
install -m 0755 /work/systemctl /usr/bin/systemctl
mkdir -p /run/systemd/system /etc/systemd/system

UNIT=/etc/systemd/system/agency.service
MARKER='# Managed by the agency Debian package'
PASSWORD='a long password 1'

mkdeb() { # version
    local v=$1 d=/tmp/pkg-$1
    rm -rf "$d"; mkdir -p "$d/DEBIAN" "$d/usr/bin"
    install -m 0755 /work/agency "$d/usr/bin/agency"
    [ -f /work/agency-auth ] && install -m 0755 /work/agency-auth "$d/usr/bin/agency-auth"
    ln -s agency "$d/usr/bin/openclaw"
    for s in preinst postinst prerm postrm; do
        [ -f "/work/scripts/$s" ] && install -m 0755 "/work/scripts/$s" "$d/DEBIAN/$s"
    done
    cat >"$d/DEBIAN/control" <<EOF
Package: agency
Version: $v
Architecture: $(dpkg --print-architecture)
Maintainer: Test <test@example.invalid>
Depends: ca-certificates, adduser
Description: agency package built by tests/deb
EOF
    dpkg-deb --root-owner-group -b "$d" "/tmp/agency_$v.deb" >/dev/null
}
mkdeb 0.0.1
mkdeb 0.0.2

# apt runs dpkg on a pseudo-terminal: its output has CRLF line ends.
apt_out() { "$@" 2>&1 | tr -d '\r'; }
# What the maintainer scripts printed: the lines after "Setting up".
script_out() { sed -n '/^Setting up/,$p' | sed 1d | grep -v '^logger:\|^logging to syslog\|returned error: 256$\|^ *returned error'; }

wait_ready() {
    for _ in $(seq 1 150); do
        curl -sf -o /dev/null http://127.0.0.1:18790/ready && return 0
        sleep 0.2
    done
    echo "  service output:"; tail -20 /run/agency-service.out 2>/dev/null | sed 's/^/    > /'
    tail -20 /opt/agency/logs/agency.log 2>/dev/null | sed 's/^/    log> /'
    return 1
}
svc_pid() { cat /run/agency-stub.pid 2>/dev/null; }
svc_user() { ps -o user= -p "$(svc_pid)" 2>/dev/null | tr -d ' '; }
login_code() {
    curl -s -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
        -d "{\"username\":\"alice\",\"password\":\"$PASSWORD\"}" http://127.0.0.1:18790/api/auth/login
}
# The service picks up a new account within a minute.
login_works() {
    for _ in $(seq 1 75); do [ "$(login_code)" = 204 ] && return 0; sleep 1; done
    return 1
}
# Everything under /opt/agency belongs to agency:agency.
all_owned() {
    local bad
    bad=$(find /opt/agency \( ! -user agency -o ! -group agency \) -print)
    [ -z "$bad" ] || { echo "  not agency:agency:"; echo "$bad" | sed 's/^/    /'; return 1; }
}
# The default security.permissions: folders 0700, files 0600, bin/ files 0700.
modes_ok() {
    local bad
    bad=$(find /opt/agency \( -type d ! -perm 0700 \) -o \( -type f ! -path '/opt/agency/bin/*' ! -perm 0600 \) -o \( -type f -path '/opt/agency/bin/*' ! -perm 0700 \))
    [ -z "$bad" ] || { echo "  modes:"; echo "$bad" | while read -r f; do stat -c '    %a %U:%G %n' "$f"; done; return 1; }
}
# A command an operator runs with sudo: root, HOME=/root, SUDO_USER set.
as_root() {
    env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin HOME=/root USER=root LOGNAME=root SUDO_USER=alice TERM=dumb "$@"
}
run_cmd() { # description, command...
    local d=$1; shift
    local o rc
    o=$(as_root "$@" 2>&1); rc=$?
    echo "  \$ $* -> $rc"; echo "$o" | head -6 | sed 's/^/    | /'
    check "after '$d': all agency-owned" all_owned
}
unit_line() { head -n 1 "$UNIT" 2>/dev/null; }

adduser --disabled-password --gecos "" alice >/dev/null 2>&1
chmod 0750 /home/alice

echo "=== fresh install"
out=$(apt_out apt-get install -y -qq /tmp/agency_0.0.1.deb); msg=$(echo "$out" | script_out)
echo "$msg" | sed 's/^/  | /'
check "account agency, home /home/agency, shell /bin/bash" [ "$(getent passwd agency | cut -d: -f6,7)" = "/home/agency:/bin/bash" ]
check "/home/agency belongs to agency" [ "$(stat -c '%U' /home/agency)" = agency ]
check "unit PATH names the account's home" grep -q '^Environment=PATH=/home/agency/.local/bin:/home/agency/bin:' "$UNIT"
check "group agency" getent group agency >/dev/null
pw=$(getent shadow agency | cut -d: -f2)
case "$pw" in '!'* | '*'*) pass "no usable password ($pw)" ;; *) fail "password field is $pw" ;; esac
check "system account (uid < 1000)" [ "$(id -u agency)" -lt 1000 ]
check "/opt/agency agency:agency 0700" [ "$(stat -c '%U:%G %a' /opt/agency)" = "agency:agency 700" ]
check "unit is the package's" [ "$(unit_line)" = "$MARKER" ]
check "unit runs as agency" grep -q '^User=agency$' "$UNIT"
check "unit sets AGENCY_HOME=/opt/agency" grep -q '^Environment=AGENCY_HOME=/opt/agency$' "$UNIT"
check "markers" [ -e /var/lib/agency/created-account ] && [ -e /var/lib/agency/created-group ] && [ -e /var/lib/agency/created-home ] && [ -e /var/lib/agency/created-account-home ]
check "message: installed and running" grep -qx "Agency is installed and running." <<<"$msg"
check "message: account created" grep -qx "Service account:        agency (created)" <<<"$msg"
check "message: where the files are" grep -qx "Data and settings:      /opt/agency" <<<"$msg"
check "message: sudo agency admin" grep -qx "       sudo agency admin" <<<"$msg"
check "message: allow a network" grep -qx "       sudo agency network 192.168.1.0/24    (one network)" <<<"$msg"
check "message: allow one computer" grep -qx "       sudo agency network 192.168.1.50      (one computer)" <<<"$msg"
check "message: the next steps are last" [ "$(tail -n 1 <<<"$msg")" = "       sudo agency network                   (all private networks)" ]
check "message: WebUI address" grep -q "^WebUI on this machine:  http://127.0.0.1:18790/" <<<"$msg"
check "service ready" wait_ready
pid=$(svc_pid)
check "service runs as agency" [ "$(svc_user)" = agency ]
sleep 30
check "service still active after 30 s, same PID" [ -n "$pid" ] && [ "$(svc_pid)" = "$pid" ] && kill -0 "$pid" 2>/dev/null
check "WebUI answers" curl -sf -o /dev/null http://127.0.0.1:18790/
check "no admin account yet" [ ! -e /opt/agency/credentials.json ]
check "login refused without an account" [ "$(login_code)" != 204 ]
check "all agency-owned with the service running" all_owned

echo "=== sudo agency admin, then sign in"
run_cmd "admin" script -qec "agency admin alice" /dev/null <<<"$PASSWORD"$'\n'"$PASSWORD"
check "credentials.json agency:agency 0600" [ "$(stat -c '%U:%G %a' /opt/agency/credentials.json 2>/dev/null)" = "agency:agency 600" ]
check "login works with no other step" login_works
check "same service process" [ "$(svc_pid)" = "$pid" ]

echo "=== root-run commands keep everything agency's"
run_cmd "status" agency status
run_cmd "network private" agency network private
run_cmd "network none" agency network none
run_cmd "backup" agency backup
run_cmd "token issue" agency token issue agency
run_cmd "token list" agency token list
run_cmd "token revoke" agency token revoke agency
run_cmd "cron add" agency cron add -n probe -m hello -e 3600
run_cmd "cron list" agency cron list
run_cmd "sessions migrate" agency sessions migrate
run_cmd "skills list" agency skills list
run_cmd "test models" agency test models
run_cmd "agent" agency agent -m hello
run_cmd "model" agency model
run_cmd "memory purge (dry run)" agency memory purge
run_cmd "tls" agency tls
run_cmd "tls regenerate" agency tls --regenerate
run_cmd "devices list" agency devices list
o=$(as_root agency install --yes 2>&1); check "install refused, points to apt" grep -q "sudo apt remove agency" <<<"$o"
o=$(as_root agency uninstall --yes 2>&1); check "uninstall refused, points to apt" grep -q "sudo apt remove agency" <<<"$o"
o=$(as_root agency upgrade --yes 2>&1); check "upgrade points to apt" grep -q "apt install --only-upgrade agency" <<<"$o"
check "unit unchanged by refused commands" [ "$(unit_line)" = "$MARKER" ]
check "modes match the defaults" modes_ok
check "all agency-owned" all_owned
bk=$(ls /opt/agency/backup/agency-backup-*.tar.gz 2>/dev/null | head -1)
check "a backup made by root belongs to agency" [ -n "$bk" ] && [ "$(stat -c %U "$bk")" = agency ]

echo "=== paths the agency account cannot reach, restore"
o=$(as_root agency backup --dest /root/agency-backups 2>&1); rc=$?
echo "$o" | sed 's/^/    | /'
check "backup to a root-only folder: says why" [ "$o" = "The agency account cannot write to /root/agency-backups." ]
check "backup to a root-only folder: fails" [ "$rc" != 0 ]
check "all agency-owned" all_owned

systemctl stop agency
cp "$bk" /home/alice/backup.tar.gz && chown alice:alice /home/alice/backup.tar.gz
o=$(as_root agency restore /home/alice/backup.tar.gz --yes 2>&1); rc=$?
echo "$o" | sed 's/^/    | /'
check "restore from a home folder: says why" [ "$o" = "The agency account cannot read /home/alice/backup.tar.gz." ]
check "restore from a home folder: fails" [ "$rc" != 0 ]
run_cmd "restore" agency restore "$bk" --yes
systemctl start agency
check "service ready after restore" wait_ready
check "agency can delete a backup root made" setpriv --reuid=agency --regid=agency --init-groups rm "$bk"
check "login after restore" login_works
check "no permission errors in the service log" bash -c '! grep -qi "permission denied" /opt/agency/logs/agency.log'

echo "=== postinst twice, reinstall"
check "postinst configure twice" bash -c '/var/lib/dpkg/info/agency.postinst configure 0.0.1 >/dev/null && /var/lib/dpkg/info/agency.postinst configure 0.0.1 >/dev/null'
check "service ready" wait_ready
check "reinstall" bash -c 'apt-get install -y -qq --reinstall /tmp/agency_0.0.1.deb >/dev/null 2>&1'
check "credentials kept" [ -e /opt/agency/credentials.json ]

echo "=== upgrade restarts the service and keeps data"
before=$(svc_pid)
out=$(apt_out apt-get install -y -qq /tmp/agency_0.0.2.deb); msg=$(echo "$out" | script_out)
echo "$msg" | sed 's/^/  | /'
check "restarted" [ -n "$(svc_pid)" ] && [ "$(svc_pid)" != "$before" ]
check "ready" wait_ready
check "upgrade prints one line" [ "$msg" = "Agency is installed and running." ]
check "credentials kept" [ -e /opt/agency/credentials.json ]
check "unit kept" [ "$(unit_line)" = "$MARKER" ]
check "all agency-owned" all_owned

echo "=== a failed configure can be retried"
touch /run/fail-restart
check "configure fails while the restart fails" bash -c '! apt-get install -y -qq --reinstall /tmp/agency_0.0.2.deb >/dev/null 2>&1'
rm -f /run/fail-restart
check "dpkg --configure -a succeeds" bash -c 'dpkg --configure -a >/dev/null 2>&1'
check "ready" wait_ready

echo "=== remove keeps data and account"
check "remove" bash -c 'apt-get remove -y -qq agency >/dev/null 2>&1'
check "unit removed" [ ! -e "$UNIT" ]
check "service stopped" bash -c '! systemctl is-active'
check "data kept" [ -e /opt/agency/credentials.json ]
check "account kept" getent passwd agency >/dev/null
out=$(apt_out apt-get install -y -qq /tmp/agency_0.0.2.deb); msg=$(echo "$out" | script_out)
check "install again after remove" wait_ready
check "no admin hint while the account exists" bash -c "! grep -q 'sudo agency admin' <<<\"\$1\"" _ "$msg"

echo "=== purge deletes what the package created"
check "purge" bash -c 'apt-get purge -y -qq agency >/dev/null 2>&1'
check "/opt/agency deleted" [ ! -e /opt/agency ]
check "/home/agency deleted" [ ! -e /home/agency ]
check "account deleted" bash -c '! getent passwd agency'
check "group deleted" bash -c '! getent group agency'
check "/var/lib/agency deleted" [ ! -e /var/lib/agency ]

echo "=== purge keeps an account and /opt/agency that were there before"
adduser --system --group --home /opt/agency --no-create-home agency >/dev/null 2>&1
install -d -m 0700 -o agency -g agency /opt/agency
check "install on a pre-existing account" bash -c 'apt-get install -y -qq /tmp/agency_0.0.2.deb >/dev/null 2>&1'
check "service ready" wait_ready
check "no markers" [ ! -e /var/lib/agency/created-account ] && [ ! -e /var/lib/agency/created-home ] && [ ! -e /var/lib/agency/created-account-home ]
check "an existing account keeps its home" [ "$(getent passwd agency | cut -d: -f6)" = /opt/agency ]
check "unit PATH follows the existing home" grep -q '^Environment=PATH=/opt/agency/.local/bin:/opt/agency/bin:' "$UNIT"
apt-get purge -y -qq agency >/dev/null 2>&1
check "/opt/agency kept" [ -d /opt/agency ]
check "account kept" getent passwd agency >/dev/null
deluser --system agency >/dev/null 2>&1; rm -rf /opt/agency

echo "=== an existing manual install is left unchanged"
cat >"$UNIT" <<'EOF'
[Service]
User=alice
ExecStart=/usr/local/bin/agency
Environment=AGENCY_HOME=/home/alice/.agency
EOF
sum=$(sha256sum "$UNIT")
out=$(apt_out apt-get install -y -qq /tmp/agency_0.0.2.deb); msg=$(echo "$out" | script_out)
check "one line" [ "$msg" = "An existing Agency install was found and left unchanged ($UNIT)." ]
check "unit unchanged" [ "$(sha256sum "$UNIT")" = "$sum" ]
check "no account created" bash -c '! getent passwd agency'
check "no /opt/agency" [ ! -e /opt/agency ]
apt-get remove -y -qq agency >/dev/null 2>&1
check "remove leaves a unit on its own binary" [ "$(sha256sum "$UNIT")" = "$sum" ]
apt-get purge -y -qq agency >/dev/null 2>&1
sed -i 's|^ExecStart=.*|ExecStart=/usr/bin/agency|' "$UNIT"
out=$(apt_out apt-get install -y -qq /tmp/agency_0.0.2.deb); msg=$(echo "$out" | script_out)
check "a manual unit on /usr/bin/agency is left on install" grep -q "left unchanged" <<<"$msg"
apt-get remove -y -qq agency >/dev/null 2>&1
check "remove takes away a unit on the package's binary" [ ! -e "$UNIT" ]
apt-get purge -y -qq agency >/dev/null 2>&1

echo "=== /opt/agency of another account and no unit"
install -d -m 0700 -o alice -g alice /opt/agency
out=$(apt_out apt-get install -y -qq /tmp/agency_0.0.2.deb); msg=$(echo "$out" | script_out)
check "one line" [ "$msg" = "An existing Agency install was found and left unchanged (/opt/agency)." ]
check "still alice's" [ "$(stat -c %U /opt/agency)" = alice ]
check "no account created" bash -c '! getent passwd agency'
apt-get purge -y -qq agency >/dev/null 2>&1
check "purge keeps alice's /opt/agency" [ -d /opt/agency ]

finish
