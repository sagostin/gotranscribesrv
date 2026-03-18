package handlers

import (
	"encoding/json"
	"log/slog"
	"net/url"
	"time"

	ws "github.com/fasthttp/websocket"
	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
)

// DeepgramHandler handles the Deepgram-compatible streaming endpoint.
// WS /v1/listen — proxies to the Swift sidecar's /stream WebSocket,
// translating between Deepgram's protocol and the internal protocol.
type DeepgramHandler struct {
	sidecar *sidecar.Client
}

// NewDeepgramHandler creates a new DeepgramHandler.
func NewDeepgramHandler(sc *sidecar.Client) *DeepgramHandler {
	return &DeepgramHandler{sidecar: sc}
}

// Upgrade returns the Fiber middleware that upgrades HTTP to WebSocket.
func (h *DeepgramHandler) Upgrade() fiber.Handler {
	return websocket.New(h.handle)
}

// --- Deepgram Response Types ---

type dgResults struct {
	Type         string      `json:"type"`
	ChannelIndex []int       `json:"channel_index"`
	Duration     float64     `json:"duration"`
	Start        float64     `json:"start"`
	IsFinal      bool        `json:"is_final"`
	SpeechFinal  bool        `json:"speech_final"`
	Channel      dgChannel   `json:"channel"`
	Metadata     dgModelMeta `json:"metadata"`
	FromFinalize bool        `json:"from_finalize"`
}

type dgChannel struct {
	Alternatives []dgAlternative `json:"alternatives"`
}

type dgAlternative struct {
	Transcript string   `json:"transcript"`
	Confidence float64  `json:"confidence"`
	Words      []dgWord `json:"words"`
}

type dgWord struct {
	Word           string  `json:"word"`
	Start          float64 `json:"start"`
	End            float64 `json:"end"`
	Confidence     float64 `json:"confidence"`
	PunctuatedWord string  `json:"punctuated_word"`
}

type dgMetadata struct {
	Type      string            `json:"type"`
	RequestID string            `json:"request_id"`
	Created   string            `json:"created"`
	Duration  float64           `json:"duration"`
	Channels  int               `json:"channels"`
	ModelInfo map[string]string `json:"model_info"`
}

type dgModelMeta struct {
	RequestID string            `json:"request_id"`
	ModelInfo map[string]string `json:"model_info"`
	ModelUUID string            `json:"model_uuid"`
}

// sidecarStreamEvent represents a JSON event from the Swift sidecar /stream WebSocket.
type sidecarStreamEvent struct {
	Type    string         `json:"type"`
	Text    string         `json:"text,omitempty"`
	Start   float64        `json:"start,omitempty"`
	End     float64        `json:"end,omitempty"`
	Words   []sidecar.Word `json:"words,omitempty"`
	IsFinal bool           `json:"is_final,omitempty"`
	Message string         `json:"message,omitempty"`
}

