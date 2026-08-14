package handlers

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/shaunagostinho/gotranscribesrv/internal/logging"
	"github.com/shaunagostinho/gotranscribesrv/internal/middleware"
	"github.com/shaunagostinho/gotranscribesrv/internal/models"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
	"gorm.io/gorm"
)

// PocketTTS voice-prompt geometry, mirroring FluidAudio 0.15.5's
// PocketTtsVoiceCloner: embeddings are packed little-endian Float32
// [frames × 1024], and loadVoice rejects prompts over 125 frames
// ("Voice file too large: N frames (max 125)"). FluidAudio ≤ 0.13.6
// allowed up to 250 frames, so voices cloned before the 0.15.5 upgrade
// can exceed the new cap. The over-cap tail is just extra reference
// audio (the new cloner itself truncates input to the first 10s), so
// truncating to the first maxVoiceEmbeddingFrames frames is lossless.
const (
	voiceEmbeddingFrameBytes = 1024 * 4 // one frame: 1024 float32
	maxVoiceEmbeddingFrames  = 125
	maxVoiceEmbeddingBytes   = voiceEmbeddingFrameBytes * maxVoiceEmbeddingFrames // 512_000
)

// truncateVoiceEmbedding caps a PocketTTS voice embedding at
// maxVoiceEmbeddingFrames frames, returning the (possibly shortened)
// data and whether it changed. Data whose length is not a whole number
// of frames is left untouched — that's corruption, not oversize.
func truncateVoiceEmbedding(data []byte) ([]byte, bool) {
	if len(data) <= maxVoiceEmbeddingBytes || len(data)%voiceEmbeddingFrameBytes != 0 {
		return data, false
	}
	return data[:maxVoiceEmbeddingBytes], true
}

// VoiceHandler manages per-user voice cloning and listing.
type VoiceHandler struct {
	db        *gorm.DB
	sidecar   *sidecar.Client
	voicesDir string // Base directory for voice embedding files
	lm        *logging.LogManager
}

// NewVoiceHandler creates a new VoiceHandler.
func NewVoiceHandler(db *gorm.DB, sc *sidecar.Client, voicesDir string, lm *logging.LogManager) *VoiceHandler {
	// Ensure the voices directory exists
	if err := os.MkdirAll(voicesDir, 0755); err != nil {
		slog.Error("failed to create voices directory", "path", voicesDir, "error", err)
	}
	return &VoiceHandler{db: db, sidecar: sc, voicesDir: voicesDir, lm: lm}
}

// Clone uploads audio and extracts a voice embedding for reuse.
// POST /api/v1/voices/clone
func (h *VoiceHandler) Clone(c *fiber.Ctx) error {
	userID, err := middleware.ParseUserID(c)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "UNAUTHORIZED",
				"message": "Authentication required",
				"status":  401,
			},
		})
	}

	// Parse form fields
	name := c.FormValue("name")
	if name == "" {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "MISSING_NAME",
				"message": "Voice name is required",
				"status":  422,
			},
		})
	}
	description := c.FormValue("description")

	// Read uploaded audio file
	file, err := c.FormFile("audio")
	if err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "MISSING_AUDIO",
				"message": "Audio file is required (form field: audio)",
				"status":  422,
			},
		})
	}

	// Limit file size to 10MB
	if file.Size > 10*1024*1024 {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "FILE_TOO_LARGE",
				"message": "Audio file must be 10MB or less",
				"status":  422,
			},
		})
	}

	f, err := file.Open()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "FILE_READ_ERROR",
				"message": "Failed to read uploaded file",
				"status":  500,
			},
		})
	}
	defer f.Close()

	audioBytes, err := io.ReadAll(f)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "FILE_READ_ERROR",
				"message": "Failed to read uploaded file",
				"status":  500,
			},
		})
	}

	voice, audioDurationMs, cloneMs, opErr := h.cloneVoiceCore(c.UserContext(), userID, name, description, audioBytes, file.Size, "/api/v1/voices/clone", c.IP(), middleware.RequestIDFromCtx(c))
	if opErr != nil {
		return c.Status(opErr.httpStatus).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    opErr.code,
				"message": opErr.msg,
				"status":  opErr.httpStatus,
			},
		})
	}

	// Set audio_duration_ms for the usage middleware (actual audio duration)
	c.Locals("audio_duration_ms", audioDurationMs)

	c.Locals("usage_meta", map[string]interface{}{
		"voice_name":        name,
		"embedding_size":    voice.SizeBytes,
		"audio_size":        file.Size,
		"clone_time_ms":     cloneMs,
		"audio_duration_ms": audioDurationMs,
	})

	return c.Status(fiber.StatusCreated).JSON(voice.ToResponse())
}

