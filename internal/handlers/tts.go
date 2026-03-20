package handlers

import (
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/shaunagostinho/gotranscribesrv/internal/middleware"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
)

// TTSHandler handles text-to-speech routes.
type TTSHandler struct {
	sidecar      *sidecar.Client
	voiceHandler *VoiceHandler // For loading stored voice embeddings
}

// NewTTSHandler creates a new TTSHandler.
func NewTTSHandler(sc *sidecar.Client, voiceHandler *VoiceHandler) *TTSHandler {
	return &TTSHandler{sidecar: sc, voiceHandler: voiceHandler}
}

// SynthesizeBody is the JSON request body for TTS synthesis.
type SynthesizeBody struct {
	Text     string  `json:"text"`
	Voice    string  `json:"voice"`     // System voice name (e.g. "jane", "charles")
	VoiceID  string  `json:"voice_id"`  // UUID of a stored custom voice
	VoiceRef string  `json:"voice_ref"` // Base64 raw audio for one-shot cloning
	Speed    float64 `json:"speed"`
	Format   string  `json:"format"`
}

// Synthesize converts text to speech using PocketTTS.
// POST /api/v1/tts
func (h *TTSHandler) Synthesize(c *fiber.Ctx) error {
	var body SynthesizeBody
	if err := c.BodyParser(&body); err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "INVALID_INPUT",
				"message": "Invalid request body",
				"status":  422,
			},
		})
	}

	if body.Text == "" {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "MISSING_TEXT",
				"message": "Text field is required",
				"status":  422,
			},
		})
	}

	if len(body.Text) > 5000 {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "TEXT_TOO_LONG",
				"message": "Text must be 5,000 characters or less",
				"status":  422,
			},
		})
	}

	// Build sidecar request
	req := sidecar.SynthesizeRequest{
		Text:     body.Text,
		Voice:    body.Voice,
		VoiceRef: body.VoiceRef,
		Speed:    body.Speed,
		Format:   body.Format,
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

	// If voice_id is provided, load the stored embedding
	if body.VoiceID != "" {
		voiceUUID, err := uuid.Parse(body.VoiceID)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": fiber.Map{
					"code":    "INVALID_VOICE_ID",
					"message": "Invalid voice_id format",
					"status":  400,
				},
			})
		}

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

		voiceData, err := h.voiceHandler.LoadVoiceData(voiceUUID, userID)
		if err != nil {
			slog.Warn("failed to load stored voice", "voice_id", body.VoiceID, "error", err)
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"error": fiber.Map{
					"code":    "VOICE_NOT_FOUND",
					"message": "Stored voice not found",
					"status":  404,
				},
			})
		}

		req.VoiceData = voiceData
		// Clear voice_ref — stored embedding takes priority
		req.VoiceRef = ""
	}

	synthStart := time.Now()
	audio, contentType, err := h.sidecar.Synthesize(req)
	synthDuration := time.Since(synthStart)
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

	// Calculate output audio duration (WAV 24kHz 16-bit mono = 48000 bytes/sec)
	// Subtract 44-byte WAV header
	outputDurationMs := 0
	if len(audio) > 44 {
		outputDurationMs = (len(audio) - 44) * 1000 / 48000
	}

	// Set audio_duration_ms for the usage middleware
	c.Locals("audio_duration_ms", outputDurationMs)

	c.Locals("usage_meta", map[string]interface{}{
		"text_length":        len(body.Text),
		"voice":              req.Voice,
		"voice_id":           body.VoiceID,
		"format":             req.Format,
		"output_bytes":       len(audio),
		"output_duration_ms": outputDurationMs,
		"synth_time_ms":      int(synthDuration.Milliseconds()),
	})

	c.Set("Content-Type", contentType)
	c.Set("X-Audio-Sample-Rate", "24000")
	return c.Send(audio)
}
