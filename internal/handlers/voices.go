package handlers

import (
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/shaunagostinho/gotranscribesrv/internal/logging"
	"github.com/shaunagostinho/gotranscribesrv/internal/middleware"
	"github.com/shaunagostinho/gotranscribesrv/internal/models"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
	"gorm.io/gorm"
)

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

	// Check name uniqueness for this user
	var existing models.Voice
	if result := h.db.Where("user_id = ? AND name = ?", userID, name).First(&existing); result.Error == nil {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "VOICE_EXISTS",
				"message": fmt.Sprintf("Voice named %q already exists", name),
				"status":  409,
			},
		})
	}

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

	h.lm.SendLog(h.lm.BuildLog("VOICE_CLONE_STARTED", "VoiceCloneStarted", slog.LevelInfo, map[string]interface{}{
		"endpoint":   "/api/v1/voices/clone",
		"ip":         c.IP(),
		"user_id":    userID.String(),
		"name":       name,
		"file_size":  file.Size,
		"request_id": middleware.RequestIDFromCtx(c),
	}))

	// Send to sidecar to extract voice embedding
	cloneStart := time.Now()
	embedding, audioDurationMs, err := h.sidecar.CloneVoice(c.UserContext(), audioBytes, file.Filename)
	cloneDuration := time.Since(cloneStart)
	if err != nil {
		errMsg := err.Error()
		h.lm.SendLog(h.lm.BuildLog("VOICE_CLONE_FAILED", "VoiceCloneFailed", slog.LevelError, map[string]interface{}{
			"endpoint":      "/api/v1/voices/clone",
			"ip":            c.IP(),
			"user_id":       userID.String(),
			"name":          name,
			"file_size":     file.Size,
			"clone_time_ms": int(cloneDuration.Milliseconds()),
			"request_id":    middleware.RequestIDFromCtx(c),
		}, err))
		// If the sidecar returned a specific error (e.g. audio too long/short),
		// forward the actual message to the client
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "CLONE_FAILED",
				"message": errMsg,
				"status":  422,
			},
		})
	}

	// Create voice record
	voiceID := uuid.New()
	relPath := filepath.Join(userID.String(), voiceID.String()+".bin")
	absPath := filepath.Join(h.voicesDir, relPath)

	// Ensure user directory exists
	userDir := filepath.Join(h.voicesDir, userID.String())
	if err := os.MkdirAll(userDir, 0755); err != nil {
		h.lm.SendLog(h.lm.BuildLog("VOICE_CLONE_DIR_ERROR", "VoiceCloneDirError", slog.LevelError, map[string]interface{}{
			"endpoint":   "/api/v1/voices/clone",
			"ip":         c.IP(),
			"user_id":    userID.String(),
			"name":       name,
			"path":       userDir,
			"request_id": middleware.RequestIDFromCtx(c),
		}, err))
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "STORAGE_ERROR",
				"message": "Failed to create voice storage",
				"status":  500,
			},
		})
	}

	// Write embedding to disk
	if err := os.WriteFile(absPath, embedding, 0644); err != nil {
		h.lm.SendLog(h.lm.BuildLog("VOICE_CLONE_WRITE_ERROR", "VoiceCloneWriteError", slog.LevelError, map[string]interface{}{
			"endpoint":   "/api/v1/voices/clone",
			"ip":         c.IP(),
			"user_id":    userID.String(),
			"name":       name,
			"path":       absPath,
			"request_id": middleware.RequestIDFromCtx(c),
		}, err))
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "STORAGE_ERROR",
				"message": "Failed to store voice embedding",
				"status":  500,
			},
		})
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
			"endpoint":   "/api/v1/voices/clone",
			"ip":         c.IP(),
			"user_id":    userID.String(),
			"name":       name,
			"voice_id":   voiceID.String(),
			"request_id": middleware.RequestIDFromCtx(c),
		}, result.Error))
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "DB_ERROR",
				"message": "Failed to save voice record",
				"status":  500,
			},
		})
	}

	slog.InfoContext(c.UserContext(), "voice cloned successfully",
		"voice_id", voiceID, "user_id", userID, "name", name,
		"embedding_size", len(embedding), "clone_time_ms", cloneDuration.Milliseconds(),
		"audio_duration_ms", audioDurationMs)

	h.lm.SendLog(h.lm.BuildLog("VOICE_CLONE_COMPLETED", "VoiceCloneCompleted", slog.LevelInfo, map[string]interface{}{
		"endpoint":          "/api/v1/voices/clone",
		"ip":                c.IP(),
		"user_id":           userID.String(),
		"voice_id":          voiceID.String(),
		"name":              name,
		"embedding_bytes":   len(embedding),
		"clone_time_ms":     int(cloneDuration.Milliseconds()),
		"audio_duration_ms": audioDurationMs,
		"request_id":        middleware.RequestIDFromCtx(c),
	}))

	// Set audio_duration_ms for the usage middleware (actual audio duration)
	c.Locals("audio_duration_ms", audioDurationMs)

	c.Locals("usage_meta", map[string]interface{}{
		"voice_name":        name,
		"embedding_size":    len(embedding),
		"audio_size":        file.Size,
		"clone_time_ms":     int(cloneDuration.Milliseconds()),
		"audio_duration_ms": audioDurationMs,
	})

	return c.Status(fiber.StatusCreated).JSON(voice.ToResponse())
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

	var voice models.Voice
	if result := h.db.Where("id = ? AND user_id = ?", voiceID, userID).First(&voice); result.Error != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "NOT_FOUND",
				"message": "Voice not found",
				"status":  404,
			},
		})
	}

	// Remove the embedding file
	absPath := filepath.Join(h.voicesDir, voice.FilePath)
	if err := os.Remove(absPath); err != nil && !os.IsNotExist(err) {
		slog.Warn("failed to remove voice embedding file", "path", absPath, "error", err)
	}

	// Soft-delete the record
	if result := h.db.Delete(&voice); result.Error != nil {
		h.lm.SendLog(h.lm.BuildLog("VOICE_DELETE_DB_ERROR", "VoiceDeleteDBError", slog.LevelError, map[string]interface{}{
			"endpoint":   "/api/v1/voices/:id",
			"ip":         c.IP(),
			"user_id":    userID.String(),
			"voice_id":   voiceID.String(),
			"request_id": middleware.RequestIDFromCtx(c),
		}, result.Error))
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "DB_ERROR",
				"message": "Failed to delete voice",
				"status":  500,
			},
		})
	}

	slog.InfoContext(c.UserContext(), "voice deleted", "voice_id", voiceID, "user_id", userID, "name", voice.Name)

	h.lm.SendLog(h.lm.BuildLog("VOICE_DELETED", "VoiceDeleted", slog.LevelInfo, map[string]interface{}{
		"endpoint":   "/api/v1/voices/:id",
		"ip":         c.IP(),
		"user_id":    userID.String(),
		"voice_id":   voiceID.String(),
		"name":       voice.Name,
		"request_id": middleware.RequestIDFromCtx(c),
	}))

	return c.JSON(fiber.Map{
		"message": "Voice deleted",
	})
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
// Safe to run on every node boot — both directions are idempotent:
//
//   - Backfill (disk → DB): rows with an empty embedding_data blob but an
//     existing local file get the blob stored. Covers voices cloned before
//     the blob column existed and pre-existing files on this node.
//   - Forward-fill (DB → disk): rows with a blob but no local file get the
//     file materialized. Covers voices cloned on other nodes.
func (h *VoiceHandler) SyncVoiceStorage() {
	var voices []models.Voice
	if result := h.db.Find(&voices); result.Error != nil {
		slog.Error("voice storage sync: failed to list voices", "error", result.Error)
		return
	}

	var backfilled, forwardFilled, failed int
	for _, voice := range voices {
		absPath := filepath.Join(h.voicesDir, voice.FilePath)
		_, diskErr := os.ReadFile(absPath)
		diskOK := diskErr == nil

		switch {
		case len(voice.EmbeddingData) == 0 && diskOK:
			// Backfill: disk file exists but DB blob missing.
			data, _ := os.ReadFile(absPath)
			if err := h.db.Model(&models.Voice{}).Where("id = ?", voice.ID).
				Update("embedding_data", data).Error; err != nil {
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

	if backfilled > 0 || forwardFilled > 0 || failed > 0 {
		slog.Info("voice storage sync completed",
			"total_voices", len(voices), "backfilled", backfilled,
			"forward_filled", forwardFilled, "failed", failed)
	}
	h.lm.SendLog(h.lm.BuildLog("VOICE_STORAGE_SYNCED", "VoiceStorageSynced", slog.LevelInfo, map[string]interface{}{
		"endpoint":       "startup",
		"total_voices":   len(voices),
		"backfilled":     backfilled,
		"forward_filled": forwardFilled,
		"failed":         failed,
	}))
}
