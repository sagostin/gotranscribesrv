# TODO

## Phase 2 — Multi-node cloned voice sharing (status: implemented 2026-08-14, live verification pending)

**Problem:** Cloned voice embeddings are stored only as local files
(`VOICES_DATA_DIR`, default `data/voices/{user_id}/{voice_id}.bin`). The
`voices` DB row holds only the relative `FilePath` — not the bytes. In a
multi-node deployment (see `server_id`), a voice cloned on node A returns
`VOICE_NOT_FOUND` when synthesized through node B.

**Design (agreed 2026-08-14):** DB becomes the source of truth; per-node disk
becomes a write-through cache.

- [x] Add `embedding_data bytea` to `models.Voice` (AutoMigrate; embeddings
      are small — KBs — so a blob column is fine, no object storage needed).
- [x] `POST /api/v1/voices/clone`: write embedding blob to DB **and** local
      file (write-through).
- [x] `LoadVoiceData` (TTS path): local disk hit → serve; miss → fetch blob
      from DB → write-through to local disk → serve; both miss → 404.
- [x] Startup sync (`SyncVoiceStorage`, called from `cmd/server/main.go`):
      - **Backfill** disk → DB: rows with NULL `embedding_data` but an
        existing local file get their blob stored (covers voices cloned
        before this change, and this node's pre-existing files).
      - **Forward-fill** DB → disk: rows with a blob but no local file get
        the file materialized (covers voices cloned on other nodes).
- [x] `DELETE /api/v1/voices/:id`: removes local file + DB row (blob goes
      with the row). Deletion propagates to other nodes for synthesis
      immediately (DB row is checked before the disk cache); each node's
      stale cache files are removed by the **orphan sweep** in
      `SyncVoiceStorage` at boot.
- [ ] **Live verification pending:** multi-node clone→synthesize flow needs a
      running postgres (local docker daemon was off during implementation).
      Verify on mm2/staging: clone on node A → `POST /api/v1/tts` with that
      `voice_id` via node B → 200 audio; delete a voice on node A → node B's
      cached file is swept on its next boot; check Loki for
      `VOICE_STORAGE_SYNCED` backfill/forward-fill/orphan counts.

**Done when:** clone on node A → synthesize via node B works without manual
file copies; restarting any node with pre-existing local files backfills the
DB.

---

### Completed

- **2026-08-14 — TTS voice resolution fix + logging audit (Phase 1).**
  Sidecar `VoiceResolver` maps `nil`/""/`"default"` to the real per-backend
  default (PocketTTS `alba`, Kokoro `af_heart`), auto-reroutes cross-backend
  voices, and 422s unknown voices — fixes the FluidAudio
  `default.safetensors` 404 loop that surfaced as gateway 502s. Go gateway
  now forwards sidecar 4xx instead of masking as 502, and all TTS endpoints
  (REST, OpenAI-compat, realtime S2S, voice management) emit Loki events.
