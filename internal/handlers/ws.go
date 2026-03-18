package handlers

import (
	"log/slog"
	"net/url"

	ws "github.com/fasthttp/websocket"
	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
)

// WSHandler handles WebSocket ASR streaming by proxying to the Python sidecar.
type WSHandler struct {
	sidecar *sidecar.Client
}

// NewWSHandler creates a new WSHandler.
func NewWSHandler(sc *sidecar.Client) *WSHandler {
	return &WSHandler{sidecar: sc}
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

	errCh := make(chan error, 2)

	// Client → Sidecar (audio frames)
	go func() {
		for {
			msgType, msg, err := c.ReadMessage()
			if err != nil {
				errCh <- err
				return
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
	slog.Info("WebSocket ASR session ended")
}
