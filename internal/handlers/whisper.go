package handlers

import (
	"fmt"
	"io"

	"github.com/gofiber/fiber/v2"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
)

// WhisperHandler handles the OpenAI Whisper-compatible endpoint.
type WhisperHandler struct {
	sidecar *sidecar.Client
}

// NewWhisperHandler creates a new WhisperHandler.
func NewWhisperHandler(sc *sidecar.Client) *WhisperHandler {
	return &WhisperHandler{sidecar: sc}
}

// Transcriptions handles the Whisper-compatible endpoint.
// POST /v1/audio/transcriptions
func (h *WhisperHandler) Transcriptions(c *fiber.Ctx) error {
	file, err := c.FormFile("file")
	if err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "MISSING_FILE",
				"message": "Audio file is required (field: 'file')",
				"status":  422,
			},
		})
	}

	// 25MB limit per Whisper spec
	if file.Size > 25*1024*1024 {
		return c.Status(fiber.StatusRequestEntityTooLarge).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "FILE_TOO_LARGE",
				"message": "Audio file must be less than 25MB",
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

	language := c.FormValue("language", "en")

	// model field is accepted but ignored (always uses Parakeet TDT)
	// temperature is accepted but ignored
	// prompt is accepted for compatibility

	result, err := h.sidecar.Transcribe(sidecar.TranscribeRequest{
		Audio:    audioBytes,
		Filename: file.Filename,
		Language: language,
		Diarize:  false,
	})
	if err != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "SIDECAR_ERROR",
				"message": "Transcription service unavailable",
				"status":  502,
			},
		})
	}

	// Store metadata for usage tracking
	c.Locals("audio_duration_ms", int(result.Duration*1000))

	// Format response based on response_format
	responseFormat := c.FormValue("response_format", "json")
	return formatWhisperResponse(c, result, responseFormat)
}

// formatWhisperResponse formats the transcript in the requested Whisper format.
func formatWhisperResponse(c *fiber.Ctx, result *sidecar.TranscribeResponse, format string) error {
	switch format {
	case "text":
		c.Set("Content-Type", "text/plain")
		return c.SendString(result.Text)

	case "srt":
		c.Set("Content-Type", "text/plain")
		return c.SendString(toSRT(result))

	case "vtt":
		c.Set("Content-Type", "text/vtt")
		return c.SendString(toVTT(result))

	case "verbose_json":
		return c.JSON(fiber.Map{
			"task":     "transcribe",
			"language": "en",
			"duration": result.Duration,
			"text":     result.Text,
			"segments": result.Segments,
			"words":    result.Words,
		})

	default: // "json"
		return c.JSON(fiber.Map{
			"text": result.Text,
		})
	}
}

func toSRT(result *sidecar.TranscribeResponse) string {
	var srt string
	for i, seg := range result.Segments {
		srt += fmt.Sprintf("%d\n%s --> %s\n%s\n\n",
			i+1,
			formatTimeSRT(seg.Start),
			formatTimeSRT(seg.End),
			seg.Text,
		)
	}
	return srt
}

func toVTT(result *sidecar.TranscribeResponse) string {
	vtt := "WEBVTT\n\n"
	for _, seg := range result.Segments {
		vtt += fmt.Sprintf("%s --> %s\n%s\n\n",
			formatTimeVTT(seg.Start),
			formatTimeVTT(seg.End),
			seg.Text,
		)
	}
	return vtt
}

func formatTimeSRT(seconds float64) string {
	h := int(seconds) / 3600
	m := (int(seconds) % 3600) / 60
	s := int(seconds) % 60
	ms := int((seconds - float64(int(seconds))) * 1000)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}

func formatTimeVTT(seconds float64) string {
	h := int(seconds) / 3600
	m := (int(seconds) % 3600) / 60
	s := int(seconds) % 60
	ms := int((seconds - float64(int(seconds))) * 1000)
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
}
