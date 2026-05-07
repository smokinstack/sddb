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
- **Live container stats** — CPU, memory, network I/O, ports, compose project
- **Update detection** — checks Docker Hub, GCR, GHCR, and any OCI-compatible registry
- **One-click upgrade** — pulls the latest image and recreates the container (compose-aware)
- **Auto-update** — per-container toggle; the dashboard upgrades automatically when a new image is available, with a 10-minute cooldown between attempts
- **Log viewer** — view the last N lines of any container's logs, with search/filter and clipboard copy
- **AI analysis** — send logs or a health prompt to Claude, ChatGPT, or a local Ollama model for instant analysis
- **Settings** — choose which AI provider to use when multiple are configured
- **Admin authentication** — optional login; unprotected by default until you run `set-admin`
- **mTLS security** — all agent communication is certificate-authenticated
- **Systemd service files** — included for both components

---

## Requirements

- Go 1.22+ (to build)
- Docker running on each monitored host
- `docker compose` plugin (or legacy `docker-compose`) on agent hosts if you use Compose

---

## Building

```bash
git clone <repo>
cd sddb

# Build both binaries into bin/
make build

# Cross-compile for Linux amd64 (e.g. building on a Mac)
make linux
```

Binaries are written to `bin/sddb-agent` and `bin/sddb-dashboard`.

---

## Installation

### Dashboard host

The dashboard can run on any machine that has network access to your agent hosts. It does not need Docker installed.

```bash
sudo make install-dashboard-service
```

This installs the binary to `/usr/local/bin/sddb-dashboard` and the systemd unit to `/etc/systemd/system/sddb-dashboard.service`. Data (PKI, config, agent list) is stored in `/var/lib/sddb`.

**Set an admin password** (optional but recommended):

```bash
sudo sddb-dashboard set-admin -data-dir /var/lib/sddb
```

You will be prompted for a username and password. If you skip this step the dashboard is accessible without login — add it any time and restart the service to activate it.

**Start the service:**

```bash
sudo systemctl enable --now sddb-dashboard
```

The dashboard listens on port `8080` by default. Open `http://<host>:8080` in your browser.

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

```bash
scp /tmp/certs/<hostname>-agent.crt \
    /tmp/certs/<hostname>-agent.key \
    /tmp/certs/<hostname>-ca.crt \
    user@<agent-host>:/etc/sddb/
```

Create the directory first if needed:

```bash
ssh user@<agent-host> "sudo mkdir -p /etc/sddb"
```

#### Step 3 — Install the agent binary

```bash
scp bin/sddb-agent user@<agent-host>:/tmp/sddb-agent
ssh user@<agent-host> "sudo install -m 755 /tmp/sddb-agent /usr/local/bin/sddb-agent"
```

#### Step 4 — Install the systemd unit

Copy `deploy/sddb-agent.service` to the agent host:

```bash
scp deploy/sddb-agent.service user@<agent-host>:/tmp/
ssh user@<agent-host> "sudo cp /tmp/sddb-agent.service /etc/systemd/system/ && sudo systemctl daemon-reload"
```

#### Step 5 — Configure mTLS for the agent

On the agent host, create a systemd override to add the certificate flags:

```bash
sudo systemctl edit sddb-agent
```

Add:

```ini
[Service]
ExecStart=
ExecStart=/usr/local/bin/sddb-agent -addr :8484 -interval 5s \
  -tls-cert /etc/sddb/<hostname>-agent.crt \
  -tls-key  /etc/sddb/<hostname>-agent.key \
  -tls-ca   /etc/sddb/<hostname>-ca.crt
```

Then start:

```bash
sudo systemctl enable --now sddb-agent
```

#### Step 6 — Add the agent to the dashboard

Open the dashboard, click **+ Add Agent**, enter the agent's IP and port (e.g. `192.168.1.10:8484`), and optionally give it a label. It will appear as online within one poll cycle.

---

## Upgrading binaries

After rebuilding with `make build`:

**Dashboard:**
```bash
sudo make install && sudo systemctl restart sddb-dashboard
```

**Each agent host:**
```bash
scp bin/sddb-agent user@<host>:/tmp/sddb-agent
ssh user@<host> "sudo install -m 755 /tmp/sddb-agent /usr/local/bin/sddb-agent && sudo systemctl restart sddb-agent"
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

## Security notes

- Without `set-admin` the dashboard has **no authentication**. Anyone on your network can access it. Set a password before exposing it outside a trusted LAN.
- mTLS ensures only agents with certificates signed by your CA can communicate with the dashboard. Each agent host must be enrolled separately.
- AI provider API keys are stored only as environment variables — never written to disk or exposed in the UI.
- `/var/lib/sddb` is created with mode `0700`. The config file is written with mode `0600`.
