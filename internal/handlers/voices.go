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
		"endpoint":  "/api/v1/voices/clone",
		"user_id":   userID.String(),
		"name":      name,
		"file_size": file.Size,
	}))

	// Send to sidecar to extract voice embedding
	cloneStart := time.Now()
	embedding, audioDurationMs, err := h.sidecar.CloneVoice(audioBytes, file.Filename)
	cloneDuration := time.Since(cloneStart)
	if err != nil {
		slog.Error("voice cloning failed", "error", err, "user_id", userID)
		errMsg := err.Error()
		h.lm.SendLog(h.lm.BuildLog("VOICE_CLONE_FAILED", "VoiceCloneFailed", slog.LevelError, map[string]interface{}{
			"endpoint":      "/api/v1/voices/clone",
			"user_id":       userID.String(),
			"name":          name,
			"file_size":     file.Size,
			"clone_time_ms": int(cloneDuration.Milliseconds()),
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
		slog.Error("failed to create user voice directory", "path", userDir, "error", err)
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
		slog.Error("failed to write voice embedding", "path", absPath, "error", err)
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
		ID:          voiceID,
		UserID:      userID,
		Name:        name,
		Description: description,
		FilePath:    relPath,
		SizeBytes:   int64(len(embedding)),
		DurationSec: audioDurationSec,
	}

	if result := h.db.Create(&voice); result.Error != nil {
		// Clean up the file if DB insert fails
		_ = os.Remove(absPath)
		slog.Error("failed to create voice record", "error", result.Error)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "DB_ERROR",
				"message": "Failed to save voice record",
				"status":  500,
			},
		})
	}

	slog.Info("voice cloned successfully",
		"voice_id", voiceID, "user_id", userID, "name", name,
		"embedding_size", len(embedding), "clone_time_ms", cloneDuration.Milliseconds(),
		"audio_duration_ms", audioDurationMs)

	h.lm.SendLog(h.lm.BuildLog("VOICE_CLONE_COMPLETED", "VoiceCloneCompleted", slog.LevelInfo, map[string]interface{}{
		"endpoint":          "/api/v1/voices/clone",
		"user_id":           userID.String(),
		"voice_id":          voiceID.String(),
		"name":              name,
		"embedding_bytes":   len(embedding),
		"clone_time_ms":     int(cloneDuration.Milliseconds()),
		"audio_duration_ms": audioDurationMs,
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
		slog.Error("failed to query voices", "error", result.Error)
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
		slog.Error("failed to delete voice record", "error", result.Error)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "DB_ERROR",
				"message": "Failed to delete voice",
				"status":  500,
			},
		})
	}

	slog.Info("voice deleted", "voice_id", voiceID, "user_id", userID, "name", voice.Name)

	return c.JSON(fiber.Map{
		"message": "Voice deleted",
	})
}

// LoadVoiceData reads the stored embedding for a voice and returns base64.
// Used by TTSHandler when synthesizing with a stored voice_id.
func (h *VoiceHandler) LoadVoiceData(voiceID, userID uuid.UUID) (string, error) {
	var voice models.Voice
	if result := h.db.Where("id = ? AND user_id = ?", voiceID, userID).First(&voice); result.Error != nil {
		return "", fmt.Errorf("voice not found")
	}

	absPath := filepath.Join(h.voicesDir, voice.FilePath)
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("read embedding file: %w", err)
	}

	return base64.StdEncoding.EncodeToString(data), nil
}
