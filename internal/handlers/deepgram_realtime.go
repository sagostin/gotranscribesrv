package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
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
	requestID string
	ws        *websocket.Conn
	sidecar   *ws.Conn
	modelMeta dgModelMeta
	stats     *dgSessionStats
	// lastResultEnd is the stream-relative end time (seconds) of the last
	// final emitted — used to compute start/duration for the next final.
	// Only touched by handleSidecarEvent (single goroutine).
	lastResultEnd float64
	// encoding/sampleRate are the client's audio params, used for the
	// terminal Metadata duration math.
	encoding   string
	sampleRate string
	// Spec gating: UtteranceEnd only when utterance_end_ms is set,
	// SpeechStarted only when vad_events=true.
	utteranceEnd bool
	vadEvents    bool
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
	// Deepgram spec: interim_results defaults to false.
	interimResults := c.Query("interim_results", "false") == "true"

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
		"user_agent":      c.Headers("User-Agent"),
		"engine":          model,
		"interim_results": interimResults,
		"language":        c.Query("language", "en"),
		"sample_rate":     c.Query("sample_rate", "16000"),
		"encoding":        c.Query("encoding", "linear16"),
		"itn":             c.Query("itn", "true") != "false",
		"vad":             c.Query("vad", ""),
	}))

	sidecarConn, _, err := ws.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		slog.Error("[DG-RT] failed to connect to sidecar realtime WebSocket", "error", err, "request_id", requestID)
		h.lm.SendLog(h.lm.BuildLog("DEEPGRAM_REALTIME_CONNECT_FAILED", "DeepgramRealtimeConnectFailed", slog.LevelError, map[string]interface{}{
			"endpoint":   "/v2/listen",
			"ip":         c.IP(),
			"request_id": requestID,
			"engine":     model,
			"url":        u.String(),
		}, err))
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
		stats:     &dgSessionStats{},
		modelMeta: dgModelMeta{
			RequestID: requestID,
			ModelInfo: map[string]string{
				"name":    model,
				"version": "2026-03-01",
				"arch":    "parakeet-streaming",
			},
			ModelUUID: uuid.New().String(),
		},
		encoding:     c.Query("encoding", "linear16"),
		sampleRate:   c.Query("sample_rate", "16000"),
		utteranceEnd: c.Query("utterance_end_ms") != "",
		vadEvents:    c.Query("vad_events", "false") == "true",
	}

	// Send Deepgram Metadata event on connection open (same shape as legacy handler).
	_ = c.WriteJSON(fiber.Map{
		"type":            "Metadata",
		"transaction_key": "deprecated",
		"request_id":      requestID,
		"sha256":          fmt.Sprintf("%x", sha256.Sum256([]byte(requestID))),
		"created":         time.Now().UTC().Format(time.RFC3339),
		"duration":        0,
		"channels":        1,
		"model_info":      sess.modelMeta.ModelInfo,
		"model_uuid":      sess.modelMeta.ModelUUID,
	})

	errCh := make(chan error, 2)
	// done is closed when the session ends — see DeepgramHandler.handle
	// for why teardown-induced client read errors are not logged.
	done := make(chan struct{})
	// wg tracks both forwarding goroutines — see DeepgramHandler.handle
	// for why handle must not return while either is still running.
	var wg sync.WaitGroup
	wg.Add(2)

	// Client → sidecar: forward raw binary audio verbatim
	go func() {
		defer wg.Done()
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				select {
				case <-done:
					// Teardown-induced: session already ended.
				default:
					slog.Info("[DG-RT] Client read error (connection closed?)", "error", err, "request_id", requestID)
					h.lm.SendLog(h.lm.BuildLog("DEEPGRAM_REALTIME_CLIENT_READ_ERROR", "DeepgramRealtimeClientReadError", slog.LevelWarn, map[string]interface{}{
						"endpoint":    "/v2/listen",
						"ip":          c.IP(),
						"request_id":  requestID,
						"engine":      model,
						"total_bytes": sess.stats.bytes(),
					}, err))
				}
				errCh <- err
				return
			}
			if mt == websocket.BinaryMessage {
				// Tolerate binary-framed control messages (Deepgram spec
				// says text frames; some clients misbehave). A binary frame
				// that merely starts with '{' must strictly parse as a
				// known control message, so PCM audio can never
				// false-positive.
				if len(msg) > 0 && msg[0] == '{' {
					if handled := interceptControl(sess, msg); handled {
						continue
					}
				}
				if len(msg) > 0 {
					sess.stats.addAudio(len(msg))
				}
				if err := sidecarConn.WriteMessage(websocket.BinaryMessage, msg); err != nil {
					slog.Error("[DG-RT] Failed to forward audio to sidecar", "error", err, "request_id", requestID)
					h.lm.SendLog(h.lm.BuildLog("DEEPGRAM_REALTIME_FORWARD_FAILED", "DeepgramRealtimeForwardFailed", slog.LevelError, map[string]interface{}{
						"endpoint":    "/v2/listen",
						"ip":          c.IP(),
						"request_id":  requestID,
						"engine":      model,
						"total_bytes": sess.stats.bytes(),
					}, err))
					errCh <- err
					return
				}
			} else if mt == websocket.TextMessage {
				// Intercept Deepgram control messages: KeepAlive is
				// consumed locally; Finalize/CloseStream are tracked for
				// flush bookkeeping, then forwarded (the sidecar's
				// /stream/realtime speaks the same control JSON).
				if interceptControl(sess, msg) {
					continue
				}
				// Forward anything else (e.g., {"action":"stop"})
				if err := sidecarConn.WriteMessage(websocket.TextMessage, msg); err != nil {
					errCh <- err
					return
				}
			}
		}
	}()

	// Sidecar → client: translate JSON events → Deepgram event schema
	go func() {
		defer wg.Done()
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

	// Signal teardown + wait for both goroutines to exit — see
	// DeepgramHandler.handle for the full rationale (including why
	// SetReadDeadline is needed to unblock the client read goroutine).
	close(done)
	_ = sidecarConn.Close()
	_ = c.SetReadDeadline(time.Now())
	_ = c.Close()
	wg.Wait()

	// Log usage — processing time = first audio frame → last final result
	totalAudioBytes, frameCount, firstAudioAt, lastResultAt := sess.stats.snapshot()
	audioDurationMs := 0
	if totalAudioBytes > 0 {
		// Duration from the client's actual encoding/sample_rate (see the
		// v1 handler — mulaw 8kHz is 8 bytes/ms, not 32).
		bps := audioBytesPerSecond(sess.encoding, sess.sampleRate)
		audioDurationMs = int(float64(totalAudioBytes) / bps * 1000)
	}
	processTimeMs := 0
	if !firstAudioAt.IsZero() && !lastResultAt.IsZero() {
		processTimeMs = int(lastResultAt.Sub(firstAudioAt).Milliseconds())
	}
	userID, _ := c.Locals("user_id").(string)
	apiKeyID, _ := c.Locals("api_key_id").(string)
	middleware.LogWebSocketUsage(h.db, userID, apiKeyID, "asr_deepgram_realtime",
		audioDurationMs, processTimeMs, false)

	slog.Info("[DG-RT] Deepgram-realtime session ended", "request_id", requestID,
		"audio_bytes", totalAudioBytes, "audio_duration_ms", audioDurationMs,
		"process_ms", processTimeMs, "frames", frameCount)

	h.lm.SendLog(h.lm.BuildLog("DEEPGRAM_REALTIME_ENDED", "DeepgramRealtimeEnded", slog.LevelInfo, map[string]interface{}{
		"endpoint":          "/v2/listen",
		"ip":                c.IP(),
		"request_id":        requestID,
		"user_agent":        c.Headers("User-Agent"),
		"engine":            model,
		"audio_bytes":       totalAudioBytes,
		"audio_duration_ms": audioDurationMs,
		"process_ms":        processTimeMs,
		"frame_count":       frameCount,
		"realtime_x":        realtimeFactor(audioDurationMs, processTimeMs),
	}))
}

