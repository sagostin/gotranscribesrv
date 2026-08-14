package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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
	"github.com/shaunagostinho/gotranscribesrv/internal/pii"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
	"gorm.io/gorm"
)

// DeepgramHandler handles the Deepgram-compatible streaming endpoint.
// WS /v1/listen — proxies to the audio sidecar's /stream WebSocket,
// translating between Deepgram's protocol and the internal protocol.
//
// NOTE: Transcript text is only ever added to Loki events AFTER PII
// redaction (DEEPGRAM_PARTIAL_SENT / DEEPGRAM_FINAL_SENT) — see the
// partial/final cases below and internal/handlers/{asr,whisper,watson}.go
// for the pattern. Session-end logs deliberately do NOT include
// transcript text (only audio bytes / duration / process time); if that
// ever changes, the text MUST go through the redactor first.
type DeepgramHandler struct {
	sidecar    *sidecar.Client
	redactor   *pii.Redactor
	db         *gorm.DB
	defaultITN bool
	lm         *logging.LogManager
}

// NewDeepgramHandler creates a new DeepgramHandler.
func NewDeepgramHandler(sc *sidecar.Client, redactor *pii.Redactor, db *gorm.DB, defaultITN bool, lm *logging.LogManager) *DeepgramHandler {
	return &DeepgramHandler{sidecar: sc, redactor: redactor, db: db, defaultITN: defaultITN, lm: lm}
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
	Type           string            `json:"type"`
	TransactionKey string            `json:"transaction_key"`
	RequestID      string            `json:"request_id"`
	SHA256         string            `json:"sha256"`
	Created        string            `json:"created"`
	Duration       float64           `json:"duration"`
	Channels       int               `json:"channels"`
	ModelInfo      map[string]string `json:"model_info"`
}

type dgModelMeta struct {
	RequestID string            `json:"request_id"`
	ModelInfo map[string]string `json:"model_info"`
	ModelUUID string            `json:"model_uuid"`
}

// sidecarStreamEvent represents a JSON event from the audio sidecar /stream WebSocket.
type sidecarStreamEvent struct {
	Type         string         `json:"type"`
	Text         string         `json:"text,omitempty"`
	Start        float64        `json:"start,omitempty"`
	End          float64        `json:"end,omitempty"`
	Words        []sidecar.Word `json:"words,omitempty"`
	IsFinal      bool           `json:"is_final,omitempty"`
	FromFinalize bool           `json:"from_finalize,omitempty"`
	Message      string         `json:"message,omitempty"`
}

