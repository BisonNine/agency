# Administration

This part of the manual covers running an Agency install safely: signing in, reaching it over the network, checking its configuration, and keeping records and backups.

## Pages in this section

| Page | What it covers |
|---|---|
| [Signing in](./login.md) | The admin account, `agency admin`, sessions and login lockouts. |
| [HTTPS and certificates](./https.md) | The HTTP and HTTPS listeners, the self-signed certificate, using your own, `agency tls`. |
| [Network access](./network.md) | Localhost or network, allowed networks, trusted proxies, the device listener. |
| [Remote access](./remote-access.md) | Reaching Agency from elsewhere: your network, a reverse proxy, Cloudflare Tunnel, Tailscale. |
| [Check Up and the full report](./checkup.md) | The **Check Up** page, the PDF report, and what to do about a marked row. |
| [Audit log](./audit.md) | The record of tool calls, configuration saves and logins. |
| [Alerts](./alerts.md) | What alerts are for, how to receive them, and the list of alerts. |
| [Backup and restore](./backup.md) | The nightly backup, `agency backup` and `agency restore`. |
| [Logs](./logs.md) | Where the logs are, log levels, the **Logs** page, and the startup permission checks. |
| [Billing codes](./billing.md) | What each model call is billed to, the accounting log and the pricing table. |

## Where things are in the WebUI

- **Check Up** sits on its own in the sidebar, below the groups.
- **Network**, **System**, **Logs** and **Audit** are in the **Services** group.
- **Sign out** is next to the version at the bottom of the sidebar.

## Commands on the server

Run these on the machine Agency is installed on, as the user it runs as.

| Command | What it does |
|---|---|
| `agency admin` | Creates or replaces the admin account. See [Signing in](./login.md). |
| `agency status` | Shows whether Agency is running, the addresses to open, the certificate and whether an admin account exists. |
| `agency network` | Shows or sets network access. See [Network access](./network.md). |
| `agency tls` | Shows the HTTPS certificate. See [HTTPS and certificates](./https.md). |
| `agency backup`, `agency restore` | Take or restore a backup. See [Backup and restore](./backup.md). |

On a system install, run them as the service user with the data directory set, for example `sudo -u <service user> AGENCY_HOME=/opt/agency agency status`.
