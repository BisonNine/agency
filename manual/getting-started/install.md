# Installing Agency

This page installs Agency as a background service and creates the admin account you sign in with.

There are four ways to install. The Debian package sets up and starts the service itself; the others finish with `agency install`, which registers the service, creates the admin account and starts Agency.

## Before you start

- A Linux or macOS machine on amd64 or arm64 (Linux 386 is also available).
- A terminal on that machine.
- Decide who runs the service:
  - **As your user** (no `sudo`): Agency starts when you log in. The data directory is `~/.agency`.
  - **As a system service** (with `sudo`): Agency starts at boot without anyone logging in. It still runs as your user, never as root.

## Option 1: the one-line installer

This downloads the latest release, verifies its checksum, and runs `agency install`.

```bash
curl -fsSL https://raw.githubusercontent.com/BisonNine/agency/main/agency-online-install.sh | bash
```

For a system service, end the line with `sudo bash` instead of `bash`.

When it finishes, it opens the WebUI in your browser if it can, or prints the addresses to open.

To install a specific release, set `AGENCY_VERSION` (for example `AGENCY_VERSION=0.6.0`) before the command.

## Option 2: a release archive

1. Download the archive for your platform from the [GitHub releases](https://github.com/BisonNine/agency/releases). The names are `agency-<os>-<arch>.tar.gz`, for example `agency-linux-amd64.tar.gz` or `agency-darwin-arm64.tar.gz`.
2. Unpack it and run the installer:

   ```bash
   tar -xzf agency-linux-amd64.tar.gz
   ./agency install
   ```

   For a system service, run `sudo ./agency install`.

## Option 3: Debian or Ubuntu package (`apt`)

On Debian and Ubuntu (`amd64` and `arm64`), install the `agency` package from the APT repository (or from a downloaded `agency_<version>_<arch>.deb` file with `sudo apt install ./agency_<version>_<arch>.deb`), then create the admin account:

```bash
sudo apt update
sudo apt install agency
sudo agency admin
```

The package sets up a system service and starts it, so there is no `agency install` step. Until `sudo agency admin` has run, nobody can sign in.

| | |
|---|---|
| Programs | `/usr/bin/agency`, `/usr/bin/agency-auth`, `/usr/bin/openclaw` |
| Service account | `agency`, a system account with no password |
| Data directory | `/opt/agency`, owned by `agency` |
| Service | `/etc/systemd/system/agency.service`, running as `agency` |

Commands you run with `sudo` (`sudo agency status`, `sudo agency network`, `sudo agency backup`, …) act as the `agency` account, so what they write in `/opt/agency` stays its own.

To sign a CLI provider in for the service (see [CLI providers](../providers/cli-providers.md)), open a shell as the service account with `sudo -u agency -i` and sign in there. CLIs installed in `/opt/agency/.local/bin` or `/opt/agency/bin` are on the service's `PATH`.

If the machine already has an Agency system service, or an `/opt/agency` that belongs to another account, the package leaves it unchanged and says so. The package does not see a user service (`~/.config/systemd/user/agency.service`), so stop that one first.

To remove the package:

- `sudo apt remove agency` stops and removes the service, and keeps `/opt/agency` and the `agency` account.
- `sudo apt purge agency` also deletes `/opt/agency` and the `agency` account and group, each only if the package created it.

## Option 4: build from source

You need Git, Go, Node.js and pnpm.

```bash
git clone https://github.com/BisonNine/agency.git
cd agency
make build
./build/agency install
```

## The admin account

The WebUI requires a login, and there is no default account. When none exists, `agency install` asks for one before it starts the service:

- a username (no spaces, at most 64 characters);
- a password, typed twice, at least 12 characters. It is not shown as you type.

This works under `curl … | bash` too: the installer reads from your terminal, not from the pipe.

An existing account is kept. To replace it later, run `agency admin` on the server. See [Signing in](../admin/login.md).

### Unattended install

Without a terminal, export the account in the same shell, run the installer, then remove the variables:

```bash
read -r AGENCY_ADMIN_USER; read -rs AGENCY_ADMIN_PASSWORD
export AGENCY_ADMIN_USER AGENCY_ADMIN_PASSWORD
./agency install
unset AGENCY_ADMIN_USER AGENCY_ADMIN_PASSWORD
```

With `sudo`, keep them across it: `sudo --preserve-env=AGENCY_ADMIN_USER,AGENCY_ADMIN_PASSWORD ./agency install`.

::: warning
Do not write the password on the command line in front of the installer. It would be saved in your shell history.
:::

If there is no account, no terminal and no variables, the installer stops without starting the service and says how to continue.

## What the installer does

| | User service | System service (`sudo`) |
|---|---|---|
| Binary | `/usr/bin` when installed by `apt`, else `~/bin` if it exists, else `~/.local/bin` | `/usr/bin` when installed by `apt`, `/opt/agency/bin` when the data directory is `/opt/agency`, else `/usr/local/bin` |
| Linux service | `~/.config/systemd/user/agency.service` | `/etc/systemd/system/agency.service` |
| macOS service | `~/Library/LaunchAgents/com.bison9.agency.plist` | `/Library/LaunchDaemons/com.bison9.agency.plist` |

It also:

- creates the data directory, `~/.agency` by default (`AGENCY_HOME` overrides it; an existing `/opt/agency` is used if present);
- writes a complete `config.json` there on first start;
- creates an `openclaw` link next to `agency`, which the Rabbit R1 needs (see [Devices](../channels/devices.md));
- prints the addresses where the WebUI can be opened.

`--bin-dir` installs the binary somewhere else. `--user` picks the account the system service runs as (default: the user who ran `sudo`). `-y` skips the confirmation question but never the admin account.

Re-run `agency install` with the same privilege as the first time: `sudo` for a system service, none for a user service. The installer refuses the other one.

## Opening the WebUI

By default Agency listens on:

- `http://127.0.0.1:18790/` — plain HTTP, this machine only;
- `https://<this machine>:18443/` — HTTPS on every interface, with a self-signed certificate. Your browser warns about it once.

Other machines are refused until you allow their network, even over HTTPS (see [HTTPS and certificates](../admin/https.md)). `agency status` prints the current addresses at any time.

## Installing on a headless or remote host

To reach the WebUI from another machine, allow your network when you install:

```bash
./agency install --allowed-cidrs 192.168.1.0/24
```

`--allowed-cidrs` takes a comma-separated list of networks, or `private` (the private LAN ranges) or `any` (any address). Prefer the narrowest range that works. Then open the HTTPS address from the other machine.

`--host 0.0.0.0` also serves plain, unencrypted HTTP on the network. It is not recommended, and it requires `--allowed-cidrs`.

The same options work with the one-line installer:

```bash
curl -fsSL https://raw.githubusercontent.com/BisonNine/agency/main/agency-online-install.sh | bash -s -- --allowed-cidrs 192.168.1.0/24
```

You can change network access later on the **Network** page or with `agency network`. See [Network access](../admin/network.md).

## Running without a service

You can also run `agency` in a terminal. It starts Agency in the foreground until you press Ctrl+C. Create the admin account first with `agency admin`.

## Removing the service

`agency uninstall` stops and removes the service. The binary and the data directory are left in place. Use `sudo` if the service was installed with `sudo`.

## Next step

[Quick start](./quickstart.md): sign in and set up your first agent.
