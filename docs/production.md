# Production deployment — split nodes + DB/Caddy VM

This setup runs the Go API server in Docker on **each Mac mini** (next to its
native CoreML/ANE audio sidecar), with **Postgres and Caddy on a separate
virtualized server**. Caddy load-balances client requests across the minis;
every mini talks to the same database.

```
                        ┌──────────────────────────────┐
                        │  DB VM (virtualized server)  │
                        │                              │
  clients ─────────────▶│  Caddy :80  (load balancer)  │
      http://db-vm/     │  Postgres :5432              │
                        └──────┬───────────────────────┘
                               │ least_conn + /health checks
            ┌──────────────────┼──────────────────┐
            ▼                  ▼                  ▼
   ┌────────────────┐ ┌────────────────┐ ┌────────────────┐
   │  Mac mini 1    │ │  Mac mini 2    │ │  Mac mini N    │
   │  server :3000  │ │  server :3000  │ │  server :3000  │   ← Docker
   │  presidio      │ │  presidio      │ │  presidio      │   ← Docker
   │  audio :8101   │ │  audio :8101   │ │  audio :8101   │   ← native (launchd)
   └───────┬────────┘ └───────┬────────┘ └───────┬────────┘
           └──────────────────┴──────────────────┘
                    DATABASE_URL → Postgres on DB VM
```

**Why server-per-mini (not one server + remote inference):** the sidecar
binds to localhost on each mini, so co-locating the API server keeps audio
payloads off the LAN entirely. JWTs are stateless — with the same
`JWT_SECRET` on every node, a token issued by any mini is valid on all of
them. The only shared state is Postgres. Each node sets a unique `SERVER_ID`
so Loki/stdout logs can be attributed per mini.

## Files

| File | Runs on | Contents |
|---|---|---|
| `docker-compose.node.yml` | each Mac mini | Go server + Presidio (no DB) |
| `docker-compose.db.yml` | DB VM | Postgres + Caddy |
| `Caddyfile.example` | DB VM | template → copy to `Caddyfile` (gitignored), list node IPs |
| `.env.node.example` | each Mac mini | node config template |
| `.env.db.example` | DB VM | DB config template |
| `deploy/macos/` | each Mac mini | launchd plists + headless boot guide |

## First-time setup

### 1. DB VM

```bash
cp .env.db.example .env          # set a strong POSTGRES_PASSWORD
cp Caddyfile.example Caddyfile   # list each mini's LAN IP under `to`
make db-up                       # Postgres + Caddy
```

Firewall Postgres to the node IPs only, e.g.:

```bash
sudo ufw allow from 192.168.1.0/24 to any port 5432 proto tcp
```

### 2. Each Mac mini

Follow `deploy/macos/README.md` for the headless boot configuration
(power-on after outage, FileVault off, auto-login, Docker at login), then:

```bash
make sidecar-install           # builds + installs the audio sidecar launchd agent
make sidecar-status            # verify it's up on :8101

cp .env.node.example .env      # DB VM IP in DATABASE_URL, shared JWT_SECRET,
chmod 600 .env                 # unique SERVER_ID for this mini
```

Generate the shared JWT secret **once** and copy it into every node's `.env`:

```bash
openssl rand -hex 32
```

### 3. Migrate + launch

```bash
# On ONE node only (first boot, and after upgrades):
make node-migrate

# Then on every node:
make node-up
```

Migrations also run automatically when each server starts, but running the
one-shot migrate first avoids concurrent-AutoMigrate races when the whole
fleet boots at once. The admin seed is safe: it skips if any user exists.

### 4. Verify

```bash
curl http://<mini-ip>:3000/health        # each node directly
curl http://<db-vm>/health               # through Caddy
```

## Adding another Mac mini

1. Set up the mini per `deploy/macos/README.md` (auto-login, pmset, Docker
   at login), then `make sidecar-install`.
2. `cp .env.node.example .env` — same `JWT_SECRET`/`DATABASE_URL`, new
   `SERVER_ID`.
3. `make node-up` on the mini.
4. Add its IP to the `to` list in `Caddyfile` on the DB VM.
5. `make caddy-reload` on the DB VM (zero downtime; health checks gate
   traffic until the node is ready).

## Caveats

- **Rate limits are per node.** The limiter is in-memory, so with N minis
  the effective per-user limit is roughly N × the configured value. Divide
  the configured limits by N if you need a hard global cap.
