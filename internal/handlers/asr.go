package handlers

import (
	"io"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/shaunagostinho/gotranscribesrv/internal/logging"
	"github.com/shaunagostinho/gotranscribesrv/internal/middleware"
	"github.com/shaunagostinho/gotranscribesrv/internal/pii"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
)

// ASRHandler handles speech-to-text routes.
type ASRHandler struct {
	sidecar    *sidecar.Client
	redactor   *pii.Redactor
	defaultITN bool
	lm         *logging.LogManager
}

// NewASRHandler creates a new ASRHandler.
func NewASRHandler(sc *sidecar.Client, redactor *pii.Redactor, defaultITN bool, lm *logging.LogManager) *ASRHandler {
	return &ASRHandler{sidecar: sc, redactor: redactor, defaultITN: defaultITN, lm: lm}
}

// TranscribeFile handles multipart audio file upload for transcription.
// POST /api/v1/asr
func (h *ASRHandler) TranscribeFile(c *fiber.Ctx) error {
	file, err := c.FormFile("audio")
	if err != nil {
		h.lm.SendLog(h.lm.BuildLog("ASR_MISSING_AUDIO", "ASRMissingAudio", slog.LevelWarn, map[string]interface{}{
			"endpoint":   "/api/v1/asr",
			"ip":         c.IP(),
			"user_agent": c.Get("User-Agent"),
			"request_id": middleware.RequestIDFromCtx(c),
		}))
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
		h.lm.SendLog(h.lm.BuildLog("ASR_FILE_TOO_LARGE", "ASRFileTooLarge", slog.LevelWarn, map[string]interface{}{
			"endpoint":      "/api/v1/asr",
			"ip":            c.IP(),
			"file_size":     file.Size,
			"filename":      file.Filename,
			"size_limit_mb": 100,
			"request_id":    middleware.RequestIDFromCtx(c),
		}))
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
		h.lm.SendLog(h.lm.BuildLog("ASR_FILE_READ_ERROR", "ASRFileReadError", slog.LevelError, map[string]interface{}{
			"endpoint":   "/api/v1/asr",
			"ip":         c.IP(),
			"filename":   file.Filename,
			"request_id": middleware.RequestIDFromCtx(c),
		}, err))
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
		h.lm.SendLog(h.lm.BuildLog("ASR_FILE_READ_ERROR", "ASRFileReadError", slog.LevelError, map[string]interface{}{
			"endpoint":   "/api/v1/asr",
			"ip":         c.IP(),
			"filename":   file.Filename,
			"request_id": middleware.RequestIDFromCtx(c),
		}, err))
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

	// ITN: respect the per-request "itn" form field if present, otherwise
	// fall back to the server-wide default (cfg.EnableITN).
	itnVal := h.defaultITN
	if v := c.FormValue("itn"); v != "" {
		itnVal = v != "false"
	}

	// Emit "received" event up front so the request flow is visible
	// in Loki even when the sidecar call hangs or times out.
	h.lm.SendLog(h.lm.BuildLog("ASR_REQUEST_RECEIVED", "ASRRequestReceived", slog.LevelInfo, map[string]interface{}{
		"endpoint":   "/api/v1/asr",
		"filename":   file.Filename,
		"file_size":  file.Size,
		"language":   language,
		"diarize":    diarize,
		"itn":        itnVal,
		"ip":         c.IP(),
		"request_id": middleware.RequestIDFromCtx(c),
	}))

	start := time.Now()
	result, err := h.sidecar.Transcribe(c.UserContext(), sidecar.TranscribeRequest{
		Audio:    audioBytes,
		Filename: file.Filename,
		Language: language,
		Diarize:  diarize,
		ITN:      &itnVal,
	})
	sidecarMs := int(time.Since(start).Milliseconds())
	if err != nil {
		slog.ErrorContext(c.UserContext(), "transcription failed", "error", err, "filename", file.Filename)
		h.lm.SendLog(h.lm.BuildLog("ASR_FAILED", "ASRFailed", slog.LevelError, map[string]interface{}{
			"endpoint":   "/api/v1/asr",
			"ip":         c.IP(),
			"filename":   file.Filename,
			"file_size":  file.Size,
			"sidecar_ms": sidecarMs,
			"request_id": middleware.RequestIDFromCtx(c),
		}, err))
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "SIDECAR_ERROR",
				"message": "Transcription service unavailable",
				"status":  502,
			},
		})
	}

	// Full transcript + audio meta + sidecar timings ship to Loki.
	// This is the highest-value event for debugging — you can grep
	// transcripts in Grafana by content/filename/duration/etc.
	//
	// The transcript text is run through the PII redactor before
	// being placed into AdditionalData. The redactor is fail-closed:
	// on analyzer error the field becomes "<REDACTED-ERROR>".
	redactedText, piiItems, piiErr := h.redactor.RedactText(c.UserContext(), result.Text)
	if piiErr != nil {
		h.lm.SendLog(h.lm.BuildLog("PII_REDACTOR_ERROR", "PIIRedactorError", slog.LevelWarn, map[string]interface{}{
			"endpoint":   "/api/v1/asr",
			"ip":         c.IP(),
			"text_len":   len(result.Text),
			"request_id": middleware.RequestIDFromCtx(c),
		}, piiErr))
	}
	completedFields := map[string]interface{}{
		"endpoint":       "/api/v1/asr",
		"ip":             c.IP(),
		"filename":       file.Filename,
		"file_size":      file.Size,
		"audio_ms":       int(result.Duration * 1000),
		"audio_duration": result.Duration,
		"sidecar_ms":     sidecarMs,
		"asr_ms":         result.ProcessTimeMs,
		"model":          result.Model,
		"language":       language,
		"diarized":       result.Diarized,
		"num_speakers":   result.NumSpeakers,
		"itn_applied":    result.ITNApplied,
		"word_count":     len(result.Words),
		"segment_count":  len(result.Segments),
		"transcript":     redactedText,
		"pii_redacted":   len(piiItems),
		"request_id":     middleware.RequestIDFromCtx(c),
	}
	if len(piiItems) > 0 {
		completedFields["pii_entity_types"] = piiEntityTypes(piiItems)
	}
	if result.NumSpeakers > 0 {
		completedFields["speakers"] = result.Speakers
	}
	h.lm.SendLog(h.lm.BuildLog("ASR_COMPLETED", "ASRCompleted", slog.LevelInfo, completedFields))

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
