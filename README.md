# SDDB — Container Dashboard

A self-hosted Docker container monitoring dashboard with a lightweight agent/dashboard architecture. Monitor containers across multiple hosts from a single web UI, with live stats, log viewing, one-click upgrades, AI-powered log analysis, and automatic update management.

---

## Overview

SDDB has two components:

- **`sddb-agent`** — runs on each Docker host. Polls the local Docker daemon, checks for image updates, and exposes an HTTP API.
- **`sddb-dashboard`** — runs anywhere on your network (or in a container). Polls all registered agents, presents the unified web UI, and handles upgrades.

Communication between the dashboard and agents is secured with mutual TLS (mTLS) — both sides present certificates signed by the same CA.

---

## Features

- **Multi-host sidebar** — filter the dashboard to a single host or view all at once
- **Host summary bar** — per-host CPU, RAM, disk, and aggregate network I/O shown above container cards
- **Live container stats** — CPU, memory, network I/O, ports, compose project
- **Restart / exit history** — recent crashes, OOM kills, and exit codes surfaced directly on the container card (last 48 hours only)
- **Update detection** — checks Docker Hub, GCR, GHCR, and any OCI-compatible registry
- **One-click upgrade** — pulls the latest image and recreates the container (compose-aware)
- **Auto-update** — per-container toggle; the dashboard upgrades automatically when a new image is available, with a 10-minute cooldown between attempts
- **Log viewer** — view the last N lines of any container's logs, with search/filter, clipboard copy, and live auto-refresh
- **AI analysis** — send logs or a health prompt to Claude, ChatGPT, or a local Ollama model for instant analysis
- **Notifications** — ntfy push alerts on crash, OOM kill, or restart loop; master on/off toggle plus per-host mute; automatic crash-loop suppression and recovery notification
- **Settings** — choose AI provider; configure ntfy URL; toggle visibility of stopped containers
- **Admin authentication** — optional login; unprotected by default until you run `set-admin`
- **mTLS security** — all agent communication is certificate-authenticated
- **Systemd service files** — included for both components

---

## Requirements

- Docker running on each monitored host
- `docker compose` plugin (or legacy `docker-compose`) on agent hosts if you use Compose

---

## Releases

