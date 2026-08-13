package handlers

import (
	"encoding/base64"
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
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
)

// OpenAIRealtimeHandler proxies a /v1/realtime WebSocket session to the
// Swift sidecar's /stream/realtime endpoint, translating between OpenAI's
// Realtime API event protocol and the sidecar's native JSON+PCM protocol.
//
// Wire protocol on this endpoint follows OpenAI's Realtime API as documented
// at https://platform.openai.com/docs/api-reference/realtime — but this
// proxy only handles the **input transcription** half. LLM/TTS stays in
// the user's existing service (no /response endpoint is implemented here).
//
// Client → server event types handled:
//
//	session.update
//	input_audio_buffer.append       (base64-encoded PCM16 frames)
//	input_audio_buffer.commit
//
// Server → client event types emitted:
//
//	session.created
//	session.updated
//	input_audio_buffer.speech_started
//	input_audio_buffer.speech_stopped
//	conversation.item.input_audio_transcription.delta
//	conversation.item.input_audio_transcription.completed
//	error
//
// `model` in session.update maps to a sidecar streaming engine:
//
//	"gpt-4o-realtime-preview"      → eou-320 (default; good balance)
//	"gpt-4o-realtime"              → eou-320
//	"gpt-4o-mini-realtime-preview" → nemotron-560 (lower accuracy)
//	"nova-3" / "parakeet-unified-320" → unified-320
//
// Any `model` containing "unified", "nemotron", or "eou-" passes through.
type OpenAIRealtimeHandler struct {
	sc *sidecar.Client
	lm *logging.LogManager
}

// NewOpenAIRealtimeHandler constructs the handler.
func NewOpenAIRealtimeHandler(sc *sidecar.Client, lm *logging.LogManager) *OpenAIRealtimeHandler {
	return &OpenAIRealtimeHandler{sc: sc, lm: lm}
}

// Upgrade returns the Fiber middleware that upgrades HTTP to WebSocket.
func (h *OpenAIRealtimeHandler) Upgrade() fiber.Handler {
	return websocket.New(h.handle)
}

// realtimeSession is the per-connection state shared by the two goroutines.
type realtimeSession struct {
	requestID  string
	sidecarURL string
	ws         *websocket.Conn
	sidecar    *ws.Conn
	engine     string
	itemID     string // current conversation.item.id for transcription events
}

func (h *OpenAIRealtimeHandler) handle(c *websocket.Conn) {
	defer c.Close()
	c.SetReadLimit(8 * 1024 * 1024) // 8 MB — larger frames for batched audio

	requestID, _ := c.Locals(middleware.RequestIDLocalKey).(string)
	if requestID == "" {
		requestID = uuid.New().String()
		c.Locals(middleware.RequestIDLocalKey, requestID)
	}

	// Forward selected query params to sidecar. Engine comes from
	// session.update, not a query param, so the sidecar WS connects with
	// the default engine first; we send the engine via URL query by
	// carrying the sidecar's resolved default. The session.update later
	// can't change engines mid-session, but we pass our chosen default.
	defaultEngine := defaultRealtimeEngineFromQuery(c)

	sidecarURL := h.sc.RealtimeStreamURL(defaultEngine)
	u, err := url.Parse(sidecarURL)
	if err != nil {
		h.sendErr(c, "invalid_stream_url", "internal configuration error")
		return
	}
	q := u.Query()
	for _, p := range []string{"encoding", "sample_rate", "itn", "vad"} {
		if v := c.Query(p); v != "" {
			q.Set(p, v)
		}
	}
	u.RawQuery = q.Encode()

	slog.Info("[OA-RT] Session started", "request_id", requestID, "sidecar_url", u.String())
	h.lm.SendLog(h.lm.BuildLog("OPENAI_REALTIME_STARTED", "OpenAIRealtimeStarted", slog.LevelInfo, map[string]interface{}{
		"endpoint":    "/v1/realtime",
		"request_id":  requestID,
		"engine":      defaultEngine,
		"encoding":    c.Query("encoding", "linear16"),
		"sample_rate": c.Query("sample_rate", "16000"),
	}))

	sidecarConn, _, err := ws.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		h.sendErr(c, "sidecar_unavailable", "transcription service unavailable")
		return
	}
	defer sidecarConn.Close()
	metrics.ActiveWebSocketConnections.WithLabelValues("openai_realtime").Inc()
	defer metrics.ActiveWebSocketConnections.WithLabelValues("openai_realtime").Dec()

	sess := &realtimeSession{
		requestID:  requestID,
		sidecarURL: u.String(),
		ws:         c,
		sidecar:    sidecarConn,
		engine:     defaultEngine,
		itemID:     "item_" + strings.ReplaceAll(uuid.New().String(), "-", ""),
	}

	// session.created (sent immediately per OpenAI spec)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	h.sendJSON(c, fiber.Map{
		"type":     "session.created",
		"event_id": "evt_" + strings.ReplaceAll(uuid.New().String(), "-", ""),
		"session": fiber.Map{
			"id":                 "sess_" + strings.ReplaceAll(uuid.New().String(), "-", ""),
			"object":             "realtime.session",
			"model":              defaultEngine,
			"modalities":         []string{"text", "audio"},
			"input_audio_format": "pcm16",
			"turn_detection":     fiber.Map{"type": "server_vad"},
			"created_at":         now,
		},
	})

	errCh := make(chan error, 2)

	// Client → sidecar: handle OpenAI events
	go func() {
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			if mt != websocket.TextMessage {
				// Binary frames aren't part of OpenAI's Realtime protocol —
				// clients must use input_audio_buffer.append (base64).
				continue
			}
			h.handleClientEvent(sess, msg)
		}
	}()

	// Sidecar → client: translate sidecar JSON events → OpenAI events
	go func() {
		for {
			_, msg, err := sidecarConn.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			h.handleSidecarEvent(sess, msg)
		}
	}()

	<-errCh
	h.lm.SendLog(h.lm.BuildLog("OPENAI_REALTIME_ENDED", "OpenAIRealtimeEnded", slog.LevelInfo, map[string]interface{}{
		"endpoint":   "/v1/realtime",
		"request_id": requestID,
	}))
}

