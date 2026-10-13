# Notified

A self-hosted notification hub for a media server stack, inspired by Notifiarr. Your apps send
webhooks to Notified, Notified watches the things that can't send webhooks, and everything is
delivered to Discord as rich embeds. One small container, one SQLite file, no external services.

## What it does

| Group | Integration | How it works | Events |
|---|---|---|---|
| \*arr Apps | Sonarr, Radarr, Lidarr, Readarr, Prowlarr | Webhook (+ optional queue polling) | Grab, import, upgrade, rename, added, deleted, health, health restored, app update, manual interaction, stuck queue items |
| Media servers | Plex | Webhook (Plex Pass) | Play, pause, resume, stop, watched, new in library, new device, database backup/corruption |
| Media servers | Jellyfin | Webhook (Webhook plugin) | Play, stop, new in library, failed sign-ins, user lockouts, failed tasks, plugin changes, restart required |
| Media servers | Emby | Webhook (Emby Premiere) | Play, pause, resume, stop, new in library, failed sign-ins, update available, restart required, plugin changes |
| Media monitoring | Tautulli | Webhook | Play, stop, pause, resume, watched, errors, buffering, recently added, server up/down, updates |
| Requests | Seerr, Overseerr, Jellyseerr | Webhook | Pending, approved, auto-requested, available, declined, failed, issues |
| Downloaders | qBittorrent | Polling | Stalled downloads, errored torrents |
| Downloaders | SABnzbd | Polling | Failed downloads, queue paused/resumed |
| Downloaders | NZBGet | Polling | Failed downloads (with reason), downloads paused/resumed |
| Indexers | NZBHydra 2 | Polling | Indexer disabled after errors, indexer re-enabled |
| System | System monitor | Polling | CPU, memory and disk thresholds, periodic snapshot |
| System | Docker containers | Polling (Docker socket) | Container stopped, started, unhealthy, healthy |
| System | Portainer | Polling (Portainer API) | Container update available; one-click update from the web UI; update succeeded or failed |
| System | Docker updates | Polling (Docker API, e.g. a socket proxy) | Same as Portainer, for Docker hosts without it such as OpenMediaVault Compose |
| System | Service check | Polling | HTTP or TCP target down / recovered |

Every event can be switched on or off and routed to its own Discord channel per integration.
The History page keeps what was received and sent, shows the raw payload, and can resend.

## Run it

Copy [docker-compose.yml](docker-compose.yml) to your Docker host, then:

```bash
docker compose up -d
```

This pulls `ghcr.io/tet0r/notified:latest` (amd64 and arm64), which CI publishes from `main`.
To build from source instead,
clone the repo, switch the compose file to `build: .` and run `docker compose up -d --build`.

Open `http://<host>:8080` and create the admin account. Then:

1. **Channels** → add a Discord webhook URL (Discord: channel settings → Integrations → Webhooks → New Webhook).
2. **Integrations** → add an app. Each integration gets its own webhook URL and setup steps to paste into the app.
3. **Settings** → set **Base URL** to the address your apps use to reach Notified (for example
   `http://notified:8080` on a shared Docker network) so the displayed webhook URLs are correct.

### Pictures in notifications

Every notification carries a picture. Discord can only show images it can download itself, so:

- **Media artwork** (posters) is used when the app supplies a public image URL. For Sonarr, Radarr,
  Lidarr and Readarr, fill in the integration's URL and API key and Notified will look the poster up
  when the webhook does not include one.
- **Plex, Jellyfin and Emby** artwork is uploaded to Discord by Notified, so it works even when the
  media server is only reachable on your network.
- **Containers** get the icon matching their image or name, from the public
  [dashboard-icons](https://github.com/homarr-labs/dashboard-icons) collection.
- Anything else falls back to the app's own icon. Icons are loaded by Discord from the jsDelivr CDN.

### Versions and updates

Releases are tagged `vMAJOR.MINOR.PATCH`. Each release publishes `ghcr.io/tet0r/notified:X.Y.Z`,
`:X.Y` and `:latest`; every push to `main` publishes `:edge`. Pin a tag in the compose file if you
want to control when you upgrade.

Notified checks GitHub for a newer release every 6 hours and shows a notice in the sidebar and under
**Settings → Updates**. Update notices stay in the app; they are never sent to Discord. The check
only reads the public releases API and can be switched off there. Update with:

```bash
docker compose pull && docker compose up -d
```

### Container updates (Portainer)

Add a **Portainer** integration with your Portainer URL and an administrator access token. Notified
then compares each running container's image with the registry and lists the ones that are behind
under **Container updates**, where **Update** asks Portainer to pull the image and recreate the
container with the same settings.

- It detects a newer build of the *same tag* (for example `latest` being re-pushed). It does not
  tell you that `4.1` exists when you run `4.0`.
- Never offered: Notified itself, containers in stacks created outside Portainer (shown as
  *Limited* control in Portainer), swarm services, images pinned to a digest, locally built images.
- The Discord notice links to the page; the update itself always needs a click in the web UI.
- If an update fails partway, check the container in Portainer: it can be left stopped or renamed
  with an `-old` suffix.

### Container updates without Portainer (OpenMediaVault and others)

The **Docker updates** integration does the same job by talking to a Docker host directly, and
performs the pull-and-recreate itself. On that host, run a socket proxy:

```yaml
services:
  dockerproxy:
    image: tecnativa/docker-socket-proxy:latest
    container_name: dockerproxy
    restart: unless-stopped
    environment:
      - CONTAINERS=1
      - IMAGES=1
      - NETWORKS=1
      - DISTRIBUTION=1
      - POST=1
    ports:
      - "2375:2375"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
```

Then set the integration's address to `http://that-host:2375`.

- **The proxy has no login.** Anything that can reach port 2375 can control that host's
  containers. Keep it on your LAN and firewall the port to Notified's address if you can.
- Updating keeps the old container (renamed `name-old`) until the new one has started, and puts it
  back if creating or starting the new one fails.
- Only public images can be checked and pulled. Data in anonymous volumes is not carried over to the
  new container; named volumes and bind mounts are.
- Never offered: Notified itself, the socket proxy, swarm services, digest-pinned and local images.

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
| `NOTIFIED_UPDATE_URL` | this repo | GitHub "latest release" API URL to check for updates (for forks) |
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
go build -ldflags "-X main.version=$(git describe --tags --always | sed 's/^v//')" -o notified .
NOTIFIED_DATA_DIR=./data ./notified
```

To cut a release, push a tag: `git tag v1.2.3 && git push origin v1.2.3`. CI builds the images
and creates the GitHub release with generated notes.

The System monitor and Docker integrations only work on Linux.

## Not included (yet)

Compared with Notifiarr this leaves out the Discord bot (slash commands, reactions), TRaSH guide sync,
media requests from Discord, and multi-user accounts.