// handle proxies WebSocket frames between the Deepgram-compatible client and
// the Swift sidecar's /stream endpoint, translating the event protocol.
func (h *DeepgramHandler) handle(c *websocket.Conn) {
	defer c.Close()

	requestID := uuid.New().String()
	interimResults := c.Query("interim_results", "true") == "true"

	modelMeta := dgModelMeta{
		RequestID: requestID,
		ModelInfo: map[string]string{
			"name":    "parakeet-tdt-v3-coreml",
			"version": "2026-03-01",
			"arch":    "parakeet-tdt",
		},
		ModelUUID: uuid.New().String(),
	}

	// Send Deepgram Metadata event on connection open
	meta := dgMetadata{
		Type:      "Metadata",
		RequestID: requestID,
		Created:   time.Now().UTC().Format(time.RFC3339),
		Duration:  0,
		Channels:  1,
		ModelInfo: modelMeta.ModelInfo,
	}
	if err := c.WriteJSON(meta); err != nil {
		slog.Error("failed to send Metadata event", "error", err)
		return
	}

	// Connect to Swift sidecar /stream WebSocket
	sidecarURL := h.sidecar.StreamURL()
	u, err := url.Parse(sidecarURL)
	if err != nil {
		slog.Error("invalid sidecar stream URL", "error", err)
		_ = c.WriteJSON(fiber.Map{"type": "Error", "message": "internal configuration error"})
		return
	}

	// Forward query params to sidecar
	q := u.Query()
	for _, param := range []string{"language", "diarize", "encoding", "sample_rate"} {
		if v := c.Query(param); v != "" {
			q.Set(param, v)
		}
	}
	u.RawQuery = q.Encode()

	slog.Info("[DG] Query params", "request_id", requestID,
		"encoding", c.Query("encoding", "linear16"),
		"sample_rate", c.Query("sample_rate", "16000"),
		"language", c.Query("language", "en"))

	sidecarConn, _, err := ws.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		slog.Error("failed to connect to sidecar /stream WebSocket", "error", err)
		_ = c.WriteJSON(fiber.Map{"type": "Error", "message": "transcription service unavailable"})
		return
	}
	slog.Info("[DG] Connected to sidecar /stream", "request_id", requestID, "url", u.String())
	defer sidecarConn.Close()

	slog.Info("[DG] Deepgram-compat session started", "request_id", requestID,
		"interim_results", interimResults)

	errCh := make(chan error, 2)

	// Client → Sidecar: forward binary audio and translate control messages
	go func() {
		var frameCount int
		var totalBytes int
		for {
			msgType, msg, err := c.ReadMessage()
			if err != nil {
				slog.Info("[DG] Client read error (connection closed?)", "error", err, "request_id", requestID)
				errCh <- err
				return
			}

			// Text frames may be Deepgram control messages
			if msgType == websocket.TextMessage {
				slog.Info("[DG] Received text from client", "text", string(msg), "request_id", requestID)
				var ctrl map[string]interface{}
				if json.Unmarshal(msg, &ctrl) == nil {
					if ctrlType, ok := ctrl["type"].(string); ok {
						switch ctrlType {
						case "KeepAlive":
							slog.Info("[DG] KeepAlive from client", "request_id", requestID)
							continue
						case "CloseStream":
							slog.Info("[DG] CloseStream from client, forwarding to sidecar", "request_id", requestID, "total_frames", frameCount, "total_bytes", totalBytes)
							_ = sidecarConn.WriteMessage(ws.TextMessage,
								[]byte(`{"type":"CloseStream"}`))
							continue
						}
					}
				}
				// Forward other text messages
				if err := sidecarConn.WriteMessage(msgType, msg); err != nil {
					errCh <- err
					return
				}
				continue
			}

			// Binary audio frame
			frameCount++
			totalBytes += len(msg)
			if frameCount%50 == 1 {
				slog.Info("[DG] Forwarding audio to sidecar", "frame", frameCount, "bytes", len(msg), "total_bytes", totalBytes, "request_id", requestID)
			}

			if err := sidecarConn.WriteMessage(msgType, msg); err != nil {
				slog.Error("[DG] Failed to forward audio to sidecar", "error", err, "request_id", requestID)
				errCh <- err
				return
			}
		}
	}()

	// Sidecar → Client: translate internal events to Deepgram format
	go func() {
		for {
			_, msg, err := sidecarConn.ReadMessage()
			if err != nil {
				slog.Info("[DG] Sidecar read error (connection closed?)", "error", err, "request_id", requestID)
				errCh <- err
				return
			}

			slog.Info("[DG] Received from sidecar", "msg", string(msg[:min(len(msg), 500)]), "request_id", requestID)

			var evt sidecarStreamEvent
			if json.Unmarshal(msg, &evt) != nil {
				slog.Warn("[DG] Non-JSON from sidecar, forwarding raw", "request_id", requestID)
				if writeErr := c.WriteMessage(websocket.TextMessage, msg); writeErr != nil {
					errCh <- writeErr
					return
				}
				continue
			}

			switch evt.Type {
			case "partial":
				slog.Info("[DG] Sidecar partial result", "text", evt.Text[:min(len(evt.Text), 200)], "request_id", requestID)
				if !interimResults {
					slog.Info("[DG] Skipping partial (interim_results=false)", "request_id", requestID)
					continue
				}
				dgEvt := buildDGResults(evt, false, false, modelMeta)
				if err := c.WriteJSON(dgEvt); err != nil {
					errCh <- err
					return
				}
				slog.Info("[DG] Sent Deepgram partial Results to client", "request_id", requestID)

			case "final":
				slog.Info("[DG] Sidecar final result", "text", evt.Text[:min(len(evt.Text), 200)], "request_id", requestID)
				dgEvt := buildDGResults(evt, true, true, modelMeta)
				if err := c.WriteJSON(dgEvt); err != nil {
					errCh <- err
					return
				}
				slog.Info("[DG] Sent Deepgram final Results to client", "request_id", requestID)

			case "ready":
				slog.Info("[DG] Sidecar ready", "request_id", requestID)
				continue

			case "error":
				slog.Error("[DG] Sidecar error", "message", evt.Message, "request_id", requestID)
				_ = c.WriteJSON(fiber.Map{"type": "Error", "message": evt.Message})
				errCh <- nil
				return

			case "done":
				slog.Info("[DG] Sidecar done, ending session", "request_id", requestID)
				errCh <- nil
				return

			default:
				slog.Info("[DG] Unknown sidecar event, forwarding", "type", evt.Type, "request_id", requestID)
				if writeErr := c.WriteMessage(websocket.TextMessage, msg); writeErr != nil {
					errCh <- writeErr
					return
				}
			}
		}
	}()

	<-errCh
	slog.Info("[DG] Deepgram-compat session ended", "request_id", requestID)
}

// buildDGResults converts a sidecar stream event into a Deepgram Results event.
func buildDGResults(evt sidecarStreamEvent, isFinal, speechFinal bool, meta dgModelMeta) dgResults {
	words := make([]dgWord, 0, len(evt.Words))
	for _, w := range evt.Words {
		words = append(words, dgWord{
			Word:           w.Word,
			Start:          w.Start,
			End:            w.End,
			Confidence:     0.99,
			PunctuatedWord: w.Word,
		})
	}

	duration := evt.End - evt.Start
	if duration < 0 {
		duration = 0
	}

	return dgResults{
		Type:         "Results",
		ChannelIndex: []int{0, 1},
		Duration:     duration,
		Start:        evt.Start,
		IsFinal:      isFinal,
		SpeechFinal:  speechFinal,
		Channel: dgChannel{
			Alternatives: []dgAlternative{
				{
					Transcript: evt.Text,
					Confidence: 0.99,
					Words:      words,
				},
			},
		},
		Metadata:     meta,
		FromFinalize: false,
	}
}
