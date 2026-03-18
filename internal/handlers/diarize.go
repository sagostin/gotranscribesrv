package handlers

import (
	"io"

	"github.com/gofiber/fiber/v2"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
)

// DiarizeHandler handles speaker detection routes.
type DiarizeHandler struct {
	sidecar *sidecar.Client
}

// NewDiarizeHandler creates a new DiarizeHandler.
func NewDiarizeHandler(sc *sidecar.Client) *DiarizeHandler {
	return &DiarizeHandler{sidecar: sc}
}

// DetectSpeakers performs standalone speaker diarization without ASR.
// POST /api/v1/diarize
func (h *DiarizeHandler) DetectSpeakers(c *fiber.Ctx) error {
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

	if file.Size > 100*1024*1024 {
		return c.Status(fiber.StatusRequestEntityTooLarge).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "FILE_TOO_LARGE",
				"message": "Audio file must be less than 100MB",
				"status":  413,
			},
		})
	}

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

	result, err := h.sidecar.Diarize(audioBytes, file.Filename)
	if err != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "SIDECAR_ERROR",
				"message": "Speaker detection service unavailable: " + err.Error(),
				"status":  502,
			},
		})
	}

	c.Locals("audio_duration_ms", int(result.Duration*1000))

	return c.JSON(result)
}
