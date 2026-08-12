# Headless macOS node setup (Mac mini)

This folder contains everything needed to run a Mac mini as an unattended
GoTranscribeSrv **node** that recovers from power outages and reboots with
**no one logging in**.

Two things must start automatically after boot:

1. **Docker containers** (Go server + Presidio) — via Docker Desktop/OrbStack
   "start at login" + `restart: unless-stopped` in `docker-compose.node.yml`.
2. **Native sidecar** (Swift on :8101 — ASR, VAD, diarization, TTS) — via the
   LaunchAgent in this folder. It must run natively for CoreML/ANE access.

> The DB VM (Postgres + Caddy) is a separate machine — see
> `docker-compose.db.yml` and `docs/production.md`.

---

## 1. Hardware / OS settings (once per mini)

```bash
# Automatically power back on after a power failure
sudo pmset -a autorestart 1

# Never sleep (server duty); display can sleep
sudo pmset -a sleep 0 displaysleep 10

# Reboot automatically after a freeze/kernel panic
sudo systemsetup -setrestartfreeze on
```

**Disable FileVault** (System Settings → Privacy & Security → FileVault).
This is required: with FileVault on, the machine stops at the pre-boot
password screen after a power outage and never reaches macOS. The mitigations
for a disk-encryption-less mini are physical security and keeping secrets in
`.env` readable only by the service account (`chmod 600 .env`).

**Create a service account** (e.g. `transcribe`) and enable **automatic
login** for it (System Settings → Users & Groups → Automatic login). The
machine will then boot straight into that session with no input. Lock the
screen via a screensaver password if the minis are physically accessible —
auto-login does not mean unlocked remote access.

## 2. Docker auto-start

Docker Desktop **or** OrbStack:

- Enable **"Start at login"** (Docker Desktop: Settings → General;
  OrbStack: Settings → General).
- The compose file already sets `restart: unless-stopped` on every service,
  so containers come up with Docker.

## 3. Install the sidecar LaunchAgent

From a checkout of this repo on the mini:

```bash
# Build the Swift sidecar release binary first
make swift-build

# Point the plist at your repo path
REPO=/Users/transcribe/gotranscribesrv    # ← your actual checkout path
mkdir -p "$REPO/deploy/macos/logs"
sed -i '' "s|__REPO_PATH__|$REPO|g" deploy/macos/com.gotranscribesrv.swift-sidecar.plist

# Install into the service account's LaunchAgents
mkdir -p ~/Library/LaunchAgents
cp deploy/macos/com.gotranscribesrv.swift-sidecar.plist ~/Library/LaunchAgents/

# Load it (starts now and at every login)
launchctl load ~/Library/LaunchAgents/com.gotranscribesrv.swift-sidecar.plist
```

Notes:

- The Swift plist runs the **release binary** (`.build/release/Server`), not
  `swift run`, so boot startup is fast and doesn't need a toolchain warmup.
- `KeepAlive` restarts a crashed sidecar; `ThrottleInterval` prevents
  crash-loop spinning. Check `deploy/macos/logs/*.log` if a sidecar won't
  stay up.
- After `git pull` + `make swift-build`, restart the sidecar:
  `launchctl kickstart -k gui/$(id -u)/com.gotranscribesrv.swift-sidecar`

## 4. Start the node stack

```bash
cp .env.node.example .env   # edit: DB VM IP, shared JWT_SECRET, unique SERVER_ID
chmod 600 .env
make node-up                # builds + starts server + presidio
```

## 5. Verify headless recovery (once per mini)

1. `sudo reboot` — do **not** log in via console; give it a few minutes.
2. From another machine: `curl http://<mini-ip>:3000/health` → OK (Docker +
   containers came up).
3. `curl http://<mini-ip>:8101/health` (Swift sidecar) → OK (LaunchAgent ran).
4. `curl http://<caddy-ip>/health` → OK (Caddy re-added the node after its
   health checks passed).
5. Pull the plug (or `sudo pmset` schedule a power event), restore power —
   machine boots on its own (`autorestart`), repeats steps 2–4 unattended.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Machine doesn't power on after outage | `pmset autorestart` not set; also check the mini isn't on a switched power strip |
| Boots to a password screen, never logs in | FileVault still enabled, or auto-login not configured |
| Containers down but sidecars up | Docker Desktop not set to start at login |
| Sidecars down, containers up | Plists not in `~/Library/LaunchAgents` of the auto-login account, or `__REPO_PATH__` not substituted |
| Server up but transcriptions fail | `host.docker.internal` unreachable — confirm Docker Desktop/OrbStack (not colima) is the runtime |
