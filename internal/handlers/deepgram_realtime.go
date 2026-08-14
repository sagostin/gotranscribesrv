package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"strings"
	"time"

	ws "github.com/fasthttp/websocket"
	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/shaunagostinho/gotranscribesrv/internal/logging"
	"github.com/shaunagostinho/gotranscribesrv/internal/metrics"
	"github.com/shaunagostinho/gotranscribesrv/internal/middleware"
	"github.com/shaunagostinho/gotranscribesrv/internal/pii"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
	"gorm.io/gorm"
)

// DeepgramRealtimeHandler proxies a Deepgram-compatible WebSocket session
// to the audio sidecar's /stream/realtime endpoint. The legacy /v1/listen
// handler (DeepgramCompatHandler in deepgram.go) still proxies to the
// buffered /stream route — this new handler exposes the **true real-time**
// streaming engine via /v2/listen so existing clients are untouched.
//
// Deepgram Nova model IDs map to sidecar streaming engines:
//
//	"nova-3", "nova-2"         → eou-320 (default; balanced)
//	"nova-2-eou"                → eou-320
//	"nova-3-unified"            → unified-320
//	"2-nova"                    → nemotron-560
//
// Explicit engine IDs (eou-160, nemotron-1120, unified-640, …) pass through.
type DeepgramRealtimeHandler struct {
	sc       *sidecar.Client
	redactor *pii.Redactor
	lm       *logging.LogManager
	db       *gorm.DB
}

// NewDeepgramRealtimeHandler constructs the handler.
func NewDeepgramRealtimeHandler(sc *sidecar.Client, redactor *pii.Redactor, lm *logging.LogManager, db *gorm.DB) *DeepgramRealtimeHandler {
	return &DeepgramRealtimeHandler{sc: sc, redactor: redactor, lm: lm, db: db}
}

// Upgrade returns the Fiber middleware that upgrades HTTP to WebSocket.
func (h *DeepgramRealtimeHandler) Upgrade() fiber.Handler {
	return websocket.New(h.handle)
}

type dgRealtimeSession struct {
	requestID       string
	ws              *websocket.Conn
	sidecar         *ws.Conn
	modelMeta       dgModelMeta
	speechEndF      bool
	totalAudioBytes int
	firstAudioAt    time.Time
	lastResultAt    time.Time
}