Pre-built binaries for `linux/amd64`, `linux/arm64`, `darwin/amd64`, and `darwin/arm64` are available on the [Releases](https://github.com/smokinstack/sddb/releases) page.

---

## Building from source

Requires Go 1.22+.

```bash
git clone https://github.com/smokinstack/sddb
cd sddb

# Build both binaries into bin/
make build

# Cross-compile for Linux amd64 (e.g. building on a Mac)
make linux
```

Binaries are written to `bin/sddb-agent` and `bin/sddb-dashboard`.

---

## Docker (recommended)

The easiest way to run the dashboard is with Docker Compose. Caddy is included and handles HTTPS automatically.

> **Already running a reverse proxy?** If you have Nginx Proxy Manager, Traefik, or another proxy already occupying port 443 on the host, skip Caddy and use that instead — see [Using an existing reverse proxy](#using-an-existing-reverse-proxy).

### Prerequisites

- Docker with the Compose plugin (`docker compose version` to verify)
- A hostname for the dashboard — either a public domain or a local one (see below)

### Choosing a hostname

Caddy requires a hostname (not a bare IP address) to generate a valid TLS certificate. Pick one of:

| Option | Example | How to access |
|---|---|---|
| **Public domain** | `sddb.example.com` | DNS A record → your server IP. Caddy gets a Let's Encrypt cert automatically. |
| **LAN hostname** | `sddb.home` | Add to your router/Pi-hole DNS, or to the hosts file on each device. Caddy uses its internal CA (`tls internal`). |

For a LAN hostname on Windows add to `C:\Windows\System32\drivers\etc\hosts`:
```
192.168.0.x   sddb.home
```

On Linux/Mac add to `/etc/hosts`:
```
192.168.0.x   sddb.home
```

For network-wide resolution without per-device changes, add the entry to your router's DNS or Pi-hole (`192.168.0.x sddb.home`).

### Setup

```bash
# 1. Clone the repo
git clone https://github.com/smokinstack/sddb
cd sddb

# 2. Create your .env file
cp .env.example .env
# Edit .env — set SDDB_DOMAIN to your chosen hostname (e.g. sddb.home or sddb.example.com)

# 3. Update the Caddyfile for your setup (see below)

# 4. Start everything
docker compose up -d
```

**Caddyfile for a public domain** (Let's Encrypt cert, automatic):
```
{$SDDB_DOMAIN} {
    reverse_proxy dashboard:8080
}
```

**Caddyfile for a LAN hostname** (internal self-signed cert):
```
sddb.home {
    tls internal
    reverse_proxy dashboard:8080
}
```

Replace `sddb.home` with whatever hostname you chose. On first start with `tls internal`, Caddy generates a local CA and issues a certificate — you'll see a browser warning until you import the CA cert (see [Trusting the local CA](#trusting-the-local-ca)).

The dashboard will be available at `https://<your-hostname>`.

### Set an admin password

```bash
docker compose run --rm dashboard set-admin -data-dir /data
```

### Enroll agents

```bash
docker compose run --rm dashboard enroll <hostname> -data-dir /data -out /data/certs
```

This creates the CA (if it doesn't exist yet) and writes three cert files into the `sddb-data` volume under `/data/certs/`. **Restart the dashboard after the first enroll** so it picks up the new CA and activates mTLS:

```bash
docker compose restart dashboard
```

The dashboard log will confirm: `mTLS enabled — agents must present a certificate signed by the dashboard CA`.

Find the cert files on the host and copy them to the agent:

```bash
VOL=$(docker volume inspect sddb_sddb-data --format '{{ .Mountpoint }}')

# Agent on a remote host
scp $VOL/certs/<hostname>-agent.crt user@<agent-host>:/tmp/agent.crt
scp $VOL/certs/<hostname>-agent.key user@<agent-host>:/tmp/agent.key
scp $VOL/certs/<hostname>-ca.crt    user@<agent-host>:/tmp/ca.crt
ssh user@<agent-host> "sudo mv /tmp/agent.crt /tmp/agent.key /tmp/ca.crt /etc/sddb/"

# Agent on the same host as the dashboard
sudo cp $VOL/certs/<hostname>-agent.crt /etc/sddb/agent.crt
sudo cp $VOL/certs/<hostname>-agent.key /etc/sddb/agent.key
sudo cp $VOL/certs/<hostname>-ca.crt    /etc/sddb/ca.crt
```

The service unit expects the files named `agent.crt`, `agent.key`, and `ca.crt` — rename them on copy as shown above.

### Data persistence

Dashboard data (PKI, config, agent list) is stored in the `sddb-data` named volume. Caddy certificates are in `caddy-data`. Both survive `docker compose down` — only `docker compose down -v` removes them.

### Upgrading

```bash
docker compose pull   # if using a pre-built image from a registry
docker compose build  # if building from source
docker compose up -d
```

---

## Installation (binary)

### Dashboard host

The dashboard can run on any machine that has network access to your agent hosts. It does not need Docker installed.

**Download and install the binary:**

```bash
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
VERSION=$(curl -fsSL https://api.github.com/repos/smokinstack/sddb/releases/latest | grep '"tag_name"' | cut -d'"' -f4)
curl -fsSL "https://github.com/smokinstack/sddb/releases/download/${VERSION}/sddb-dashboard_${VERSION}_linux_${ARCH}.tar.gz" \
  | tar xz -C /tmp
sudo install -m755 /tmp/sddb-dashboard /usr/local/bin/
```

**Install the systemd unit:**

```bash
curl -fsSL https://raw.githubusercontent.com/smokinstack/sddb/main/deploy/sddb-dashboard.service \
  | sudo tee /etc/systemd/system/sddb-dashboard.service
sudo systemctl daemon-reload
```

Data (PKI, config, agent list) is stored in `/var/lib/sddb`.

**Set an admin password** (optional but recommended):

```bash
sudo sddb-dashboard set-admin -data-dir /var/lib/sddb
```

You will be prompted for a username and password. If you skip this step the dashboard is accessible without login — add it any time and restart the service to activate it.

**Start the service:**

```bash
sudo systemctl enable --now sddb-dashboard
```

The dashboard listens on port `8080` by default. Open `http://<host>:8080` in your browser (or put Caddy in front — see the [Reverse Proxy](#reverse-proxy-caddy) section).

---

### Agent hosts

Each machine you want to monitor needs the agent binary and a certificate issued by the dashboard CA.

#### Step 1 — Issue a certificate (run on the dashboard host)

```bash
sudo sddb-dashboard enroll <hostname> -data-dir /var/lib/sddb -out /tmp/certs
```

Replace `<hostname>` with a label for the machine (e.g. `plexmon`, `shadow`). Three files are created:

| File | Purpose |
|---|---|
| `<hostname>-agent.crt` | Agent TLS certificate |
| `<hostname>-agent.key` | Agent TLS private key |
| `<hostname>-ca.crt` | Dashboard CA (agent uses this to verify the dashboard) |

You only need to do this once per agent. Existing certificates are reused — re-running `enroll` with the same name is safe.

#### Step 2 — Copy files to the agent host

On the agent host, create the directory and a dedicated user:

```bash
sudo useradd -r -s /sbin/nologin sddb
sudo usermod -aG docker sddb
sudo mkdir -p /etc/sddb
sudo chown sddb:sddb /etc/sddb
sudo chmod 750 /etc/sddb
```

Copy the cert files from the dashboard host, renaming them to the standard names the service unit expects:

```bash
scp /tmp/certs/<hostname>-agent.crt user@<agent-host>:/tmp/agent.crt
scp /tmp/certs/<hostname>-agent.key user@<agent-host>:/tmp/agent.key
scp /tmp/certs/<hostname>-ca.crt    user@<agent-host>:/tmp/ca.crt

ssh user@<agent-host> "sudo mv /tmp/agent.crt /tmp/agent.key /tmp/ca.crt /etc/sddb/ \
  && sudo chown sddb:sddb /etc/sddb/* \
  && sudo chmod 640 /etc/sddb/agent.crt /etc/sddb/ca.crt \
  && sudo chmod 600 /etc/sddb/agent.key"
```

#### Step 3 — Install the agent binary

On the agent host:

```bash
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
VERSION=$(curl -fsSL https://api.github.com/repos/smokinstack/sddb/releases/latest | grep '"tag_name"' | cut -d'"' -f4)
curl -fsSL "https://github.com/smokinstack/sddb/releases/download/${VERSION}/sddb-agent_${VERSION}_linux_${ARCH}.tar.gz" \
  | tar xz -C /tmp
sudo install -m755 /tmp/sddb-agent /usr/local/bin/
```

#### Step 4 — Install and start the systemd unit

```bash
curl -fsSL https://raw.githubusercontent.com/smokinstack/sddb/main/deploy/sddb-agent.service \
  | sudo tee /etc/systemd/system/sddb-agent.service
sudo systemctl daemon-reload
sudo systemctl enable --now sddb-agent
```

No override needed — the service unit already points to `/etc/sddb/agent.crt`, `/etc/sddb/agent.key`, and `/etc/sddb/ca.crt`.

#### Step 5 — Add the agent to the dashboard

Open the dashboard, click **+ Add Agent**, enter the agent's IP and port (e.g. `192.168.1.10:8484`), and optionally give it a label. It will appear as online within one poll cycle.

---

## Upgrading binaries

Re-run the install one-liners from above — they always pull the latest release. Then restart the service:

**Dashboard:**
```bash
sudo systemctl restart sddb-dashboard
```

**Each agent host:**
```bash
sudo systemctl restart sddb-agent
```

---

## AI Integration

The dashboard can send container logs or health prompts to an AI model for instant analysis. Three providers are supported: **Claude** (Anthropic), **ChatGPT** (OpenAI), and **Ollama** (local/self-hosted). You can configure one, two, or all three simultaneously.

### What it does

Each expanded container card has two AI buttons:

- **Logs → Analyze** — opens the log viewer, fetches recent log output, sends it to the AI, and asks it to summarise errors, warnings, and anything unusual.
- **Health** — sends the container name, image, and current state to the AI and asks it to assess whether the container appears healthy.

The AI response is shown in a modal. No data is stored or logged — each request is a direct, one-shot API call from the dashboard server to the AI provider.

### Configuring providers

Set environment variables on the dashboard host before starting the service. The easiest way is via a systemd override:

```bash
sudo systemctl edit sddb-dashboard
```

```ini
[Service]
Environment="ANTHROPIC_API_KEY=sk-ant-..."
Environment="OPENAI_API_KEY=sk-..."
Environment="OLLAMA_BASE_URL=http://192.168.1.50:11434"
Environment="OLLAMA_MODEL=llama3.2"
```

Restart to apply:

```bash
sudo systemctl restart sddb-dashboard
```

Only set the variables for providers you actually have. The dashboard startup log confirms what is active:

```
AI assistant enabled — Claude (Anthropic)
```

| Variable | Required for | Notes |
|---|---|---|
| `ANTHROPIC_API_KEY` | Claude | Get from console.anthropic.com |
| `OPENAI_API_KEY` | ChatGPT | Get from platform.openai.com |
| `OLLAMA_BASE_URL` | Ollama | URL of your Ollama server, e.g. `http://192.168.1.50:11434` |
| `OLLAMA_MODEL` | Ollama | Model name, e.g. `llama3.2`. Defaults to `llama3.2` if not set |

### Selecting a provider

If more than one provider is configured, open **Settings** (gear icon, top right) and pick which one to use. The choice is saved to `/var/lib/sddb/config.json` and persists across restarts.

If no provider is selected in settings, the dashboard automatically uses whichever is available, in priority order: **Claude → OpenAI → Ollama**.

### Other settings

The Settings panel also contains:

| Setting | Default | Description |
|---|---|---|
| Show stopped containers | On | Hide/show stopped containers across all hosts. Saved in browser `localStorage`. |
| Enable push notifications | Off | Master on/off for all ntfy alerts. Saved to `config.json`. |
| ntfy URL | — | Full topic URL for push notifications. Saved to `config.json`. |
| Per-host bell icon | On | Mutes/unmutes alerts for a specific host. Bell gains a strikethrough when muted. Saved to `config.json`. |

### Claude (Anthropic)

Sign up at [console.anthropic.com](https://console.anthropic.com), create an API key, and set `ANTHROPIC_API_KEY`. The dashboard uses the `claude-sonnet-4-6` model with a 2048-token response limit. Claude typically gives the most detailed and actionable analysis.

### ChatGPT (OpenAI)

Sign up at [platform.openai.com](https://platform.openai.com), create an API key, and set `OPENAI_API_KEY`. The dashboard uses the `gpt-4o` model.

### Ollama (local)

Ollama lets you run open-source models entirely on your own hardware — no API keys, no external calls.

1. Install Ollama: [ollama.com](https://ollama.com)
2. Pull a model: `ollama pull llama3.2`
3. Make sure Ollama is reachable from the dashboard host (default port `11434`)
4. Set `OLLAMA_BASE_URL` to the Ollama server address and `OLLAMA_MODEL` to the model name

If Ollama is on a different machine, ensure port `11434` is open between the dashboard host and the Ollama server. The dashboard uses Ollama's OpenAI-compatible `/v1/chat/completions` endpoint.

Recommended models for log analysis (in order of quality vs. speed):
- `llama3.2` — good balance, fast on modest hardware
- `mistral` — strong reasoning, slightly larger
- `qwen2.5` — excellent for structured output

---

## Auto-Update

Each container card has an **Auto** button. When enabled, the dashboard will automatically upgrade that container whenever the registry has a newer image, without any manual intervention.

### How it works

1. The agent checks the image registry on each refresh cycle (every 5 minutes per image, cached). It fetches the remote manifest digest and compares it to the local `RepoDigests`. If they differ, the container gets an **update available** badge.
2. After each successful poll, the dashboard checks all containers. If a container is **running**, has **update available**, and **Auto is enabled**, it sends an upgrade command to the agent.
3. The agent pulls the new image and recreates the container. For compose containers it runs `docker compose pull` then `docker compose up -d`. For standalone containers it stops and removes the old one, then creates a new one with the same configuration.
4. A **10-minute cooldown** applies per container to prevent repeated upgrade attempts. If the upgrade fails, the cooldown is immediately reset so it can retry.

### Important notes

- Auto-update only applies to **running** containers. A stopped container will not be started or upgraded automatically.
- Toggles are saved to `/var/lib/sddb/config.json` and survive dashboard restarts.
- The 10-minute cooldown is in-memory — restarting the dashboard clears all cooldown timers.
- Registries that require authentication (private registries) are not supported for update checks. The container will never show **update available** and auto-update will not fire.

### Docker Hub rate limiting

Docker Hub limits unauthenticated image pulls to **100 pulls per 6 hours** per IP address. If auto-update hits this limit you will see an error like:

```
toomanyrequests: You have reached your unauthenticated pull rate limit
```

When this happens the dashboard automatically backs off for **1 hour** before retrying, rather than hammering the registry every 10 minutes.

The permanent fix is to authenticate Docker on each agent host:

```bash
docker login
```

Authenticated free accounts get 200 pulls per 6 hours. Docker Pro/Team accounts have no pull limit. Once logged in, compose pulls and standalone pulls will use your credentials automatically.

---

## Host Summary Bar

Each agent panel shows a one-line summary of host-level resource usage above its container cards:

```
CPU 12% | RAM 9.2 GiB / 31.9 GiB | Disk 61% | Net ↓1.2 MiB/s ↑0.4 MiB/s
```

| Metric | Source |
|---|---|
| CPU | `/proc/stat` delta between poll cycles |
| RAM | `/proc/meminfo` (MemTotal − MemAvailable) |
| Disk | Root filesystem (`/`) via `statfs` |
| Net | Sum of all container network rates |

The bar is only shown once the agent has reported at least one poll cycle. CPU is 0% on the first cycle (needs two samples to compute a delta) and fills in normally from the second refresh onward.

CPU turns yellow at 50% and red at 80%. Disk turns yellow at 75% and red at 90%.

---

## Restart / Exit History

If a container has crashed, been OOM-killed, or exited non-zero within the last 48 hours, the container card shows a history line:

```
↻ 3 restarts   Exit 137 (OOMKilled) · 14m ago
```

- **↻ N restarts** — number of times Docker's restart policy has restarted the container
- **Exit N** — last exit code (yellow for non-zero, red for OOM kill, grey for clean exit 0)
- **OOMKilled** — shown when the kernel killed the container due to memory pressure
- **· Xm ago** — time since the last exit

The history line is hidden when the last exit was more than 48 hours ago to avoid noise from containers with large historical restart counts.

---

## Notifications (ntfy)

SDDB can push alerts to any [ntfy](https://ntfy.sh) topic when a container crashes, is OOM-killed, or enters a restart loop.

### Setup

1. Choose a topic URL — either on the public ntfy.sh server or your own self-hosted instance:
   - Public: `https://ntfy.sh/my-homelab-alerts` (use a hard-to-guess name)
   - Self-hosted: `https://ntfy.example.com/my-topic`
2. Subscribe to the topic in the ntfy app on your phone or browser.
3. Open **Settings** in the dashboard, enable the **Enable push notifications** toggle, paste the URL into the **ntfy URL** field, and click **Save**.
4. Click **Test** to send a test notification and confirm it arrives before relying on it.

The URL and all notification settings are saved to `/var/lib/sddb/config.json` and take effect immediately — no restart needed.

### Enabling and disabling

There are two levels of control:

**Master toggle** — in Settings > Notifications. Turns all alerts on or off globally. Useful when doing maintenance and you don't want your phone buzzing. The per-host mute states are preserved and take effect again when you re-enable.

**Per-host mute** — the bell icon in each host panel header. Click it to silence alerts for all containers on that host. The bell gains a diagonal strikethrough when muted. Click again to unmute. Muting a host does not affect other hosts.

When a host or the master toggle is muted, the dashboard still tracks container state internally (crash-loop counters, stability timers) so it picks up correctly when alerts are re-enabled rather than firing a burst of stale alerts.

### What triggers an alert

| Event | Priority | Condition |
|---|---|---|
| Container restarted | default | Docker restart policy fired (RestartCount increased) |
| Container OOM-killed and restarted | high | As above, OOMKilled flag set |
| Container crashed and stopped | default | `running` → `exited` with non-zero exit code |
| Container OOM-killed and stopped | high | As above, OOMKilled flag set |
| **Crash loop detected** | urgent | 2 or more crash events within 15 minutes |
| **Container recovered** | low | Stable for 10 minutes after crash-loop suppression |
| Clean stop (exit 0) | — | No alert — assumed intentional |

### Crash-loop suppression

If a container crashes repeatedly, SDDB escalates and then goes quiet to avoid flooding your phone:

1. First crash → normal alert
2. Second crash within 15 minutes → **"Crash loop" urgent alert**, then all further alerts for that container are suppressed
3. Once the container has been running without a new restart for 10 minutes → single **"Recovered"** alert, normal alerting resumes

The 15-minute crash window is a rolling timestamp check, not a poll-cycle count, so fast restart loops are caught quickly regardless of poll interval.

---

## Log Viewer

Each container card has a **Logs** button that opens the log viewer. Features:

- **Tail selector** — choose the last 50, 100, 200, 500, or 1000 lines
- **Filter** — type to filter lines in real time (client-side, no re-fetch)
- **Live refresh** — click **Live** to auto-refresh every 3 seconds. The panel updates silently in the background (no flicker); the view only scrolls to the bottom if you were already there, so you can scroll up to read without being interrupted. Click **Live** again or close the modal to stop.
- **AI analysis** — send the current log output to your configured AI provider for instant analysis
- **Clipboard copy** — copies the raw (unfiltered) log output

---

## Dashboard reference

### Subcommands

```
sddb-dashboard                   start the dashboard
sddb-dashboard set-admin         create or update the admin login
sddb-dashboard enroll <name>     issue an mTLS certificate for an agent
```

### Dashboard flags

| Flag | Default | Description |
|---|---|---|
| `-addr` | `:8080` | Listen address |
| `-poll` | `5s` | Agent poll interval |
| `-agent-port` | `8484` | Default port for network scan and bare-IP adds |
| `-data-dir` | `~/.sddb` | Data directory (PKI, config, agent list) |

### Agent flags

| Flag | Default | Description |
|---|---|---|
| `-addr` | `:8484` | Listen address |
| `-interval` | `5s` | Docker stats refresh interval |
| `-tls-cert` | — | Path to agent TLS certificate |
| `-tls-key` | — | Path to agent TLS private key |
| `-tls-ca` | — | Path to CA cert (verifies the dashboard) |

---

## Project structure

```
cmd/
  agent/            agent entry point
  dashboard/        dashboard entry point + embedded web assets
    web/
      templates/    HTML templates
internal/
  agent/            Docker polling, update checks, upgrade logic
  ai/               Claude / OpenAI / Ollama client
  auth/             Session management and admin credentials
  config/           Persistent JSON config (AI provider, auto-update toggles)
  dashboard/        HTTP handlers, poller, state management
  pki/              CA and certificate issuance
  types/            Shared types between agent and dashboard
deploy/
  sddb-agent.service
  sddb-dashboard.service
```

---

## Using an existing reverse proxy

If you already have Nginx Proxy Manager, Traefik, or any other reverse proxy running on your server, it will already own port 443. Adding Caddy to the mix causes a port conflict — only one process can bind port 443.

In this case, remove Caddy from the compose file and expose the dashboard directly on a host port:

```yaml
services:
  dashboard:
    build: .
    restart: unless-stopped
    ports:
      - "8069:8080"   # pick any free port
    volumes:
      - sddb-data:/data

volumes:
  sddb-data:
```

Then point your existing proxy at `<host-ip>:8069`.

**Nginx Proxy Manager:** Hosts → Add Proxy Host
- Domain: `sddb.example.com`
- Scheme: `http`
- Forward Hostname/IP: your server IP
- Forward Port: `8069`
- SSL tab: enable and request a Let's Encrypt certificate

**Traefik:** add the standard labels to the dashboard service in your compose file.

The dashboard works over plain HTTP on the internal port — your reverse proxy handles TLS termination. Login works correctly because the session cookie detects HTTPS via the `X-Forwarded-Proto` header that NPM and Traefik both set automatically.

---

## Reverse Proxy (Caddy)

If you want to expose the dashboard over the internet or enable HTTPS on your LAN, Caddy is the simplest option — it handles TLS certificates automatically via Let's Encrypt.

### Install Caddy

```bash
sudo apt install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | sudo tee /etc/apt/sources.list.d/caddy-stable.list
sudo apt update && sudo apt install caddy
```

### Hostname requirement

Caddy needs a hostname — not a bare IP — to generate a valid TLS certificate. Use a public domain or a local one (e.g. `sddb.home`) and make it resolve to your server via DNS or a hosts file entry. See [Choosing a hostname](#choosing-a-hostname) in the Docker section for options.

### Caddyfile — public domain

Create `/etc/caddy/Caddyfile`:

```
sddb.example.com {
    reverse_proxy localhost:8080
}
```

Caddy will automatically obtain and renew a Let's Encrypt certificate.

### Caddyfile — LAN hostname

```
sddb.home {
    tls internal
    reverse_proxy localhost:8080
}
```

Replace `sddb.home` with your chosen local hostname. Caddy generates an internal CA and issues a self-signed certificate on first start.

Apply either config:

```bash
sudo systemctl enable --now caddy   # first time
sudo systemctl reload caddy          # after changes
```

### Trusting the local CA

When using `tls internal` you'll get a browser warning until you import Caddy's root CA. Extract it:

```bash
# Binary install
sudo find /var/lib/caddy -name "root.crt" 2>/dev/null

# Docker install
docker cp sddb-caddy-1:/data/caddy/pki/authorities/local/root.crt ~/caddy-root.crt
```

Import the cert:

- **Windows:** double-click → Install Certificate → Local Machine → Trusted Root Certification Authorities
- **Mac:** double-click → Keychain Access → right-click → Get Info → Trust → Always Trust
- **Linux (Chrome/Edge):** Settings → Privacy → Manage Certificates → Authorities → Import
- **Linux system-wide:** `sudo cp caddy-root.crt /usr/local/share/ca-certificates/caddy.crt && sudo update-ca-certificates`

### Important: forwarded headers

The login rate limiter reads `X-Forwarded-For` to get the real client IP when sitting behind a proxy. Caddy sets this header automatically — no extra configuration needed.

### HTTPS and the session cookie

The session cookie has `Secure: true` set, which means it will only be sent over HTTPS. The dashboard will work correctly once Caddy is in front. If you access `http://` directly (without Caddy), the login cookie won't be sent by the browser and you will be redirected back to `/login` — this is expected behaviour.

---

## Security notes

- Without `set-admin` the dashboard has **no authentication**. Anyone on your network can access it. Set a password before exposing it outside a trusted LAN.
- The login endpoint rate-limits by IP: **5 failed attempts within 15 minutes** triggers a 15-minute lockout. The counter resets on a successful login.
- The session cookie is `HttpOnly`, `Secure`, and `SameSite=Strict`. The `Secure` flag means the cookie is only sent over HTTPS — use Caddy (or another TLS proxy) when exposing the dashboard publicly.
- mTLS ensures only agents with certificates signed by your CA can communicate with the dashboard. Each agent host must be enrolled separately.
- AI provider API keys are stored only as environment variables — never written to disk or exposed in the UI.
- `/var/lib/sddb` is created with mode `0700`. The config file is written with mode `0600`.
- The agent service runs as a dedicated `sddb` user (member of the `docker` group) with `NoNewPrivileges` and a read-only filesystem outside `/etc/sddb`.