// voiceOpError carries an HTTP-mappable failure from the voice storage core.
type voiceOpError struct {
	httpStatus int
	code       string
	msg        string
}

// cloneVoiceCore is the dialect-independent voice-cloning pipeline shared by
// the native POST /api/v1/voices/clone and the ElevenLabs-compatible
// POST /v1/voices/add: name-uniqueness check, sidecar embedding extraction,
// local file write, and DB record (with the embedding blob for multi-node
// sharing). Loki events are emitted from here so both dialects log
// identically. Returns the voice, source-audio duration, clone latency.
func (h *VoiceHandler) cloneVoiceCore(ctx context.Context, userID uuid.UUID, name, description string, audioBytes []byte, fileSize int64, endpoint, ip, requestID string) (*models.Voice, int, int, *voiceOpError) {
	// Check name uniqueness for this user
	var existing models.Voice
	if result := h.db.Where("user_id = ? AND name = ?", userID, name).First(&existing); result.Error == nil {
		return nil, 0, 0, &voiceOpError{fiber.StatusConflict, "VOICE_EXISTS", fmt.Sprintf("Voice named %q already exists", name)}
	}

	h.lm.SendLog(h.lm.BuildLog("VOICE_CLONE_STARTED", "VoiceCloneStarted", slog.LevelInfo, map[string]interface{}{
		"endpoint":   endpoint,
		"ip":         ip,
		"user_id":    userID.String(),
		"name":       name,
		"file_size":  fileSize,
		"request_id": requestID,
	}))

	// Send to sidecar to extract voice embedding
	cloneStart := time.Now()
	embedding, audioDurationMs, err := h.sidecar.CloneVoice(ctx, audioBytes, "audio")
	cloneMs := int(time.Since(cloneStart).Milliseconds())
	if err != nil {
		errMsg := err.Error()
		h.lm.SendLog(h.lm.BuildLog("VOICE_CLONE_FAILED", "VoiceCloneFailed", slog.LevelError, map[string]interface{}{
			"endpoint":      endpoint,
			"ip":            ip,
			"user_id":       userID.String(),
			"name":          name,
			"file_size":     fileSize,
			"clone_time_ms": cloneMs,
			"request_id":    requestID,
		}, err))
		// If the sidecar returned a specific error (e.g. audio too long/short),
		// forward the actual message to the client
		return nil, 0, cloneMs, &voiceOpError{fiber.StatusUnprocessableEntity, "CLONE_FAILED", errMsg}
	}

	// Create voice record
	voiceID := uuid.New()
	relPath := filepath.Join(userID.String(), voiceID.String()+".bin")
	absPath := filepath.Join(h.voicesDir, relPath)

	// Ensure user directory exists
	if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		h.lm.SendLog(h.lm.BuildLog("VOICE_CLONE_DIR_ERROR", "VoiceCloneDirError", slog.LevelError, map[string]interface{}{
			"endpoint":   endpoint,
			"ip":         ip,
			"user_id":    userID.String(),
			"name":       name,
			"path":       filepath.Dir(absPath),
			"request_id": requestID,
		}, err))
		return nil, 0, cloneMs, &voiceOpError{fiber.StatusInternalServerError, "STORAGE_ERROR", "Failed to create voice storage"}
	}

	// Write embedding to disk (per-node cache; the DB blob below is the
	// source of truth for multi-node sharing)
	if err := os.WriteFile(absPath, embedding, 0644); err != nil {
		h.lm.SendLog(h.lm.BuildLog("VOICE_CLONE_WRITE_ERROR", "VoiceCloneWriteError", slog.LevelError, map[string]interface{}{
			"endpoint":   endpoint,
			"ip":         ip,
			"user_id":    userID.String(),
			"name":       name,
			"path":       absPath,
			"request_id": requestID,
		}, err))
		return nil, 0, cloneMs, &voiceOpError{fiber.StatusInternalServerError, "STORAGE_ERROR", "Failed to store voice embedding"}
	}

	audioDurationSec := float64(audioDurationMs) / 1000.0

	voice := models.Voice{
		ID:            voiceID,
		UserID:        userID,
		Name:          name,
		Description:   description,
		FilePath:      relPath,
		EmbeddingData: embedding, // DB is source of truth; disk file is a cache
		SizeBytes:     int64(len(embedding)),
		DurationSec:   audioDurationSec,
	}

	if result := h.db.Create(&voice); result.Error != nil {
		// Clean up the file if DB insert fails
		_ = os.Remove(absPath)
		h.lm.SendLog(h.lm.BuildLog("VOICE_CLONE_DB_ERROR", "VoiceCloneDBError", slog.LevelError, map[string]interface{}{
			"endpoint":   endpoint,
			"ip":         ip,
			"user_id":    userID.String(),
			"name":       name,
			"voice_id":   voiceID.String(),
			"request_id": requestID,
		}, result.Error))
		return nil, 0, cloneMs, &voiceOpError{fiber.StatusInternalServerError, "DB_ERROR", "Failed to save voice record"}
	}

	slog.Info("voice cloned successfully",
		"voice_id", voiceID, "user_id", userID, "name", name,
		"embedding_size", len(embedding), "clone_time_ms", cloneMs,
		"audio_duration_ms", audioDurationMs)

	h.lm.SendLog(h.lm.BuildLog("VOICE_CLONE_COMPLETED", "VoiceCloneCompleted", slog.LevelInfo, map[string]interface{}{
		"endpoint":          endpoint,
		"ip":                ip,
		"user_id":           userID.String(),
		"voice_id":          voiceID.String(),
		"name":              name,
		"embedding_bytes":   len(embedding),
		"clone_time_ms":     cloneMs,
		"audio_duration_ms": audioDurationMs,
		"request_id":        requestID,
	}))

	return &voice, audioDurationMs, cloneMs, nil
}