- **Cloned voices are node-local.** Voice-clone embedding files live in the
  `voicedata` volume of the node that created them; the DB metadata is
  shared, so the voice *appears* on all nodes but synthesis only works on
  the node holding the file. If you use cloning, create and use the voice
  via the same node (call that mini's IP directly for clone creation), or
  rsync `voicedata` between minis after creating a clone.
- **WebSockets** (live transcription) work through Caddy with no extra
  config; a connection stays pinned to whichever node accepted it, and
  `least_conn` keeps new connections away from busy nodes.
- **Power outages:** minis self-recover (pmset + auto-login + restart
  policies); Caddy's active health checks automatically stop routing to a
  dead mini and re-add it when it returns. Consider a UPS anyway to avoid
  cutting jobs mid-transcription.

## Recovery & restart procedures

Everything below is ordered from "happens on its own" to "you SSH in".

### Power outage — fully automatic, no action needed

1. Mini powers itself back on (`pmset autorestart 1`).
2. macOS auto-logs-in the service account (FileVault must be off).
3. Docker Desktop/OrbStack starts at login → `server` + `presidio`
   containers start (`restart: unless-stopped`).
4. LaunchAgent starts the audio sidecar (:8101).
5. Caddy's `/health` checks pass (~10–30 s) → traffic resumes to that mini.

Verify from any machine: `curl http://<mini-ip>:3000/health` and
`curl http://<db-vm>/health`. If both return OK, do nothing else.

> In-flight transcriptions at the moment of the outage are lost — clients
> should retry. Requests that had already been routed to other minis are
> unaffected.

### One mini is misbehaving — restart its stack

```bash
# On the mini (SSH in, or physically):
make node-down && make node-up          # restart server + presidio
make sidecar-restart                    # restart the audio sidecar launchd agent
```

Caddy keeps traffic on the remaining minis while this one is down; no
Caddy change is needed — health checks handle the drain and re-add.

### Full clean reboot of a mini

```bash
sudo reboot
```

Same automatic chain as a power outage. Optionally drain first by
temporarily removing the mini's IP from `Caddyfile` + `make caddy-reload`
on the DB VM, waiting for in-flight jobs to finish, then rebooting and
re-adding the IP.

### DB VM restart

```bash
sudo reboot        # docker + `restart: always` bring Postgres and Caddy back
```

While Postgres is down, **all** nodes fail — the API servers exit (they
abort on DB connection failure) and Docker restarts them until Postgres
returns. Expect a short full outage; keep DB VM reboots rare and announced.

### Caddy config change (add/remove nodes)

```bash
# On the DB VM, after editing Caddyfile:
make caddy-reload  # zero downtime; new upstreams gated by health checks
```

### Something won't come back — triage order

| Check | Where | Fix |
|---|---|---|
| `curl http://<mini-ip>:8101/health` | mini | Audio sidecar down → `make sidecar-status` / `make sidecar-restart`; logs in `deploy/macos/logs/` |
| `docker compose -f docker-compose.node.yml ps` | mini | Containers down → `make node-up`; check `make node-logs` |
| `curl http://<mini-ip>:3000/health` | mini | Server up but sidecar dead → fix sidecar first |
| `curl http://<db-vm>/health` | anywhere | Caddy up but a mini missing → that mini's health check is failing; check the mini, not Caddy |
| `docker compose -f docker-compose.db.yml logs db` | DB VM | Postgres unhealthy → nodes can't boot; restore DB first |
| Machine dark after outage | physically | FileVault password screen? (disable FileVault) — or `pmset -g` to confirm `autorestart 1` |

### Losing a mini permanently (hardware failure)

Remove its IP from `Caddyfile`, `make caddy-reload`. Its in-flight work is
lost; everything else continues. The replacement mini follows "Adding
another Mac mini" below.

## Upgrading

```bash
# On each node, one at a time (Caddy health checks keep traffic on the rest):
git pull
make node-migrate      # from the FIRST upgraded node only, if schema changed
make node-up
make sidecar-deploy    # if the audio sidecar changed — builds, re-signs,
                       # restarts, and waits for /health (prints the build SHA)
```

Two deployment gotchas, both fixed by the targets above:

- **Legacy shim.** Nodes installed during the rename transition may still
  have the `com.gotranscribesrv.swift-sidecar` agent loaded alongside the
  primary one — both compete for :8101, and a shim baked with an older repo
  path serves the OLD binary. Check `make sidecar-status`; if the shim is
  loaded, run `make sidecar-remove-shim` once per node.
- **Codesigning flake.** Replacing the release binary in place can make
  launchd kill the first spawn (`OS_REASON_CODESIGNING`). `audio-build`
  re-signs after every build and `sidecar-deploy` waits for health, so a
  failed first spawn can't go unnoticed.

Verify what's actually running on each node after an upgrade:

```bash
curl -s http://<mini-ip>:8101/health | jq .build   # {"sha":"…","built_at":"…"} — must match the deployed commit
```

## Database backups

Backups run on the **DB VM** and write compressed `pg_dump` files (custom
format) to `./backups/` (gitignored):

```bash
make db-backup                                    # one-off backup
make db-restore FILE=backups/transcribesrv-....dump   # restore (drops/recreates objects)
```

For automatic nightly backups, add a cron entry on the DB VM:

```cron
0 3 * * * cd /path/to/gotranscribesrv && make db-backup
```

Plus retention — e.g. delete dumps older than 14 days:

```cron
15 3 * * * find /path/to/gotranscribesrv/backups -name '*.dump' -mtime +14 -delete
```

Notes:

- Dumps are node-independent — every node shares this one database, so a
  single backup covers the whole fleet (users, API keys, usage logs, voice
  metadata). **Exception:** cloned-voice embedding *files* live per-node in
  the `voicedata` Docker volume; back those up separately if you use cloning
  (`docker run --rm -v gotranscribesrv_voicedata:/v -v $PWD/backups:/b alpine tar czf /b/voicedata-<node>.tgz -C /v .`).
- `db-restore` uses `pg_restore --clean --if-exists`: it drops existing
  objects before recreating them. Stop the nodes (`make node-down`) during a
  restore to avoid servers writing to a half-restored schema.
- Keep `backups/` on a different disk/machine than the DB VM itself (rsync,
  object storage) for real disaster recovery.