func (h *DeepgramRealtimeHandler) handle(c *websocket.Conn) {
	defer c.Close()
	c.SetReadLimit(8 * 1024 * 1024)

	requestID, _ := c.Locals(middleware.RequestIDLocalKey).(string)
	if requestID == "" {
		requestID = uuid.New().String()
		c.Locals(middleware.RequestIDLocalKey, requestID)
	}

	model := deepgramModelToEngine(c.Query("model"))
	interimResults := c.Query("interim_results", "true") == "true"

	sidecarURL := h.sc.DeepgramRealtimeURL(model)
	u, err := url.Parse(sidecarURL)
	if err != nil {
		_ = c.WriteJSON(fiber.Map{"type": "Error", "message": "internal configuration error"})
		return
	}
	q := u.Query()
	for _, p := range []string{"encoding", "sample_rate", "itn", "vad"} {
		if v := c.Query(p); v != "" {
			q.Set(p, v)
		}
	}
	u.RawQuery = q.Encode()

	slog.Info("[DG-RT] Session started", "request_id", requestID, "engine", model, "url", u.String())
	h.lm.SendLog(h.lm.BuildLog("DEEPGRAM_REALTIME_STARTED", "DeepgramRealtimeStarted", slog.LevelInfo, map[string]interface{}{
		"endpoint":        "/v2/listen",
		"ip":              c.IP(),
		"request_id":      requestID,
		"engine":          model,
		"interim_results": interimResults,
	}))

	sidecarConn, _, err := ws.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		_ = c.WriteJSON(fiber.Map{"type": "Error", "message": "transcription service unavailable"})
		return
	}
	defer sidecarConn.Close()
	metrics.ActiveWebSocketConnections.WithLabelValues("deepgram_realtime").Inc()
	defer metrics.ActiveWebSocketConnections.WithLabelValues("deepgram_realtime").Dec()

	sess := &dgRealtimeSession{
		requestID: requestID,
		ws:        c,
		sidecar:   sidecarConn,
		modelMeta: dgModelMeta{
			RequestID: requestID,
			ModelInfo: map[string]string{
				"name":    model,
				"version": "2026-03-01",
				"arch":    "parakeet-streaming",
			},
			ModelUUID: uuid.New().String(),
		},
	}

	// Send Deepgram Metadata event on connection open (same shape as legacy handler).
	_ = c.WriteJSON(fiber.Map{
		"type":       "Metadata",
		"request_id": requestID,
		"created":    time.Now().UTC().Format(time.RFC3339),
		"duration":   0,
		"channels":   1,
		"model_info": sess.modelMeta.ModelInfo,
		"model_uuid": sess.modelMeta.ModelUUID,
	})

	errCh := make(chan error, 2)

	// Client → sidecar: forward raw binary audio verbatim
	go func() {
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			if mt == websocket.BinaryMessage {
				if len(msg) > 0 {
					sess.totalAudioBytes += len(msg)
					if sess.firstAudioAt.IsZero() {
						sess.firstAudioAt = time.Now()
					}
				}
				if err := sidecarConn.WriteMessage(websocket.BinaryMessage, msg); err != nil {
					errCh <- err
					return
				}
			} else if mt == websocket.TextMessage {
				// Forward JSON control messages (e.g., {"action":"stop"})
				if err := sidecarConn.WriteMessage(websocket.TextMessage, msg); err != nil {
					errCh <- err
					return
				}
			}
		}
	}()

	// Sidecar → client: translate JSON events → Deepgram event schema
	go func() {
		for {
			_, msg, err := sidecarConn.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			h.handleSidecarEvent(sess, msg, interimResults)
		}
	}()

	<-errCh

	// Log usage — processing time = first audio frame → last final result
	audioDurationMs := 0
	if sess.totalAudioBytes > 0 {
		audioDurationMs = sess.totalAudioBytes / 32 // PCM 16-bit 16kHz mono = 32 bytes/ms
	}
	processTimeMs := 0
	if !sess.firstAudioAt.IsZero() && !sess.lastResultAt.IsZero() {
		processTimeMs = int(sess.lastResultAt.Sub(sess.firstAudioAt).Milliseconds())
	}
	userID, _ := c.Locals("user_id").(string)
	apiKeyID, _ := c.Locals("api_key_id").(string)
	middleware.LogWebSocketUsage(h.db, userID, apiKeyID, "asr_deepgram_realtime",
		audioDurationMs, processTimeMs, false)

	slog.Info("[DG-RT] Deepgram-realtime session ended", "request_id", requestID,
		"audio_bytes", sess.totalAudioBytes, "audio_duration_ms", audioDurationMs,
		"process_ms", processTimeMs)

	h.lm.SendLog(h.lm.BuildLog("DEEPGRAM_REALTIME_ENDED", "DeepgramRealtimeEnded", slog.LevelInfo, map[string]interface{}{
		"endpoint":          "/v2/listen",
		"ip":                c.IP(),
		"request_id":        requestID,
		"audio_bytes":       sess.totalAudioBytes,
		"audio_duration_ms": audioDurationMs,
		"process_ms":        processTimeMs,
		"realtime_x":        realtimeFactor(audioDurationMs, processTimeMs),
	}))
}