// List returns the user's custom voices and system/built-in voices.
// GET /api/v1/voices
func (h *VoiceHandler) List(c *fiber.Ctx) error {
	userID, err := middleware.ParseUserID(c)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "UNAUTHORIZED",
				"message": "Authentication required",
				"status":  401,
			},
		})
	}

	// Fetch user's custom voices
	var voices []models.Voice
	if result := h.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&voices); result.Error != nil {
		h.lm.SendLog(h.lm.BuildLog("VOICE_LIST_DB_ERROR", "VoiceListDBError", slog.LevelError, map[string]interface{}{
			"endpoint":   "/api/v1/voices",
			"ip":         c.IP(),
			"user_id":    userID.String(),
			"request_id": middleware.RequestIDFromCtx(c),
		}, result.Error))
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "DB_ERROR",
				"message": "Failed to fetch voices",
				"status":  500,
			},
		})
	}

	customVoices := make([]models.VoiceResponse, 0, len(voices))
	for _, v := range voices {
		customVoices = append(customVoices, v.ToResponse())
	}
	// Fetch system voices from sidecar
	systemVoices := make([]models.VoiceResponse, 0)
	sidecarVoices, err := h.sidecar.ListVoices()
	if err != nil {
		slog.Warn("failed to fetch system voices from sidecar", "error", err)
		h.lm.SendLog(h.lm.BuildLog("VOICE_LIST_SIDECAR_FAILED", "VoiceListSidecarFailed", slog.LevelWarn, map[string]interface{}{
			"endpoint":   "/api/v1/voices",
			"ip":         c.IP(),
			"user_id":    userID.String(),
			"request_id": middleware.RequestIDFromCtx(c),
		}, err))
		// Non-fatal — still return custom voices
	} else {
		for _, sv := range sidecarVoices.Voices {
			systemVoices = append(systemVoices, models.VoiceResponse{
				Name:        sv.Name,
				Description: sv.Description,
				Type:        "system",
			})
		}
	}

	return c.JSON(fiber.Map{
		"custom": customVoices,
		"system": systemVoices,
	})
}