// handle proxies WebSocket frames between the Deepgram-compatible client and
// the audio sidecar's /stream endpoint, translating the event protocol.
func (h *DeepgramHandler) handle(c *websocket.Conn) {
	defer c.Close()

	// Limit incoming message size to 1MB to prevent memory exhaustion
	c.SetReadLimit(1 * 1024 * 1024)

	// Reuse the id minted by the HTTP RequestID middleware (if present)
	// so the access/upgrade logs correlate with this session; the id is
	// also echoed to the client in the Deepgram Metadata event.
	requestID, _ := c.Locals(middleware.RequestIDLocalKey).(string)
	if requestID == "" {
		requestID = uuid.New().String()
		c.Locals(middleware.RequestIDLocalKey, requestID)
	}
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

	// Send Deepgram Metadata event on connection open. transaction_key is
	// "deprecated" upstream; sha256 is included for strict-schema clients
	// (hash of the request id — Deepgram hashes request internals, clients
	// treat it as opaque).
	meta := dgMetadata{
		Type:           "Metadata",
		TransactionKey: "deprecated",
		RequestID:      requestID,
		SHA256:         fmt.Sprintf("%x", sha256.Sum256([]byte(requestID))),
		Created:        time.Now().UTC().Format(time.RFC3339),
		Duration:       0,
		Channels:       1,
		ModelInfo:      modelMeta.ModelInfo,
	}
	if err := c.WriteJSON(meta); err != nil {
		slog.Error("failed to send Metadata event", "error", err, "request_id", requestID)
		return
	}

	// Connect to audio sidecar /stream WebSocket
	sidecarURL := h.sidecar.StreamURL()
	u, err := url.Parse(sidecarURL)
	if err != nil {
		slog.Error("invalid sidecar stream URL", "error", err, "request_id", requestID)
		_ = c.WriteJSON(fiber.Map{"type": "Error", "message": "internal configuration error"})
		return
	}

	// Forward query params to sidecar
	q := u.Query()
	for _, param := range []string{"language", "diarize", "encoding", "sample_rate", "itn"} {
		if v := c.Query(param); v != "" {
			q.Set(param, v)
		}
	}
	// ITN: if the client didn't pass ?itn= and the server-wide default is
	// off, inject it so ENABLE_ITN=false in .env actually disables ITN
	// for Deepgram-compat clients.
	if c.Query("itn") == "" && !h.defaultITN {
		q.Set("itn", "false")
	}
	u.RawQuery = q.Encode()

	slog.Info("[DG] Query params", "request_id", requestID,
		"encoding", c.Query("encoding", "linear16"),
		"sample_rate", c.Query("sample_rate", "16000"),
		"language", c.Query("language", "en"))

	sidecarConn, _, err := ws.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		slog.Error("failed to connect to sidecar /stream WebSocket", "error", err, "request_id", requestID)
		h.lm.SendLog(h.lm.BuildLog("DEEPGRAM_CONNECT_FAILED", "DeepgramConnectFailed", slog.LevelError, map[string]interface{}{
			"endpoint":   "/v1/listen",
			"ip":         c.IP(),
			"request_id": requestID,
			"url":        u.String(),
		}, err))
		_ = c.WriteJSON(fiber.Map{"type": "Error", "message": "transcription service unavailable"})
		return
	}
	slog.Info("[DG] Connected to sidecar /stream", "request_id", requestID, "url", u.String())
	defer sidecarConn.Close()

	slog.Info("[DG] Deepgram-compat session started", "request_id", requestID,
		"interim_results", interimResults)
	metrics.ActiveWebSocketConnections.WithLabelValues("deepgram").Inc()
	defer metrics.ActiveWebSocketConnections.WithLabelValues("deepgram").Dec()

	h.lm.SendLog(h.lm.BuildLog("DEEPGRAM_SESSION_STARTED", "DeepgramSessionStarted", slog.LevelInfo, map[string]interface{}{
		"endpoint":        "/v1/listen",
		"ip":              c.IP(),
		"request_id":      requestID,
		"interim_results": interimResults,
		"language":        c.Query("language", "en"),
		"sample_rate":     c.Query("sample_rate", "16000"),
		"encoding":        c.Query("encoding", "linear16"),
		"diarize":         c.Query("diarize", "false") == "true",
		"itn":             itnEnabled(c, h.defaultITN),
	}))
	var totalAudioBytes int
	var firstAudioAt time.Time
	var lastResultAt time.Time
	errCh := make(chan error, 2)

	// Client → Sidecar: forward binary audio and translate control messages
	go func() {
		var frameCount int
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
						case "Finalize":
							// Deepgram Finalize: flush the stream and answer
							// with a Results event carrying from_finalize=true.
							// The sidecar transcribes the buffered audio and
							// emits a final event WITHOUT closing the session.
							slog.Info("[DG] Finalize from client, forwarding to sidecar", "request_id", requestID, "total_frames", frameCount, "total_bytes", totalAudioBytes)
							_ = sidecarConn.WriteMessage(ws.TextMessage,
								[]byte(`{"type":"Finalize"}`))
							continue
						case "CloseStream":
							slog.Info("[DG] CloseStream from client, forwarding to sidecar", "request_id", requestID, "total_frames", frameCount, "total_bytes", totalAudioBytes)
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
			totalAudioBytes += len(msg)
			if frameCount == 1 {
				firstAudioAt = time.Now()
			}
			if frameCount%50 == 1 {
				slog.Info("[DG] Forwarding audio to sidecar", "frame", frameCount, "bytes", len(msg), "total_bytes", totalAudioBytes, "request_id", requestID)
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

			// Debug-level only — the raw message body contains
			// transcript text (PII). At info level we surface only
			// the parsed event type, not the raw JSON.
			slog.Debug("[DG] Received from sidecar", "msg", string(msg[:min(len(msg), 500)]), "request_id", requestID)

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
				// Redact transcript text before logging — partials
				// fire on every interim result, so this matters.
				redactedPartial, piiItems, piiErr := h.redactor.RedactText(context.Background(), evt.Text)
				if piiErr != nil {
					h.lm.SendLog(h.lm.BuildLog("PII_REDACTOR_ERROR", "PIIRedactorError", slog.LevelWarn, map[string]interface{}{
						"endpoint":   "/v1/listen",
						"ip":         c.IP(),
						"text_len":   len(evt.Text),
						"request_id": requestID,
					}, piiErr))
				}
				slog.Info("[DG] Sidecar partial result", "text", redactedPartial, "request_id", requestID)
				if !interimResults {
					slog.Info("[DG] Skipping partial (interim_results=false)", "request_id", requestID)
					continue
				}
				dgEvt := buildDGResults(evt, false, false, modelMeta)
				if err := c.WriteJSON(dgEvt); err != nil {
					errCh <- err
					return
				}
				// Response-sent event → Loki. Transcript is PII-redacted;
				// partials ship at debug level due to their volume.
				partialFields := map[string]interface{}{
					"endpoint":     "/v1/listen",
					"ip":           c.IP(),
					"request_id":   requestID,
					"transcript":   redactedPartial,
					"pii_redacted": len(piiItems),
					"word_count":   len(evt.Words),
					"duration":     dgEvt.Duration,
					"start":        dgEvt.Start,
					"is_final":     false,
				}
				if len(piiItems) > 0 {
					partialFields["pii_entity_types"] = piiEntityTypes(piiItems)
				}
				h.lm.SendLog(h.lm.BuildLog("DEEPGRAM_PARTIAL_SENT", "DeepgramPartialSent", slog.LevelDebug, partialFields))

			case "final":
				redactedFinal, piiItems, piiErr := h.redactor.RedactText(context.Background(), evt.Text)
				if piiErr != nil {
					h.lm.SendLog(h.lm.BuildLog("PII_REDACTOR_ERROR", "PIIRedactorError", slog.LevelWarn, map[string]interface{}{
						"endpoint":   "/v1/listen",
						"ip":         c.IP(),
						"text_len":   len(evt.Text),
						"request_id": requestID,
					}, piiErr))
				}
				slog.Info("[DG] Sidecar final result", "text", redactedFinal, "request_id", requestID)
				dgEvt := buildDGResults(evt, true, true, modelMeta)
				if err := c.WriteJSON(dgEvt); err != nil {
					errCh <- err
					return
				}
				lastResultAt = time.Now()
				// Response-sent event → Loki. Transcript is PII-redacted.
				finalFields := map[string]interface{}{
					"endpoint":      "/v1/listen",
					"ip":            c.IP(),
					"request_id":    requestID,
					"transcript":    redactedFinal,
					"pii_redacted":  len(piiItems),
					"word_count":    len(evt.Words),
					"duration":      dgEvt.Duration,
					"start":         dgEvt.Start,
					"is_final":      true,
					"speech_final":  dgEvt.SpeechFinal,
					"from_finalize": dgEvt.FromFinalize,
				}
				if len(piiItems) > 0 {
					finalFields["pii_entity_types"] = piiEntityTypes(piiItems)
				}
				h.lm.SendLog(h.lm.BuildLog("DEEPGRAM_FINAL_SENT", "DeepgramFinalSent", slog.LevelInfo, finalFields))

			case "ready":
				slog.Info("[DG] Sidecar ready", "request_id", requestID)
				continue

			case "error":
				slog.Error("[DG] Sidecar error", "message", evt.Message, "request_id", requestID)
				h.lm.SendLog(h.lm.BuildLog("DEEPGRAM_SESSION_ERROR", "DeepgramSessionError", slog.LevelError, map[string]interface{}{
					"endpoint":   "/v1/listen",
					"ip":         c.IP(),
					"request_id": requestID,
				}, evt.Message))
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

	// Log usage — processing time = first audio frame → last final result
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
	middleware.LogWebSocketUsage(h.db, userID, apiKeyID, "asr_deepgram",
		audioDurationMs, processTimeMs, false)

	slog.Info("[DG] Deepgram-compat session ended", "request_id", requestID,
		"audio_bytes", totalAudioBytes, "audio_duration_ms", audioDurationMs,
		"process_ms", processTimeMs)

	h.lm.SendLog(h.lm.BuildLog("DEEPGRAM_SESSION_ENDED", "DeepgramSessionEnded", slog.LevelInfo, map[string]interface{}{
		"endpoint":          "/v1/listen",
		"ip":                c.IP(),
		"request_id":        requestID,
		"audio_bytes":       totalAudioBytes,
		"audio_duration_ms": audioDurationMs,
		"process_ms":        processTimeMs,
		"realtime_x":        realtimeFactor(audioDurationMs, processTimeMs),
	}))
}

// itnEnabled returns whether ITN is on for this WS request, factoring
// in both the client's ?itn= override and the server-wide default.
func itnEnabled(c *websocket.Conn, defaultITN bool) bool {
	if v := c.Query("itn"); v != "" {
		return v != "false"
	}
	return defaultITN
}

// realtimeFactor returns audio_ms / process_ms (a 1-hour file processed
// in 60s yields 60x). Returns 0 when either input is zero.
func realtimeFactor(audioMs, processMs int) float64 {
	if processMs <= 0 || audioMs <= 0 {
		return 0
	}
	return float64(audioMs) / float64(processMs)
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
		FromFinalize: evt.FromFinalize,
	}
}
