package handlers

import (
	"log/slog"
	"net/url"
	"time"

	ws "github.com/fasthttp/websocket"
	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/shaunagostinho/gotranscribesrv/internal/logging"
	"github.com/shaunagostinho/gotranscribesrv/internal/metrics"
	"github.com/shaunagostinho/gotranscribesrv/internal/middleware"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
	"gorm.io/gorm"
)

// WSHandler handles WebSocket ASR streaming by proxying to the Python sidecar.
//
// NOTE: Session-end logs currently do NOT include transcript text
// (only audio bytes / duration / process time). If transcript text is
// added to SESSION_ENDED in the future, it MUST be run through the
// PII redactor before being added to BuildLog's AdditionalData — see
// internal/handlers/{asr,whisper,watson}.go for the pattern.
type WSHandler struct {
	sidecar    *sidecar.Client
	db         *gorm.DB
	defaultITN bool
	lm         *logging.LogManager
}

// NewWSHandler creates a new WSHandler.
func NewWSHandler(sc *sidecar.Client, db *gorm.DB, defaultITN bool, lm *logging.LogManager) *WSHandler {
	return &WSHandler{sidecar: sc, db: db, defaultITN: defaultITN, lm: lm}
}

// Upgrade returns the Fiber middleware that upgrades HTTP to WebSocket.
func (h *WSHandler) Upgrade() fiber.Handler {
	return websocket.New(h.handle)
}

// handle proxies WebSocket frames between the client and the Python sidecar.
func (h *WSHandler) handle(c *websocket.Conn) {
	defer c.Close()

	// Limit incoming message size to 1MB to prevent memory exhaustion
	c.SetReadLimit(1 * 1024 * 1024)

	// Per-session request id — used as the correlation id for every
	// log event in this WS session. WS slog calls below pass this
	// explicitly as the "request_id" attr.
	requestID := uuid.New().String()
	c.Locals(middleware.RequestIDLocalKey, requestID)

	// Parse sidecar WebSocket URL
	sidecarURL := h.sidecar.StreamURL()
	u, err := url.Parse(sidecarURL)
	if err != nil {
		slog.Error("invalid sidecar stream URL", "error", err)
		_ = c.WriteJSON(fiber.Map{"type": "error", "message": "internal configuration error"})
		return
	}

	// Add query params from client (language, diarize, itn, etc.)
	q := u.Query()
	if lang := c.Query("language"); lang != "" {
		q.Set("language", lang)
	}
	if diarize := c.Query("diarize"); diarize != "" {
		q.Set("diarize", diarize)
	}
	// ITN: forward the client's ?itn= override if present; otherwise inject
	// the server-wide default (cfg.EnableITN) so ENABLE_ITN=false in .env
	// actually disables ITN for WS clients that don't specify it.
	if itn := c.Query("itn"); itn != "" {
		q.Set("itn", itn)
	} else if !h.defaultITN {
		q.Set("itn", "false")
	}
	u.RawQuery = q.Encode()

	// Connect to sidecar WebSocket
	sidecarConn, _, err := ws.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		slog.Error("failed to connect to sidecar WebSocket", "error", err)
		h.lm.SendLog(h.lm.BuildLog("WS_ASR_CONNECT_FAILED", "WSASRConnectFailed", slog.LevelError, map[string]interface{}{
			"endpoint":   "/ws/asr",
			"request_id": requestID,
		}, err))
		_ = c.WriteJSON(fiber.Map{"type": "error", "message": "transcription service unavailable"})
		return
	}
	defer sidecarConn.Close()

	slog.Info("WebSocket ASR session started")
	metrics.ActiveWebSocketConnections.WithLabelValues("native").Inc()
	defer metrics.ActiveWebSocketConnections.WithLabelValues("native").Dec()

	h.lm.SendLog(h.lm.BuildLog("WS_ASR_SESSION_STARTED", "WSASRSessionStarted", slog.LevelInfo, map[string]interface{}{
		"endpoint":   "/ws/asr",
		"language":   c.Query("language", "en"),
		"diarize":    c.Query("diarize", "false") == "true",
		"itn":        itnEnabled(c, h.defaultITN),
		"request_id": requestID,
	}))

	var totalAudioBytes int
	var firstAudioAt time.Time
	var lastResultAt time.Time
	errCh := make(chan error, 2)

	// Client → Sidecar (audio frames)
	go func() {
		var frameCount int
		for {
			msgType, msg, err := c.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			if msgType == websocket.BinaryMessage {
				frameCount++
				totalAudioBytes += len(msg)
				if frameCount == 1 {
					firstAudioAt = time.Now()
				}
			}
			if err := sidecarConn.WriteMessage(msgType, msg); err != nil {
				errCh <- err
				return
			}
		}
	}()

	// Sidecar → Client (transcript frames)
	go func() {
		for {
			msgType, msg, err := sidecarConn.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			lastResultAt = time.Now()
			if err := c.WriteMessage(msgType, msg); err != nil {
				errCh <- err
				return
			}
		}
	}()

	// Wait for either direction to close
	<-errCh

	// Log usage — processing time = first audio frame → last sidecar response
	audioDurationMs := 0
	if totalAudioBytes > 0 {
		audioDurationMs = totalAudioBytes / 32 // PCM 16-bit 16kHz mono = 32 bytes/ms
	}
	processTimeMs := 0
	if !firstAudioAt.IsZero() && !lastResultAt.IsZero() {
		processTimeMs = int(lastResultAt.Sub(firstAudioAt).Milliseconds())
	}
	userID, _ := c.Locals("user_id").(string)
	apiKeyID, _ := c.Locals("api_key_id").(string)
	middleware.LogWebSocketUsage(h.db, userID, apiKeyID, "asr_stream",
		audioDurationMs, processTimeMs, false)

	slog.Info("WebSocket ASR session ended",
		"request_id", requestID,
		"audio_bytes", totalAudioBytes, "audio_duration_ms", audioDurationMs,
		"process_ms", processTimeMs)

	h.lm.SendLog(h.lm.BuildLog("WS_ASR_SESSION_ENDED", "WSASRSessionEnded", slog.LevelInfo, map[string]interface{}{
		"endpoint":          "/ws/asr",
		"audio_bytes":       totalAudioBytes,
		"audio_duration_ms": audioDurationMs,
		"process_ms":        processTimeMs,
		"realtime_x":        realtimeFactor(audioDurationMs, processTimeMs),
		"request_id":        requestID,
	}))
}
