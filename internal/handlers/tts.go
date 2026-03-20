package handlers

import (
	"log/slog"

	"github.com/gofiber/fiber/v2"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
)

// TTSHandler handles text-to-speech routes.
type TTSHandler struct {
	sidecar *sidecar.Client
}

// NewTTSHandler creates a new TTSHandler.
func NewTTSHandler(sc *sidecar.Client) *TTSHandler {
	return &TTSHandler{sidecar: sc}
}

// Synthesize converts text to speech using LuxTTS.
// POST /api/v1/tts
func (h *TTSHandler) Synthesize(c *fiber.Ctx) error {
	var req sidecar.SynthesizeRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "INVALID_INPUT",
				"message": "Invalid request body",
				"status":  422,
			},
		})
	}

	if req.Text == "" {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "MISSING_TEXT",
				"message": "Text field is required",
				"status":  422,
			},
		})
	}

	if len(req.Text) > 5000 {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "TEXT_TOO_LONG",
				"message": "Text must be 5,000 characters or less",
				"status":  422,
			},
		})
	}

	// Defaults
	if req.Voice == "" {
		req.Voice = "default"
	}
	if req.Speed == 0 {
		req.Speed = 1.0
	}
	if req.Format == "" {
		req.Format = "wav"
	}

	audio, contentType, err := h.sidecar.Synthesize(req)
	if err != nil {
		slog.Error("TTS synthesis failed", "error", err)
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "SIDECAR_ERROR",
				"message": "TTS service unavailable",
				"status":  502,
			},
		})
	}

	c.Locals("usage_meta", map[string]interface{}{
		"text_length":  len(req.Text),
		"voice":        req.Voice,
		"format":       req.Format,
		"output_bytes": len(audio),
	})

	c.Set("Content-Type", contentType)
	c.Set("X-Audio-Sample-Rate", "48000")
	return c.Send(audio)
}

// ListVoices returns available TTS voice presets from the sidecar.
// GET /api/v1/voices
func (h *TTSHandler) ListVoices(c *fiber.Ctx) error {
	voices, err := h.sidecar.ListVoices()
	if err != nil {
		slog.Error("TTS voices list failed", "error", err)
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "SIDECAR_ERROR",
				"message": "TTS service unavailable",
				"status":  502,
			},
		})
	}

	return c.JSON(voices)
}