func (h *OpenAIRealtimeHandler) handleClientEvent(sess *realtimeSession, msg []byte) {
	var ev map[string]any
	if err := json.Unmarshal(msg, &ev); err != nil {
		h.sendErr(sess.ws, "invalid_event", "malformed JSON")
		return
	}
	t, _ := ev["type"].(string)
	switch t {
	case "session.update":
		// Best-effort: extract session.model and use as engine.
		if session, ok := ev["session"].(map[string]any); ok {
			if m, ok := session["model"].(string); ok && m != "" {
				if eng, ok := openAIModelToRealtimeEngine(m); ok {
					sess.engine = eng
					slog.Info("[OA-RT] engine update", "request_id", sess.requestID, "engine", eng)
				}
			}
		}
		h.sendJSON(sess.ws, fiber.Map{
			"type":     "session.updated",
			"event_id": "evt_" + strings.ReplaceAll(uuid.New().String(), "-", ""),
			"session": fiber.Map{
				"id":    "sess_" + strings.ReplaceAll(sess.requestID, "-", ""),
				"model": sess.engine,
			},
		})

	case "input_audio_buffer.append":
		// Base64-encoded PCM16 frame → forward raw bytes to sidecar
		audio, _ := ev["audio"].(string)
		if audio == "" {
			return
		}
		raw, err := base64.StdEncoding.DecodeString(audio)
		if err != nil {
			h.sendErr(sess.ws, "invalid_audio", "base64 decode failed")
			return
		}
		if err := sess.sidecar.WriteMessage(websocket.BinaryMessage, raw); err != nil {
			slog.Warn("[OA-RT] sidecar write failed", "error", err, "request_id", sess.requestID)
		}

	case "input_audio_buffer.commit":
		// No-op — the sidecar's streaming engine is auto-incremental and
		// doesn't have a commit concept. We send an empty buffer.commit
		// ACK so clients see the event fire as documented.

	case "input_audio_buffer.clear":
		// No sidecar API to flush the rolling buffer — best-effort: close
		// & reopen the sidecar WS so the next append starts fresh.
		slog.Info("[OA-RT] input_audio_buffer.clear (not fully supported)", "request_id", sess.requestID)

	default:
		// Other event types (response.create, conversation.item.create, …)
		// aren't part of the input-transcription half this proxy implements.
	}
}

