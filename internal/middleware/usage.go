package middleware

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/shaunagostinho/gotranscribesrv/internal/models"
	"gorm.io/gorm"
)

// UsageTracker is middleware that asynchronously logs API usage to PostgreSQL.
// On success (2xx): writes a UsageLog with enriched metadata.
// On failure (4xx/5xx): writes a RequestLog with rough info + verbose slog output.
type UsageTracker struct {
	db     *gorm.DB
	logCh  chan models.UsageLog
	failCh chan models.RequestLog
}

// NewUsageTracker creates a new usage tracking middleware with buffered channels.
func NewUsageTracker(db *gorm.DB, bufferSize int) *UsageTracker {
	ut := &UsageTracker{
		db:     db,
		logCh:  make(chan models.UsageLog, bufferSize),
		failCh: make(chan models.RequestLog, bufferSize),
	}
	go ut.processLogs()
	go ut.processFailures()
	return ut
}

// Middleware returns the Fiber handler that tracks usage.
func (ut *UsageTracker) Middleware() fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()

		// Process request
		err := c.Next()

		// Only log for speech/API endpoints
		endpoint := classifyEndpoint(c.Path())
		if endpoint == "" {
			return err
		}

		// Extract user identity (may be nil for unauthenticated failures)
		userIDStr, _ := c.Locals("user_id").(string)
		userID, parseErr := uuid.Parse(userIDStr)

		// Extract API key ID if request was authenticated via API key
		var apiKeyID *uuid.UUID
		if akStr, ok := c.Locals("api_key_id").(string); ok {
			if parsed, parseErr := uuid.Parse(akStr); parseErr == nil {
				apiKeyID = &parsed
			}
		}

		status := c.Response().StatusCode()
		processTime := int(time.Since(start).Milliseconds())

		// ── Failed requests (4xx/5xx) ────────────────────────────────
		if status >= 400 {
			// Extract error code from response body if it's JSON
			errorCode := extractErrorCode(c.Response().Body())

			// Verbose structured log (console / Loki-ready)
			logArgs := []any{
				"endpoint", endpoint,
				"status", status,
				"error_code", errorCode,
				"method", c.Method(),
				"path", c.Path(),
				"ip", c.IP(),
				"user_agent", c.Get("User-Agent"),
				"content_type", c.Get("Content-Type"),
				"process_ms", processTime,
			}
			if parseErr == nil {
				logArgs = append(logArgs, "user_id", userID.String())
			}
			if apiKeyID != nil {
				logArgs = append(logArgs, "api_key_id", apiKeyID.String())
			}

			if status >= 500 {
				slog.Error("request failed", logArgs...)
			} else {
				slog.Warn("request failed", logArgs...)
			}

			// Rough info to DB (only if user is authenticated)
			if parseErr == nil {
				reqLog := models.RequestLog{
					UserID:    &userID,
					APIKeyID:  apiKeyID,
					Endpoint:  endpoint,
					Method:    c.Method(),
					Status:    status,
					ErrorCode: errorCode,
					IP:        c.IP(),
				}

				select {
				case ut.failCh <- reqLog:
				default:
					slog.Warn("request log channel full, dropping entry")
				}
			}

			return err
		}

		// ── Successful requests (2xx) ────────────────────────────────
		if parseErr != nil {
			return err
		}

		// Extract audio duration from locals (set by handlers)
		audioDuration := c.Locals("audio_duration_ms")
		audioDurationMs, _ := audioDuration.(int)

		diarized, _ := c.Locals("diarized").(bool)

		// Build metadata JSONB from handler-provided usage_meta
		metadata := "{}"
		if meta, ok := c.Locals("usage_meta").(map[string]interface{}); ok && len(meta) > 0 {
			if jsonBytes, err := json.Marshal(meta); err == nil {
				metadata = string(jsonBytes)
			}
		}

		log := models.UsageLog{
			UserID:        userID,
			APIKeyID:      apiKeyID,
			Endpoint:      endpoint,
			AudioDuration: audioDurationMs,
			ProcessTime:   processTime,
			Diarized:      diarized,
			Metadata:      metadata,
		}

		// Non-blocking send to channel
		select {
		case ut.logCh <- log:
		default:
			slog.Warn("usage log channel full, dropping entry")
		}

		return err
	}
}

// processLogs batches and writes usage logs to the database.
func (ut *UsageTracker) processLogs() {
	batch := make([]models.UsageLog, 0, 50)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case log := <-ut.logCh:
			batch = append(batch, log)
			if len(batch) >= 50 {
				ut.flushUsage(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				ut.flushUsage(batch)
				batch = batch[:0]
			}
		}
	}
}

// processFailures batches and writes failed request logs to the database.
func (ut *UsageTracker) processFailures() {
	batch := make([]models.RequestLog, 0, 50)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case log := <-ut.failCh:
			batch = append(batch, log)
			if len(batch) >= 50 {
				ut.flushFailures(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				ut.flushFailures(batch)
				batch = batch[:0]
			}
		}
	}
}

func (ut *UsageTracker) flushUsage(batch []models.UsageLog) {
	if len(batch) == 0 {
		return
	}
	result := ut.db.Create(&batch)
	if result.Error != nil {
		slog.Error("failed to flush usage logs", "error", result.Error, "count", len(batch))
	} else {
		slog.Debug("flushed usage logs", "count", len(batch))
	}
}

func (ut *UsageTracker) flushFailures(batch []models.RequestLog) {
	if len(batch) == 0 {
		return
	}
	result := ut.db.Create(&batch)
	if result.Error != nil {
		slog.Error("failed to flush request logs", "error", result.Error, "count", len(batch))
	} else {
		slog.Debug("flushed request logs", "count", len(batch))
	}
}

// extractErrorCode attempts to pull the error code from a JSON error response body.
func extractErrorCode(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &resp) == nil && resp.Error.Code != "" {
		return resp.Error.Code
	}
	return ""
}

func classifyEndpoint(path string) string {
	switch {
	case path == "/api/v1/asr" || path == "/v1/audio/transcriptions":
		return "asr"
	case path == "/ws/asr":
		return "asr_stream"
	case path == "/v1/recognize":
		return "asr_watson"
	case path == "/v1/listen":
		return "asr_deepgram"
	case path == "/api/v1/tts":
		return "tts"
	case path == "/api/v1/process":
		return "llm_process"
	default:
		return ""
	}
}
