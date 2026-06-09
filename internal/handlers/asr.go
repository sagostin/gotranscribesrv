package handlers

import (
	"io"
	"log/slog"

	"github.com/gofiber/fiber/v2"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
)

// ASRHandler handles speech-to-text routes.
type ASRHandler struct {
	sidecar    *sidecar.Client
	defaultITN bool
}

// NewASRHandler creates a new ASRHandler.
func NewASRHandler(sc *sidecar.Client, defaultITN bool) *ASRHandler {
	return &ASRHandler{sidecar: sc, defaultITN: defaultITN}
}

// TranscribeFile handles multipart audio file upload for transcription.
// POST /api/v1/asr
func (h *ASRHandler) TranscribeFile(c *fiber.Ctx) error {
	file, err := c.FormFile("audio")
	if err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "MISSING_AUDIO",
				"message": "Audio file is required (field: 'audio')",
				"status":  422,
			},
		})
	}

	// Check file size (100MB max)
	if file.Size > 100*1024*1024 {
		return c.Status(fiber.StatusRequestEntityTooLarge).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "FILE_TOO_LARGE",
				"message": "Audio file must be less than 100MB",
				"status":  413,
			},
		})
	}

	// Read file
	f, err := file.Open()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "FILE_READ_ERROR",
				"message": "Failed to read audio file",
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
				"message": "Failed to read audio file",
				"status":  500,
			},
		})
	}

	diarize := c.FormValue("diarize") == "true"
	language := c.FormValue("language", "en")

	itnVal := h.defaultITN
	if v := c.FormValue("itn"); v != "" {
		itnVal = v != "false"
	}

	result, err := h.sidecar.Transcribe(sidecar.TranscribeRequest{
		Audio:    audioBytes,
		Filename: file.Filename,
		Language: language,
		Diarize:  diarize,
		ITN:      &itnVal,
	})
	if err != nil {
		slog.Error("transcription failed", "error", err, "filename", file.Filename)
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "SIDECAR_ERROR",
				"message": "Transcription service unavailable",
				"status":  502,
			},
		})
	}

	// Store metadata for usage tracking middleware
	c.Locals("audio_duration_ms", int(result.Duration*1000))
	c.Locals("diarized", result.Diarized)
	c.Locals("usage_meta", map[string]interface{}{
		"file_size_bytes": file.Size,
		"filename":        file.Filename,
		"word_count":      len(result.Words),
		"segment_count":   len(result.Segments),
		"language":        language,
		"model":           result.Model,
	})

	return c.JSON(result)
}
