package handlers

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"time"

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
	stream := c.FormValue("stream") == "true"

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

	// SSE streaming mode (OpenAI-compatible)
	if stream {
		return streamWhisperResponse(c, result)
	}

	// Format response based on response_format
	responseFormat := c.FormValue("response_format", "json")
	return formatWhisperResponse(c, result, responseFormat)
}

// sseEvent represents a single Server-Sent Event payload.
type sseEvent struct {
	Type     string         `json:"type"`
	Delta    string         `json:"delta,omitempty"`
	Text     string         `json:"text,omitempty"`
	Duration float64        `json:"duration,omitempty"`
	Words    []sidecar.Word `json:"words,omitempty"`
	Logprobs interface{}    `json:"logprobs"`
}

// streamWhisperResponse writes the transcript as OpenAI-compatible SSE events.
// Events emitted:
//   - transcript.text.delta  — one per segment, with incremental text
//   - transcript.text.done   — final event with full text
//   - data: [DONE]           — terminal sentinel
func streamWhisperResponse(c *fiber.Ctx, result *sidecar.TranscribeResponse) error {
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		// Emit delta events — one per segment for incremental delivery
		if len(result.Segments) > 0 {
			for _, seg := range result.Segments {
				delta := sseEvent{
					Type:     "transcript.text.delta",
					Delta:    seg.Text,
					Logprobs: nil,
				}
				writeSSE(w, "transcript.text.delta", delta)
				w.Flush()

				// Brief delay between segments to simulate incremental delivery
				time.Sleep(5 * time.Millisecond)
			}
		} else if result.Text != "" {
			// No segments — fall back to single delta with full text
			delta := sseEvent{
				Type:     "transcript.text.delta",
				Delta:    result.Text,
				Logprobs: nil,
			}
			writeSSE(w, "transcript.text.delta", delta)
			w.Flush()
		}

		// Emit done event with full transcript
		done := sseEvent{
			Type:     "transcript.text.done",
			Text:     result.Text,
			Duration: result.Duration,
			Words:    result.Words,
			Logprobs: nil,
		}
		writeSSE(w, "transcript.text.done", done)
		w.Flush()

		// Terminal sentinel (OpenAI convention)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		w.Flush()
	})

	return nil
}

// writeSSE writes a single SSE event to the writer.
func writeSSE(w *bufio.Writer, event string, data interface{}) {
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonBytes))
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