// Get returns a single voice's details.
// GET /api/v1/voices/:id
func (h *VoiceHandler) Get(c *fiber.Ctx) error {
	userID, err := middleware.ParseUserID(c)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "UNAUTHORIZED",
				"message": "Authentication required",
				"status":  401,
			},
		})
	}

	voiceID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "INVALID_ID",
				"message": "Invalid voice ID",
				"status":  400,
			},
		})
	}

	var voice models.Voice
	if result := h.db.Where("id = ? AND user_id = ?", voiceID, userID).First(&voice); result.Error != nil {
		h.lm.SendLog(h.lm.BuildLog("VOICE_NOT_FOUND", "VoiceNotFound", slog.LevelWarn, map[string]interface{}{
			"endpoint":   "/api/v1/voices/:id",
			"ip":         c.IP(),
			"user_id":    userID.String(),
			"voice_id":   voiceID.String(),
			"request_id": middleware.RequestIDFromCtx(c),
		}))
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "NOT_FOUND",
				"message": "Voice not found",
				"status":  404,
			},
		})
	}

	return c.JSON(voice.ToResponse())
}

// Delete soft-deletes a voice and removes the embedding file.
// DELETE /api/v1/voices/:id
func (h *VoiceHandler) Delete(c *fiber.Ctx) error {
	userID, err := middleware.ParseUserID(c)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "UNAUTHORIZED",
				"message": "Authentication required",
				"status":  401,
			},
		})
	}

	voiceID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "INVALID_ID",
				"message": "Invalid voice ID",
				"status":  400,
			},
		})
	}

	_, opErr := h.deleteVoiceCore(userID, voiceID, "/api/v1/voices/:id", c.IP(), middleware.RequestIDFromCtx(c))
	if opErr != nil {
		return c.Status(opErr.httpStatus).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    opErr.code,
				"message": opErr.msg,
				"status":  opErr.httpStatus,
			},
		})
	}

	return c.JSON(fiber.Map{
		"message": "Voice deleted",
	})
}

// deleteVoiceCore is the dialect-independent delete shared by the native
// DELETE /api/v1/voices/:id and the ElevenLabs-compatible
// DELETE /v1/voices/:voice_id: removes the local embedding file (other
// nodes drop theirs via their own caches) and soft-deletes the DB row
// (which also drops the shared embedding blob).
func (h *VoiceHandler) deleteVoiceCore(userID, voiceID uuid.UUID, endpoint, ip, requestID string) (*models.Voice, *voiceOpError) {
	var voice models.Voice
	if result := h.db.Where("id = ? AND user_id = ?", voiceID, userID).First(&voice); result.Error != nil {
		h.lm.SendLog(h.lm.BuildLog("VOICE_NOT_FOUND", "VoiceNotFound", slog.LevelWarn, map[string]interface{}{
			"endpoint":   endpoint,
			"ip":         ip,
			"user_id":    userID.String(),
			"voice_id":   voiceID.String(),
			"request_id": requestID,
		}))
		return nil, &voiceOpError{fiber.StatusNotFound, "NOT_FOUND", "Voice not found"}
	}

	// Remove the embedding file
	absPath := filepath.Join(h.voicesDir, voice.FilePath)
	if err := os.Remove(absPath); err != nil && !os.IsNotExist(err) {
		slog.Warn("failed to remove voice embedding file", "path", absPath, "error", err)
	}

	// Soft-delete the record
	if result := h.db.Delete(&voice); result.Error != nil {
		h.lm.SendLog(h.lm.BuildLog("VOICE_DELETE_DB_ERROR", "VoiceDeleteDBError", slog.LevelError, map[string]interface{}{
			"endpoint":   endpoint,
			"ip":         ip,
			"user_id":    userID.String(),
			"voice_id":   voiceID.String(),
			"request_id": requestID,
		}, result.Error))
		return nil, &voiceOpError{fiber.StatusInternalServerError, "DB_ERROR", "Failed to delete voice"}
	}

	slog.Info("voice deleted", "voice_id", voiceID, "user_id", userID, "name", voice.Name)

	h.lm.SendLog(h.lm.BuildLog("VOICE_DELETED", "VoiceDeleted", slog.LevelInfo, map[string]interface{}{
		"endpoint":   endpoint,
		"ip":         ip,
		"user_id":    userID.String(),
		"voice_id":   voiceID.String(),
		"name":       voice.Name,
		"request_id": requestID,
	}))

	return &voice, nil
}

