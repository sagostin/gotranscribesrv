package handlers

import (
	"log/slog"
	"net/url"
	"time"

	ws "github.com/fasthttp/websocket"
	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/shaunagostinho/gotranscribesrv/internal/middleware"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
	"gorm.io/gorm"
)

// WSHandler handles WebSocket ASR streaming by proxying to the Python sidecar.
type WSHandler struct {
	sidecar *sidecar.Client
	db      *gorm.DB
}

// NewWSHandler creates a new WSHandler.
func NewWSHandler(sc *sidecar.Client, db *gorm.DB) *WSHandler {
	return &WSHandler{sidecar: sc, db: db}
}

// Upgrade returns the Fiber middleware that upgrades HTTP to WebSocket.
func (h *WSHandler) Upgrade() fiber.Handler {
	return websocket.New(h.handle)
}

// handle proxies WebSocket frames between the client and the Python sidecar.
func (h *WSHandler) handle(c *websocket.Conn) {
	defer c.Close()

	// Parse sidecar WebSocket URL
	sidecarURL := h.sidecar.StreamURL()
	u, err := url.Parse(sidecarURL)
	if err != nil {
		slog.Error("invalid sidecar stream URL", "error", err)
		_ = c.WriteJSON(fiber.Map{"type": "error", "message": "internal configuration error"})
		return
	}

	// Add query params from client (language, diarize, etc.)
	q := u.Query()
	if lang := c.Query("language"); lang != "" {
		q.Set("language", lang)
	}
	if diarize := c.Query("diarize"); diarize != "" {
		q.Set("diarize", diarize)
	}
	u.RawQuery = q.Encode()

	// Connect to sidecar WebSocket
	sidecarConn, _, err := ws.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		slog.Error("failed to connect to sidecar WebSocket", "error", err)
		_ = c.WriteJSON(fiber.Map{"type": "error", "message": "transcription service unavailable"})
		return
	}
	defer sidecarConn.Close()

	slog.Info("WebSocket ASR session started")

	sessionStart := time.Now()
	var totalAudioBytes int
	errCh := make(chan error, 2)

	// Client → Sidecar (audio frames)
	go func() {
		for {
			msgType, msg, err := c.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			if msgType == websocket.BinaryMessage {
				totalAudioBytes += len(msg)
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
			if err := c.WriteMessage(msgType, msg); err != nil {
				errCh <- err
				return
			}
		}
	}()

	// Wait for either direction to close
	<-errCh

	// Log usage for this streaming session
	sessionDuration := time.Since(sessionStart)
	audioDurationMs := 0
	if totalAudioBytes > 0 {
		audioDurationMs = totalAudioBytes / 32 // PCM 16-bit 16kHz mono = 32 bytes/ms
	}
	userID, _ := c.Locals("user_id").(string)
	apiKeyID, _ := c.Locals("api_key_id").(string)
	middleware.LogWebSocketUsage(h.db, userID, apiKeyID, "asr_stream",
		audioDurationMs, int(sessionDuration.Milliseconds()), false)

	slog.Info("WebSocket ASR session ended",
		"audio_bytes", totalAudioBytes, "audio_duration_ms", audioDurationMs,
		"session_duration_ms", sessionDuration.Milliseconds())
}