func (h *DeepgramRealtimeHandler) handleSidecarEvent(sess *dgRealtimeSession, msg []byte, interimResults bool) {
	var ev map[string]any
	if err := json.Unmarshal(msg, &ev); err != nil {
		return
	}
	t, _ := ev["type"].(string)
	switch t {
	case "ready":
		// Already sent Metadata on connect; nothing to do.
	case "speech_started":
		// Spec shape: {"type":"SpeechStarted","channel":[0],"timestamp":<float>}
		_ = sess.ws.WriteJSON(fiber.Map{
			"type":      "SpeechStarted",
			"channel":   []int{0},
			"timestamp": asFloat(ev["time"]),
		})
	case "partial":
		if !interimResults {
			return
		}
		text, _ := ev["text"].(string)
		// Redact transcript text before logging — the raw text goes to
		// the client untouched, only the redacted form reaches Loki.
		redactedPartial, piiItems, piiErr := h.redactor.RedactText(context.Background(), text)
		if piiErr != nil {
			h.lm.SendLog(h.lm.BuildLog("PII_REDACTOR_ERROR", "PIIRedactorError", slog.LevelWarn, map[string]interface{}{
				"endpoint":   "/v2/listen",
				"ip":         sess.ws.IP(),
				"text_len":   len(text),
				"request_id": sess.requestID,
			}, piiErr))
		}
		_ = sess.ws.WriteJSON(fiber.Map{
			"type":          "Results",
			"channel_index": []int{0, 1},
			"duration":      0.0,
			"start":         0.0,
			"channel": fiber.Map{
				"alternatives": []any{
					fiber.Map{"transcript": text, "confidence": 0.0, "words": []any{}},
				},
			},
			"is_final":      false,
			"speech_final":  false,
			"metadata":      sess.modelMeta,
			"from_finalize": false,
		})
		// Response-sent event → Loki (redacted). Debug level due to volume.
		partialFields := map[string]interface{}{
			"endpoint":     "/v2/listen",
			"ip":           sess.ws.IP(),
			"request_id":   sess.requestID,
			"engine":       sess.modelMeta.ModelInfo["name"],
			"transcript":   redactedPartial,
			"pii_redacted": len(piiItems),
			"is_final":     false,
		}
		if len(piiItems) > 0 {
			partialFields["pii_entity_types"] = piiEntityTypes(piiItems)
		}
		h.lm.SendLog(h.lm.BuildLog("DEEPGRAM_REALTIME_PARTIAL_SENT", "DeepgramRealtimePartialSent", slog.LevelDebug, partialFields))
	case "final":
		text, _ := ev["text"].(string)
		isSpeechFinal, _ := ev["speech_final"].(bool)
		sess.lastResultAt = time.Now()
		redactedFinal, piiItems, piiErr := h.redactor.RedactText(context.Background(), text)
		if piiErr != nil {
			h.lm.SendLog(h.lm.BuildLog("PII_REDACTOR_ERROR", "PIIRedactorError", slog.LevelWarn, map[string]interface{}{
				"endpoint":   "/v2/listen",
				"ip":         sess.ws.IP(),
				"text_len":   len(text),
				"request_id": sess.requestID,
			}, piiErr))
		}
		_ = sess.ws.WriteJSON(fiber.Map{
			"type":          "Results",
			"channel_index": []int{0, 1},
			"duration":      0.0,
			"start":         0.0,
			"channel": fiber.Map{
				"alternatives": []any{
					fiber.Map{"transcript": text, "confidence": 0.0, "words": []any{}},
				},
			},
			"is_final":      true,
			"speech_final":  isSpeechFinal,
			"metadata":      sess.modelMeta,
			"from_finalize": false,
		})
		// Response-sent event → Loki (redacted transcript).
		finalFields := map[string]interface{}{
			"endpoint":     "/v2/listen",
			"ip":           sess.ws.IP(),
			"request_id":   sess.requestID,
			"engine":       sess.modelMeta.ModelInfo["name"],
			"transcript":   redactedFinal,
			"pii_redacted": len(piiItems),
			"is_final":     true,
			"speech_final": isSpeechFinal,
		}
		if len(piiItems) > 0 {
			finalFields["pii_entity_types"] = piiEntityTypes(piiItems)
		}
		h.lm.SendLog(h.lm.BuildLog("DEEPGRAM_REALTIME_FINAL_SENT", "DeepgramRealtimeFinalSent", slog.LevelInfo, finalFields))
		if isSpeechFinal {
			// Spec shape: {"type":"UtteranceEnd","channel":[0],"last_word_end":<float>}
			_ = sess.ws.WriteJSON(fiber.Map{
				"type":          "UtteranceEnd",
				"channel":       []int{0},
				"last_word_end": asFloat(ev["time"]),
			})
			sess.speechEndF = true
		}
	case "end_of_turn":
		// already covered by speech_final final
	case "speech_stopped":
		// informational — Deepgram has SpeechStarted/SpeechEnded but not SpeechStopped;
		// skip to avoid protocol noise.
	case "done":
		// Connection will close naturally
	case "error":
		_ = sess.ws.WriteJSON(fiber.Map{
			"type":    "Error",
			"message": asString(ev["message"]),
		})
	}
}

// deepgramModelToEngine maps Deepgram model IDs to sidecar engines.
func deepgramModelToEngine(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return "eou-320"
	}
	switch m {
	case "nova-3", "nova-2", "nova-2-eou":
		return "eou-320"
	case "nova-3-unified", "nova-2-unified":
		return "unified-320"
	case "2-nova", "nova-2-nemotron":
		return "nemotron-560"
	}
	// Pass through any explicit engine ID
	if strings.HasPrefix(m, "eou-") || strings.HasPrefix(m, "nemotron-") || strings.HasPrefix(m, "unified-") {
		return m
	}
	// Unknown Nova model — fall back to default
	return "eou-320"
}
