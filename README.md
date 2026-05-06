# SDDB — Container Dashboard

A lightweight self-hosted dashboard for monitoring Docker containers across multiple hosts.
Agents run on each host and report to a central dashboard over mTLS.

---

## Overview

```
[agent host 1] ──┐
[agent host 2] ──┼──► [dashboard] ──► browser
[agent host 3] ──┘
```

- **Dashboard** — web UI, runs on one machine, polls agents every 5 seconds
- **Agent** — runs on every host you want to monitor, exposes a local HTTP API

---

## Building

```bash
make build
```

Binaries are written to `bin/`:
- `bin/sddb-agent`
- `bin/sddb-dashboard`

Cross-compile for Linux from Mac:
```bash
make linux
```

---

## Dashboard Setup (run once, on the dashboard machine)

### 1. Install the service

```bash
sudo make install-dashboard-service
```

### 2. Set the admin account

```bash
sudo sddb-dashboard set-admin -data-dir /var/lib/sddb
```

Prompts for a username and password. Run this again any time you want to change credentials (restart required after).

### 3. Start the dashboard

```bash
sudo systemctl enable --now sddb-dashboard
```

Open `http://<dashboard-host>:8080` in your browser.

### 4. Change the port (optional)

```bash
sudo systemctl edit sddb-dashboard
```

Add:
```ini
[Service]
ExecStart=
ExecStart=/usr/local/bin/sddb-dashboard -addr :9000 -poll 5s -data-dir /var/lib/sddb
```

Then restart:
```bash
sudo systemctl restart sddb-dashboard
```

---

## Adding an Agent Host

Do this once per host you want to monitor.

### 1. Enroll the host (on the dashboard machine)

```bash
sudo sddb-dashboard enroll <name> -data-dir /var/lib/sddb
```

Replace `<name>` with a label for that machine, e.g. `homeserver`.

This creates three files and prints the exact commands to copy them across. Copy them to `/etc/sddb/` on the agent host:

```bash
scp <name>-agent.crt <name>-agent.key <name>-ca.crt user@<host>:/etc/sddb/
```

### 2. Install the agent on the host

From your dev/dashboard machine, copy the binary and service file across:

```bash
scp bin/sddb-agent user@<host>:/tmp/sddb-agent
scp deploy/sddb-agent.service user@<host>:/tmp/sddb-agent.service
```

Then SSH in and install them:

```bash
ssh user@<host>
sudo install -m 755 /tmp/sddb-agent /usr/local/bin/sddb-agent
sudo install -m 644 /tmp/sddb-agent.service /etc/systemd/system/sddb-agent.service
sudo systemctl daemon-reload
```

### 3. Configure mTLS on the agent

```bash
sudo systemctl edit sddb-agent
```

Add (using the cert filenames from enroll):
```ini
[Service]
ExecStart=
ExecStart=/usr/local/bin/sddb-agent -addr :8484 -interval 5s \
  -tls-cert /etc/sddb/<name>-agent.crt \
  -tls-key  /etc/sddb/<name>-agent.key \
  -tls-ca   /etc/sddb/<name>-ca.crt
```

### 4. Start the agent

```bash
sudo systemctl enable --now sddb-agent
```

### 5. Add the agent to the dashboard

Either click **Add Agent** in the UI and enter `<host-ip>:8484`, or use the network scan button to auto-discover agents on your local network.

---

## Certificates

Certificates are issued by the dashboard's CA and stored in `/var/lib/sddb/` on the dashboard machine.

| File | Validity | Notes |
|---|---|---|
| `ca.crt` / `ca.key` | 20 years | Dashboard CA — keep the key safe |
| `<name>-agent.crt/key` | 10 years | One pair per agent host |
| `dashboard.crt/key` | 10 years | Dashboard client cert, auto-generated |

You only need to re-run `enroll` if you add a new host or lose the cert files.

---

## Changing the Agent Port

The default agent port is `8484`. To change it, edit the systemd override on the agent host:

```bash
sudo systemctl edit sddb-agent
```

Update `-addr` in the ExecStart line. Then on the dashboard, add the agent using the custom port: `<host-ip>:<port>`.

---

## Running Without mTLS (plain HTTP)

If you haven't run `enroll`, agents and dashboard communicate over plain HTTP with no authentication.
This is fine for a trusted LAN but not recommended if the dashboard is exposed externally.

The default service files start in plain HTTP mode. mTLS is enabled automatically once certs exist and the agent is configured with the TLS flags.

---

## Makefile Reference

| Target | What it does |
|---|---|
| `make build` | Build both binaries to `bin/` |
| `make agent` | Build agent only |
| `make dashboard` | Build dashboard only |
| `make linux` | Cross-compile for Linux amd64 |
| `make install` | Copy binaries to `/usr/local/bin` |
| `make install-agent-service` | Install agent binary + systemd service |
| `make install-dashboard-service` | Install dashboard binary + systemd service |
| `make clean` | Remove `bin/` |
