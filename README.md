# Notified

A self-hosted notification hub for a media server stack, inspired by Notifiarr. Your apps send
webhooks to Notified, Notified watches the things that can't send webhooks, and everything is
delivered to Discord as rich embeds. One small container, one SQLite file, no external services.

## What it does

| Group | Integration | How it works | Events |
|---|---|---|---|
| Starr apps | Sonarr, Radarr, Lidarr, Readarr, Prowlarr | Webhook (+ optional queue polling) | Grab, import, upgrade, rename, added, deleted, health, health restored, app update, manual interaction, stuck queue items |
| Media servers | Plex | Webhook (Plex Pass) | Play, pause, resume, stop, watched, new in library, new device, database backup/corruption |
| Media servers | Tautulli | Webhook | Play, stop, pause, resume, watched, errors, buffering, recently added, server up/down, updates |
| Requests | Overseerr, Jellyseerr | Webhook | Pending, approved, available, declined, failed, issues |
| Downloaders | qBittorrent | Polling | Stalled downloads, errored torrents |
| Downloaders | SABnzbd | Polling | Failed downloads, queue paused/resumed |
| System | System monitor | Polling | CPU, memory and disk thresholds, periodic snapshot |
| System | Docker containers | Polling (Docker socket) | Container stopped, started, unhealthy, healthy |
| System | Service check | Polling | HTTP or TCP target down / recovered |

Every event can be switched on or off and routed to its own Discord channel per integration.
The History page keeps what was received and sent, shows the raw payload, and can resend.

## Run it

```bash
git clone https://github.com/tet0r/notified.git
cd notified
docker compose up -d --build
```

Open `http://<host>:8080` and create the admin account. Then:

1. **Channels** → add a Discord webhook URL (Discord: channel settings → Integrations → Webhooks → New Webhook).
2. **Integrations** → add an app. Each integration gets its own webhook URL and setup steps to paste into the app.
3. **Settings** → set **Base URL** to the address your apps use to reach Notified (for example
   `http://notified:8080` on a shared Docker network) so the displayed webhook URLs are correct.

### Compose notes

- `./data` holds the database. Back that folder up and you have backed up everything.
- `/var/run/docker.sock` is only needed for the Docker containers integration. It is mounted read-only,
  but socket access still exposes container details, so remove the line if you don't use it.
- `/:/host:ro` lets the System monitor read disk usage (watch `/host`, or mount specific disks and list those paths).
- Set `TZ` to your timezone so the dashboard's days line up.

### Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `NOTIFIED_PORT` | `8080` | Listen port |
| `NOTIFIED_DATA_DIR` | `/data` | Where the SQLite database lives |
| `NOTIFIED_RESET_ADMIN` | unset | Set to `1` for one start to clear the admin account and run setup again |
| `TZ` | UTC | Timezone |

## Security

- Webhook URLs contain a random per-integration token; anyone who has the URL can post notifications
  to that integration. Regenerate it from the integration's editor if it leaks.
- The web UI uses a single admin login. Put Notified behind a reverse proxy with HTTPS if it is
  reachable from outside your LAN.
- API keys and Discord webhook URLs are stored unencrypted in the SQLite file; protect the `data` folder.

## Develop

Needs Go 1.26+. No CGO or Node toolchain: the UI is plain HTML/CSS/JS embedded in the binary.

```bash
go build -o notified .
NOTIFIED_DATA_DIR=./data ./notified
```

The System monitor and Docker integrations only work on Linux.

## Not included (yet)

Compared with Notifiarr this leaves out the Discord bot (slash commands, reactions), TRaSH guide sync,
media requests from Discord, and multi-user accounts.
