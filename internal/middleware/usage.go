package middleware

import (
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/shaunagostinho/gotranscribesrv/internal/models"
	"gorm.io/gorm"
)

// UsageTracker is middleware that asynchronously logs API usage to PostgreSQL.
type UsageTracker struct {
	db    *gorm.DB
	logCh chan models.UsageLog
}

// NewUsageTracker creates a new usage tracking middleware with a buffered channel.
func NewUsageTracker(db *gorm.DB, bufferSize int) *UsageTracker {
	ut := &UsageTracker{
		db:    db,
		logCh: make(chan models.UsageLog, bufferSize),
	}
	go ut.processLogs()
	return ut
}

// Middleware returns the Fiber handler that tracks usage.
func (ut *UsageTracker) Middleware() fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()

		// Process request
		err := c.Next()

		// Only log for speech endpoints
		endpoint := classifyEndpoint(c.Path())
		if endpoint == "" {
			return err
		}

		userIDStr, _ := c.Locals("user_id").(string)
		userID, parseErr := uuid.Parse(userIDStr)
		if parseErr != nil {
			return err
		}

		// Extract audio duration from response headers (set by handlers)
		audioDuration := c.Locals("audio_duration_ms")
		audioDurationMs, _ := audioDuration.(int)

		processTime := int(time.Since(start).Milliseconds())
		diarized, _ := c.Locals("diarized").(bool)

		log := models.UsageLog{
			UserID:        userID,
			Endpoint:      endpoint,
			AudioDuration: audioDurationMs,
			ProcessTime:   processTime,
			Diarized:      diarized,
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
				ut.flush(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				ut.flush(batch)
				batch = batch[:0]
			}
		}
	}
}

func (ut *UsageTracker) flush(batch []models.UsageLog) {
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

func classifyEndpoint(path string) string {
	switch {
	case path == "/api/v1/asr" || path == "/v1/audio/transcriptions":
		return "asr"
	case path == "/ws/asr":
		return "asr_stream"
	case path == "/api/v1/tts":
		return "tts"
	case path == "/api/v1/diarize":
		return "diarize"
	default:
		return ""
	}
}