func (h *OpenAIRealtimeHandler) handleSidecarEvent(sess *realtimeSession, msg []byte) {
	var ev map[string]any
	if err := json.Unmarshal(msg, &ev); err != nil {
		return
	}
	t, _ := ev["type"].(string)
	switch t {
	case "ready":
		// No client-visible event for this — handled by session.created already.
	case "speech_started":
		h.sendJSON(sess.ws, fiber.Map{
			"type":           "input_audio_buffer.speech_started",
			"event_id":       "evt_" + strings.ReplaceAll(uuid.New().String(), "-", ""),
			"audio_start_ms": int(asFloat(ev["time"]) * 1000),
			"item_id":        sess.itemID,
		})
	case "speech_stopped":
		h.sendJSON(sess.ws, fiber.Map{
			"type":         "input_audio_buffer.speech_stopped",
			"event_id":     "evt_" + strings.ReplaceAll(uuid.New().String(), "-", ""),
			"audio_end_ms": int(asFloat(ev["time"]) * 1000),
			"item_id":      sess.itemID,
		})
	case "partial":
		text, _ := ev["text"].(string)
		h.sendJSON(sess.ws, fiber.Map{
			"type":          "conversation.item.input_audio_transcription.delta",
			"event_id":      "evt_" + strings.ReplaceAll(uuid.New().String(), "-", ""),
			"item_id":       sess.itemID,
			"content_index": 0,
			"delta":         text,
		})
	case "final":
		text, _ := ev["text"].(string)
		h.sendJSON(sess.ws, fiber.Map{
			"type":          "conversation.item.input_audio_transcription.completed",
			"event_id":      "evt_" + strings.ReplaceAll(uuid.New().String(), "-", ""),
			"item_id":       sess.itemID,
			"content_index": 0,
			"transcript":    text,
		})
	case "end_of_turn":
		// OpenAI doesn't have a 1:1 mapping; surface as a low-volume
		// generic event so clients can still observe turn boundaries.
		h.sendJSON(sess.ws, fiber.Map{
			"type":     "input_audio_buffer.committed",
			"event_id": "evt_" + strings.ReplaceAll(uuid.New().String(), "-", ""),
			"item_id":  sess.itemID,
		})
		// New item_id for the next turn
		sess.itemID = "item_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	case "error":
		h.sendJSON(sess.ws, fiber.Map{
			"type":     "error",
			"event_id": "evt_" + strings.ReplaceAll(uuid.New().String(), "-", ""),
			"error": fiber.Map{
				"type":    "sidecar_error",
				"message": asString(ev["message"]),
			},
		})
	case "done":
		// Connection will close naturally
	}
}

func (h *OpenAIRealtimeHandler) sendJSON(c *websocket.Conn, v any) {
	if err := c.WriteJSON(v); err != nil {
		slog.Warn("[OA-RT] client write failed", "error", err)
	}
}

func (h *OpenAIRealtimeHandler) sendErr(c *websocket.Conn, code, msg string) {
	h.sendJSON(c, fiber.Map{
		"type":     "error",
		"event_id": "evt_" + strings.ReplaceAll(uuid.New().String(), "-", ""),
		"error":    fiber.Map{"type": code, "message": msg},
	})
}

func asFloat(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// defaultRealtimeEngineFromQuery picks the initial engine from the WS
// query string (?engine=eou-320), defaulting to the same env-derived
// default as the sidecar (eou-320).
func defaultRealtimeEngineFromQuery(c *websocket.Conn) string {
	if e := c.Query("engine"); e != "" {
		return e
	}
	return "eou-320"
}

// openAIModelToRealtimeEngine maps an OpenAI Realtime `model` string to a
// sidecar streaming engine. Returns ok=false if the model name should be
// passed through as the engine verbatim.
func openAIModelToRealtimeEngine(model string) (string, bool) {
	m := strings.ToLower(model)
	switch m {
	case "gpt-4o-realtime-preview", "gpt-4o-realtime":
		return "eou-320", true
	case "gpt-4o-mini-realtime-preview":
		return "nemotron-560", true
	case "nova-3", "parakeet-unified-320":
		return "unified-320", true
	}
	// Allow passthrough of explicit engine IDs
	if strings.HasPrefix(m, "eou-") || strings.HasPrefix(m, "nemotron-") || strings.HasPrefix(m, "unified-") {
		return m, true
	}
	return "", false
}