// interceptControl inspects a client frame for a Deepgram control message.
// KeepAlive is consumed locally (the sidecar has no use for it); Finalize
// and CloseStream are recorded for flush bookkeeping and forwarded to the
// sidecar as-is. Returns true when the frame was a control message that
// should not be treated as audio.
func interceptControl(sess *dgRealtimeSession, msg []byte) bool {
	var ctrl map[string]interface{}
	if json.Unmarshal(msg, &ctrl) != nil {
		return false
	}
	ctrlType, ok := ctrl["type"].(string)
	if !ok {
		return false
	}
	switch ctrlType {
	case "KeepAlive":
		slog.Info("[DG-RT] KeepAlive from client", "request_id", sess.requestID)
		return true
	case "Finalize":
		sess.stats.requestFlush(false)
		slog.Info("[DG-RT] Finalize from client, forwarding to sidecar", "request_id", sess.requestID)
	case "CloseStream":
		sess.stats.requestFlush(true)
		slog.Info("[DG-RT] CloseStream from client, forwarding to sidecar", "request_id", sess.requestID)
	default:
		return false
	}
	if err := sess.sidecar.WriteMessage(ws.TextMessage, msg); err != nil {
		slog.Error("[DG-RT] Failed to forward control to sidecar", "type", ctrlType, "error", err, "request_id", sess.requestID)
	}
	return true
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
		// Only emitted when the client passed vad_events=true.
		if !sess.vadEvents {
			return
		}
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
			"channel_index": []int{0},
			"duration":      0.0,
			"start":         0.0,
			"channel": fiber.Map{
				"alternatives": []any{
					fiber.Map{"transcript": text, "confidence": 0.99, "words": []any{}},
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
		fromFinalize, _ := ev["from_finalize"].(bool)
		sess.stats.markResult()
		// Stream-relative timing: the sidecar reports the final's end time;
		// the segment start is the previous final's end.
		end := asFloat(ev["time"])
		start := sess.lastResultEnd
		duration := end - start
		if duration < 0 {
			duration = 0
		}
		sess.lastResultEnd = end
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
			"channel_index": []int{0},
			"duration":      duration,
			"start":         start,
			"channel": fiber.Map{
				"alternatives": []any{
					fiber.Map{"transcript": text, "confidence": 0.99, "words": []any{}},
				},
			},
			"is_final":      true,
			"speech_final":  isSpeechFinal,
			"metadata":      sess.modelMeta,
			"from_finalize": fromFinalize,
		})
		// Response-sent event → Loki (redacted transcript).
		finalFields := map[string]interface{}{
			"endpoint":      "/v2/listen",
			"ip":            sess.ws.IP(),
			"request_id":    sess.requestID,
			"engine":        sess.modelMeta.ModelInfo["name"],
			"transcript":    redactedFinal,
			"pii_redacted":  len(piiItems),
			"is_final":      true,
			"speech_final":  isSpeechFinal,
			"from_finalize": fromFinalize,
		}
		if len(piiItems) > 0 {
			finalFields["pii_entity_types"] = piiEntityTypes(piiItems)
		}
		h.lm.SendLog(h.lm.BuildLog("DEEPGRAM_REALTIME_FINAL_SENT", "DeepgramRealtimeFinalSent", slog.LevelInfo, finalFields))
		if isSpeechFinal && sess.utteranceEnd {
			// Spec shape: {"type":"UtteranceEnd","channel":[0],"last_word_end":<float>}
			// Only emitted when the client set utterance_end_ms.
			_ = sess.ws.WriteJSON(fiber.Map{
				"type":          "UtteranceEnd",
				"channel":       []int{0},
				"last_word_end": asFloat(ev["time"]),
			})
		}
	case "end_of_turn":
		// already covered by speech_final final
	case "speech_stopped":
		// informational — Deepgram has SpeechStarted/SpeechEnded but not SpeechStopped;
		// skip to avoid protocol noise.
	case "done":
		// Deepgram spec: CloseStream earns a terminal Metadata summary
		// before the session ends (mirrors the v1 handler).
		if sess.stats.closeStreamRequested() {
			bps := audioBytesPerSecond(sess.encoding, sess.sampleRate)
			_ = sess.ws.WriteJSON(fiber.Map{
				"type":            "Metadata",
				"transaction_key": "deprecated",
				"request_id":      sess.requestID,
				"sha256":          fmt.Sprintf("%x", sha256.Sum256([]byte(sess.requestID))),
				"created":         time.Now().UTC().Format(time.RFC3339),
				"duration":        float64(sess.stats.bytes()) / bps,
				"channels":        1,
				"model_info":      sess.modelMeta.ModelInfo,
				"model_uuid":      sess.modelMeta.ModelUUID,
			})
		}
		// Connection will close naturally
	case "error":
		errMsg := asString(ev["message"])
		slog.Error("[DG-RT] Sidecar error", "message", errMsg, "request_id", sess.requestID)
		h.lm.SendLog(h.lm.BuildLog("DEEPGRAM_REALTIME_SESSION_ERROR", "DeepgramRealtimeSessionError", slog.LevelError, map[string]interface{}{
			"endpoint":   "/v2/listen",
			"ip":         sess.ws.IP(),
			"request_id": sess.requestID,
			"engine":     sess.modelMeta.ModelInfo["name"],
		}, errMsg))
		_ = sess.ws.WriteJSON(fiber.Map{
			"type":    "Error",
			"message": errMsg,
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