// listCustomVoices returns the user's cloned voices, newest first.
// Best-effort for compat-layer list endpoints: returns nil on DB error.
func (h *VoiceHandler) listCustomVoices(userID uuid.UUID) []models.Voice {
	var voices []models.Voice
	if result := h.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&voices); result.Error != nil {
		return nil
	}
	return voices
}

// LoadVoiceData reads the stored embedding for a voice and returns base64.
// Used by TTSHandler when synthesizing with a stored voice_id.
//
// Multi-node: the local disk file is a per-node cache; the DB blob is the
// source of truth. On a local miss (voice was cloned on another node) the
// blob is fetched from the DB and written through to disk for next time.
func (h *VoiceHandler) LoadVoiceData(voiceID, userID uuid.UUID) (string, error) {
	var voice models.Voice
	if result := h.db.Where("id = ? AND user_id = ?", voiceID, userID).First(&voice); result.Error != nil {
		return "", fmt.Errorf("voice not found")
	}

	absPath := filepath.Join(h.voicesDir, voice.FilePath)

	// Fast path — local cache hit.
	if data, err := os.ReadFile(absPath); err == nil {
		return base64.StdEncoding.EncodeToString(data), nil
	}

	// Local miss — fall back to the DB blob (cloned on another node, or the
	// local cache was cleared).
	if len(voice.EmbeddingData) == 0 {
		return "", fmt.Errorf("voice not found")
	}

	// Write-through so subsequent reads hit disk.
	if err := os.MkdirAll(filepath.Dir(absPath), 0755); err == nil {
		if err := os.WriteFile(absPath, voice.EmbeddingData, 0644); err != nil {
			slog.Warn("failed to write through voice embedding cache", "path", absPath, "error", err)
		}
	}

	return base64.StdEncoding.EncodeToString(voice.EmbeddingData), nil
}

// SyncVoiceStorage reconciles DB blobs and per-node disk files at startup.
// Safe to run on every node boot — all four directions are idempotent:
//
//   - Truncate: oversize embeddings (>125 frames, from voices cloned on
//     FluidAudio ≤ 0.13.6 which allowed 250 frames) are cut to the first
//     125 frames in both the DB blob and the disk file. 0.15.5's loader
//     hard-rejects these, and the dropped tail is only extra reference
//     audio. Runs first so the passes below propagate repaired bytes.
//   - Backfill (disk → DB): rows with an empty embedding_data blob but an
//     existing local file get the blob stored. Covers voices cloned before
//     the blob column existed and pre-existing files on this node.
//   - Forward-fill (DB → disk): rows with a blob but no local file get the
//     file materialized. Covers voices cloned on other nodes.
//   - Orphan sweep: local files whose voice row is gone (deleted on any
//     node — the soft-deleted row no longer matches active queries) are
//     removed. This is how deletions propagate to other nodes' caches.
func (h *VoiceHandler) SyncVoiceStorage() {
	var voices []models.Voice
	if result := h.db.Find(&voices); result.Error != nil {
		slog.Error("voice storage sync: failed to list voices", "error", result.Error)
		return
	}

	var truncated, backfilled, forwardFilled, failed int
	for _, voice := range voices {
		absPath := filepath.Join(h.voicesDir, voice.FilePath)
		diskData, diskErr := os.ReadFile(absPath)
		diskOK := diskErr == nil

		// Repair oversize legacy embeddings before anything else reads them.
		if trimmed, ok := truncateVoiceEmbedding(voice.EmbeddingData); ok {
			if err := h.db.Model(&models.Voice{}).Where("id = ?", voice.ID).
				Updates(map[string]interface{}{
					"embedding_data": trimmed,
					"size_bytes":     int64(len(trimmed)),
				}).Error; err != nil {
				slog.Warn("voice storage sync: truncation failed", "voice_id", voice.ID, "error", err)
				failed++
				continue
			}
			voice.EmbeddingData = trimmed
			if diskOK {
				if err := os.WriteFile(absPath, trimmed, 0644); err != nil {
					slog.Warn("voice storage sync: truncation disk rewrite failed", "voice_id", voice.ID, "path", absPath, "error", err)
				} else {
					diskData = trimmed
				}
			}
			truncated++
			slog.Info("voice storage sync: truncated oversize legacy voice embedding to 125 frames",
				"voice_id", voice.ID, "user_id", voice.UserID)
		} else if len(voice.EmbeddingData) > maxVoiceEmbeddingBytes {
			// Oversize but not a whole number of frames — corrupt, leave alone.
			slog.Warn("voice storage sync: embedding blob has invalid size, skipping",
				"voice_id", voice.ID, "size_bytes", len(voice.EmbeddingData))
		}

		// Blob empty but disk file oversize — repair the file so the
		// backfill below uploads the truncated bytes.
		if len(voice.EmbeddingData) == 0 && diskOK {
			if trimmed, ok := truncateVoiceEmbedding(diskData); ok {
				if err := os.WriteFile(absPath, trimmed, 0644); err != nil {
					slog.Warn("voice storage sync: disk truncation failed", "voice_id", voice.ID, "path", absPath, "error", err)
					failed++
					continue
				}
				diskData = trimmed
				truncated++
				slog.Info("voice storage sync: truncated oversize legacy voice file to 125 frames",
					"voice_id", voice.ID, "path", absPath)
			} else if len(diskData) > maxVoiceEmbeddingBytes {
				slog.Warn("voice storage sync: embedding file has invalid size, skipping",
					"voice_id", voice.ID, "path", absPath, "size_bytes", len(diskData))
			}
		}

		switch {
		case len(voice.EmbeddingData) == 0 && diskOK:
			// Backfill: disk file exists but DB blob missing.
			if err := h.db.Model(&models.Voice{}).Where("id = ?", voice.ID).
				Update("embedding_data", diskData).Error; err != nil {
				slog.Warn("voice storage sync: backfill failed", "voice_id", voice.ID, "error", err)
				failed++
				continue
			}
			backfilled++

		case len(voice.EmbeddingData) > 0 && !diskOK:
			// Forward-fill: DB blob exists but this node lacks the file.
			if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
				slog.Warn("voice storage sync: forward-fill mkdir failed", "voice_id", voice.ID, "error", err)
				failed++
				continue
			}
			if err := os.WriteFile(absPath, voice.EmbeddingData, 0644); err != nil {
				slog.Warn("voice storage sync: forward-fill write failed", "voice_id", voice.ID, "error", err)
				failed++
				continue
			}
			forwardFilled++
		}
	}

	orphansRemoved := h.sweepOrphanedVoiceFiles()

	if truncated > 0 || backfilled > 0 || forwardFilled > 0 || orphansRemoved > 0 || failed > 0 {
		slog.Info("voice storage sync completed",
			"total_voices", len(voices), "truncated", truncated,
			"backfilled", backfilled,
			"forward_filled", forwardFilled, "orphans_removed", orphansRemoved,
			"failed", failed)
	}
	h.lm.SendLog(h.lm.BuildLog("VOICE_STORAGE_SYNCED", "VoiceStorageSynced", slog.LevelInfo, map[string]interface{}{
		"endpoint":        "startup",
		"total_voices":    len(voices),
		"truncated":       truncated,
		"backfilled":      backfilled,
		"forward_filled":  forwardFilled,
		"orphans_removed": orphansRemoved,
		"failed":          failed,
	}))
}

// sweepOrphanedVoiceFiles removes embedding files under voicesDir whose
// voice row no longer exists (or was soft-deleted on any node). Filenames
// are {user_id}/{voice_id}.bin; anything unparseable is left alone.
func (h *VoiceHandler) sweepOrphanedVoiceFiles() int {
	userDirs, err := os.ReadDir(h.voicesDir)
	if err != nil {
		return 0 // no voices dir yet — nothing to sweep
	}

	removed := 0
	for _, userDir := range userDirs {
		if !userDir.IsDir() {
			continue
		}
		dir := filepath.Join(h.voicesDir, userDir.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || filepath.Ext(f.Name()) != ".bin" {
				continue
			}
			voiceID, err := uuid.Parse(strings.TrimSuffix(f.Name(), ".bin"))
			if err != nil {
				continue // not a voice embedding file — leave it
			}
			var count int64
			h.db.Model(&models.Voice{}).Where("id = ?", voiceID).Count(&count)
			if count > 0 {
				continue
			}
			path := filepath.Join(dir, f.Name())
			if err := os.Remove(path); err != nil {
				slog.Warn("voice storage sync: orphan removal failed", "path", path, "error", err)
				continue
			}
			slog.Info("voice storage sync: removed orphaned voice file (deleted on another node)", "path", path)
			removed++
		}
	}
	return removed
}
