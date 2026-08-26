package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ws "github.com/fasthttp/websocket"
	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/shaunagostinho/gotranscribesrv/internal/config"
	"github.com/shaunagostinho/gotranscribesrv/internal/logging"
	"github.com/shaunagostinho/gotranscribesrv/internal/metrics"
	"github.com/shaunagostinho/gotranscribesrv/internal/middleware"
	"github.com/shaunagostinho/gotranscribesrv/internal/pii"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
	"gorm.io/gorm"
)

// OpenAIRealtimeS2SHandler implements the speech-to-speech half of the
// OpenAI Realtime API on WS /v1/realtime: audio in → streaming ASR →
// streaming LLM → sentence-chunked streaming TTS → audio out, with
// turn-taking, barge-in, and client-side tool calling. Full spec:
// docs/realtime.md.
//
// Session selection happens at connect time (mirroring OpenAI, whose SDK
// passes the model in the URL): clients connect with
// ?model=gpt-realtime* to get an S2S session; anything else lands on the
// transcription-only proxy (openai_realtime.go). S2S is gated by
// REALTIME_S2S_ENABLED.
//
// Protocol deltas vs. the transcription proxy:
//
//	Client → server (additional): response.create, response.cancel,
//		conversation.item.create (function_call_output / message items)
//	Server → client (additional): response.*, conversation.item.created /
//		.truncated, response.function_call_arguments.*
//
// Tools are pass-through: the server never executes them — the LLM's
// tool_calls are relayed to the client, which executes and returns a
// function_call_output item, then response.create resumes the loop.
type OpenAIRealtimeS2SHandler struct {
	sc       *sidecar.Client
	redactor *pii.Redactor
	lm       *logging.LogManager
	db       *gorm.DB
	cfg      *config.Config
}

// NewOpenAIRealtimeS2SHandler constructs the handler.
func NewOpenAIRealtimeS2SHandler(sc *sidecar.Client, redactor *pii.Redactor, lm *logging.LogManager, db *gorm.DB, cfg *config.Config) *OpenAIRealtimeS2SHandler {
	return &OpenAIRealtimeS2SHandler{sc: sc, redactor: redactor, lm: lm, db: db, cfg: cfg}
}

// IsS2SModel reports whether an OpenAI Realtime model name selects a
// speech-to-speech session. gpt-realtime* → S2S; everything else
// (gpt-4o-transcribe, gpt-4o-realtime*, engine IDs, empty) → transcription.
func IsS2SModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "gpt-realtime")
}

// Upgrade returns the Fiber middleware that upgrades HTTP to WebSocket.
func (h *OpenAIRealtimeS2SHandler) Upgrade() fiber.Handler {
	return websocket.New(h.handle)
}

// s2sSession is the per-connection state. All client writes are serialized
// through writeMu (ASR goroutine + response goroutine both emit events).
type s2sSession struct {
	requestID string
	sessionID string
	ws        *websocket.Conn
	writeMu   sync.Mutex

	sidecar *ws.Conn // ASR streaming connection
	engine  string
	model   string // client-facing model name (?model= at connect)

	// Session config (session.update)
	instructions  string
	voice         string
	textOnly      bool // modalities excludes "audio"
	tools         []json.RawMessage
	toolChoice    any
	turnDetection string // "server_vad" (default) | "none"
	temperature   float64
	maxTokens     int

	// Conversation history (guarded by histMu — appended on the client
	// goroutine, snapshotted at response start on the response goroutine).
	histMu  sync.Mutex
	history []sidecar.ChatMessage

	// Response lifecycle. gen increments on every response start AND every
	// interrupt; emitters check their gen is still current before writing.
	responding atomic.Bool
	gen        atomic.Int64
	cancelResp context.CancelFunc
	turnStart  time.Time // end-of-speech time, for turn latency

	// Item IDs
	curItemID     string // current user input item
	prevItemID    string // last completed item (previous_item_id in events)
	respItemID    string // current assistant output item
	curResponseID string // current response (turn correlation ID for logs)

	// Usage counters
	audioInBytes     int64 // PCM16 16kHz → 32 bytes/ms
	audioOutBytes    int64 // PCM16 24kHz → 48 bytes/ms
	respAudioBytes   int64 // per-response, reset each turn (for truncation)
	promptTokens     int
	completionTokens int
	turns            int
	interruptions    int
	toolCalls        int
	firstAudioAt     time.Time
	lastActivityAt   time.Time

	userID   string
	apiKeyID string
}

func rtID(prefix string) string {
	return prefix + "_" + strings.ReplaceAll(uuid.New().String(), "-", "")
}

func (s *s2sSession) send(v any) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ws.WriteJSON(v); err != nil {
		slog.Warn("[OA-RT-S2S] client write failed", "error", err, "request_id", s.requestID)
	}
}

// sendIfCurrent writes v only if the response generation is still current —
// after a barge-in (gen bump) a cancelled response goroutine stops emitting.
func (s *s2sSession) sendIfCurrent(gen int64, v any) bool {
	if s.gen.Load() != gen {
		return false
	}
	s.send(v)
	return true
}

// sendErr emits a spec-shaped error event: error.type is the broad class
// ("invalid_request_error" for client mistakes, "server_error" for upstream
// failures), error.code carries our specific machine-readable code.
func (s *s2sSession) sendErr(class, code, msg string) {
	s.send(fiber.Map{
		"type":     "error",
		"event_id": rtID("evt"),
		"error": fiber.Map{
			"type":     class,
			"code":     code,
			"message":  msg,
			"param":    nil,
			"event_id": nil, // client event that caused it, when known
		},
	})
}

func (s *s2sSession) appendHistory(msg sidecar.ChatMessage) {
	s.histMu.Lock()
	defer s.histMu.Unlock()
	s.history = append(s.history, msg)
}

func (s *s2sSession) historySnapshot() []sidecar.ChatMessage {
	s.histMu.Lock()
	defer s.histMu.Unlock()
	out := make([]sidecar.ChatMessage, len(s.history))
	copy(out, s.history)
	return out
}

func (h *OpenAIRealtimeS2SHandler) handle(c *websocket.Conn) {
	defer c.Close()
	c.SetReadLimit(8 * 1024 * 1024)

	requestID, _ := c.Locals(middleware.RequestIDLocalKey).(string)
	if requestID == "" {
		requestID = uuid.New().String()
		c.Locals(middleware.RequestIDLocalKey, requestID)
	}

	engine := s2sEngineFromQuery(c)

	sidecarURL := h.sc.RealtimeStreamURL(engine)
	u, err := url.Parse(sidecarURL)
	if err != nil {
		h.sendErrFatal(c, "invalid_stream_url", "internal configuration error")
		return
	}
	q := u.Query()
	for _, p := range []string{"encoding", "sample_rate", "itn", "vad"} {
		if v := c.Query(p); v != "" {
			q.Set(p, v)
		}
	}
	u.RawQuery = q.Encode()

	sidecarConn, _, err := ws.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		h.sendErrFatal(c, "sidecar_unavailable", "transcription service unavailable")
		return
	}
	defer sidecarConn.Close()
	metrics.ActiveWebSocketConnections.WithLabelValues("openai_realtime_s2s").Inc()
	defer metrics.ActiveWebSocketConnections.WithLabelValues("openai_realtime_s2s").Dec()

	userID, _ := c.Locals("user_id").(string)
	apiKeyID, _ := c.Locals("api_key_id").(string)

	sess := &s2sSession{
		requestID:     requestID,
		sessionID:     rtID("sess"),
		ws:            c,
		sidecar:       sidecarConn,
		engine:        engine,
		model:         c.Query("model", "gpt-realtime"),
		voice:         h.cfg.RealtimeS2SVoice,
		turnDetection: "server_vad",
		temperature:   h.cfg.RealtimeS2STemperature,
		maxTokens:     h.cfg.RealtimeS2SMaxTokens,
		curItemID:     rtID("item"),
		userID:        userID,
		apiKeyID:      apiKeyID,
	}

	slog.Info("[OA-RT-S2S] Session started", "request_id", requestID, "engine", engine,
		"llm_model", h.cfg.RealtimeS2SModel, "voice", sess.voice)
	h.lm.SendLog(h.lm.BuildLog("OPENAI_REALTIME_S2S_STARTED", "OpenAIRealtimeS2SStarted", slog.LevelInfo, map[string]interface{}{
		"endpoint":   "/v1/realtime",
		"mode":       "speech_to_speech",
		"ip":         c.IP(),
		"request_id": requestID,
		"engine":     engine,
		"llm_model":  h.cfg.RealtimeS2SModel,
	}))

	// session.created (immediately, per OpenAI spec)
	sess.send(fiber.Map{
		"type":     "session.created",
		"event_id": rtID("evt"),
		"session":  h.sessionPayload(sess),
	})

	errCh := make(chan error, 2)

	// Client → server: OpenAI Realtime events
	go func() {
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			if mt != websocket.TextMessage {
				continue // audio must arrive as input_audio_buffer.append (base64)
			}
			h.handleClientEvent(sess, msg)
		}
	}()

	// Sidecar → server: ASR stream events
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

	// Cancel any in-flight response and log usage.
	if sess.cancelResp != nil {
		sess.cancelResp()
	}
	sess.gen.Add(1) // silence any final emissions from the dying goroutine

	audioInMs := int(sess.audioInBytes / 32)
	audioOutMs := int(sess.audioOutBytes / 48)
	processMs := 0
	if !sess.firstAudioAt.IsZero() && !sess.lastActivityAt.IsZero() {
		processMs = int(sess.lastActivityAt.Sub(sess.firstAudioAt).Milliseconds())
	}
	middleware.LogWebSocketUsage(h.db, sess.userID, sess.apiKeyID, "realtime_s2s",
		audioInMs+audioOutMs, processMs, false)
	if sess.promptTokens+sess.completionTokens > 0 {
		middleware.LogLLMUsage(h.db, sess.userID, sess.apiKeyID, "realtime_s2s",
			h.cfg.RealtimeS2SModel, sess.promptTokens, sess.completionTokens, processMs, true)
		metrics.RecordLLMUsage("realtime_s2s", h.cfg.RealtimeS2SModel,
			sess.promptTokens, sess.completionTokens, processMs)
	}

	slog.Info("[OA-RT-S2S] Session ended", "request_id", requestID,
		"turns", sess.turns, "interruptions", sess.interruptions, "tool_calls", sess.toolCalls,
		"audio_in_ms", audioInMs, "audio_out_ms", audioOutMs,
		"prompt_tokens", sess.promptTokens, "completion_tokens", sess.completionTokens)

	h.lm.SendLog(h.lm.BuildLog("OPENAI_REALTIME_S2S_ENDED", "OpenAIRealtimeS2SEnded", slog.LevelInfo, map[string]interface{}{
		"endpoint":          "/v1/realtime",
		"mode":              "speech_to_speech",
		"ip":                c.IP(),
		"request_id":        requestID,
		"turns":             sess.turns,
		"interruptions":     sess.interruptions,
		"tool_calls":        sess.toolCalls,
		"audio_in_ms":       audioInMs,
		"audio_out_ms":      audioOutMs,
		"prompt_tokens":     sess.promptTokens,
		"completion_tokens": sess.completionTokens,
		"process_ms":        processMs,
	}))
}

func (h *OpenAIRealtimeS2SHandler) sendErrFatal(c *websocket.Conn, code, msg string) {
	_ = c.WriteJSON(fiber.Map{
		"type":     "error",
		"event_id": rtID("evt"),
		"error": fiber.Map{
			"type":     "server_error",
			"code":     code,
			"message":  msg,
			"param":    nil,
			"event_id": nil,
		},
	})
}

// sessionPayload builds the spec-shaped realtime.session object echoed in
// session.created / session.updated. The session `model` field carries the
// client-facing model name from the connect URL.
func (h *OpenAIRealtimeS2SHandler) sessionPayload(sess *s2sSession) fiber.Map {
	maxTokens := any(sess.maxTokens)
	if sess.maxTokens <= 0 {
		maxTokens = "inf"
	}
	toolChoice := sess.toolChoice
	if toolChoice == nil {
		toolChoice = "auto"
	}
	tools := sess.tools
	if tools == nil {
		tools = []json.RawMessage{}
	}
	modalities := []string{"text", "audio"}
	if sess.textOnly {
		modalities = []string{"text"}
	}
	return fiber.Map{
		"id":                  sess.sessionID,
		"object":              "realtime.session",
		"model":               sess.model,
		"modalities":          modalities,
		"instructions":        sess.instructions,
		"voice":               sess.voice,
		"input_audio_format":  "pcm16",
		"output_audio_format": "pcm16",
		"input_audio_transcription": fiber.Map{
			"model": sess.engine, // our ASR engine fills Whisper's role
		},
		"turn_detection":             fiber.Map{"type": sess.turnDetection},
		"tools":                      tools,
		"tool_choice":                toolChoice,
		"temperature":                sess.temperature,
		"max_response_output_tokens": maxTokens,
	}
}

// ──────────────────────────────────────────────────────────────
// Client → server events
// ──────────────────────────────────────────────────────────────

func (h *OpenAIRealtimeS2SHandler) handleClientEvent(sess *s2sSession, msg []byte) {
	var ev map[string]any
	if err := json.Unmarshal(msg, &ev); err != nil {
		sess.sendErr("invalid_request_error", "invalid_event", "malformed JSON")
		return
	}
	t, _ := ev["type"].(string)
	switch t {
	case "session.update":
		h.handleSessionUpdate(sess, ev)

	case "input_audio_buffer.append":
		audio, _ := ev["audio"].(string)
		if audio == "" {
			return
		}
		raw, err := base64.StdEncoding.DecodeString(audio)
		if err != nil {
			sess.sendErr("invalid_request_error", "invalid_audio", "base64 decode failed")
			return
		}
		atomic.AddInt64(&sess.audioInBytes, int64(len(raw)))
		if sess.firstAudioAt.IsZero() {
			sess.firstAudioAt = time.Now()
		}
		if err := sess.sidecar.WriteMessage(websocket.BinaryMessage, raw); err != nil {
			slog.Warn("[OA-RT-S2S] sidecar write failed", "error", err, "request_id", sess.requestID)
		}

	case "input_audio_buffer.commit":
		// Ack-only — the streaming engine is auto-incremental.
		sess.send(fiber.Map{
			"type":             "input_audio_buffer.committed",
			"event_id":         rtID("evt"),
			"previous_item_id": sess.prevItemID,
			"item_id":          sess.curItemID,
		})

	case "input_audio_buffer.clear":
		// No sidecar flush API — matches the transcription proxy.
		slog.Info("[OA-RT-S2S] input_audio_buffer.clear (no-op)", "request_id", sess.requestID)

	case "response.create":
		// Forced response: tool follow-up, text turn, or push-to-talk
		// (turn_detection=none) flow.
		sess.turnStart = time.Now()
		h.startResponse(sess)

	case "response.cancel":
		h.interrupt(sess, "client_cancel")

	case "conversation.item.create":
		h.handleItemCreate(sess, ev)

	default:
		// Unknown events are ignored per spec tolerance.
	}
}

func (h *OpenAIRealtimeS2SHandler) handleSessionUpdate(sess *s2sSession, ev map[string]any) {
	session, _ := ev["session"].(map[string]any)
	if session == nil {
		return
	}
	if v, ok := session["instructions"].(string); ok {
		sess.instructions = v
	}
	if v, ok := session["voice"].(string); ok && v != "" {
		sess.voice = v
	}
	if v, ok := session["modalities"].([]any); ok && len(v) > 0 {
		sess.textOnly = true
		for _, m := range v {
			if s, _ := m.(string); s == "audio" {
				sess.textOnly = false
			}
		}
	}
	if v, ok := session["tools"].([]any); ok {
		sess.tools = sess.tools[:0]
		for _, tool := range v {
			raw, err := json.Marshal(tool)
			if err == nil {
				sess.tools = append(sess.tools, raw)
			}
		}
	}
	if v, ok := session["tool_choice"]; ok {
		sess.toolChoice = v
	}
	if td, ok := session["turn_detection"].(map[string]any); ok {
		if v, _ := td["type"].(string); v == "none" || v == "server_vad" {
			sess.turnDetection = v
		}
	}
	if v, ok := session["temperature"].(float64); ok && v > 0 {
		sess.temperature = v
	}
	switch v := session["max_response_output_tokens"].(type) {
	case float64:
		if v > 0 {
			sess.maxTokens = int(v)
		}
	case string:
		// "inf" → leave the server default.
	}

	sess.send(fiber.Map{
		"type":     "session.updated",
		"event_id": rtID("evt"),
		"session":  h.sessionPayload(sess),
	})
}

func (h *OpenAIRealtimeS2SHandler) handleItemCreate(sess *s2sSession, ev map[string]any) {
	item, _ := ev["item"].(map[string]any)
	if item == nil {
		sess.sendErr("invalid_request_error", "invalid_event", "conversation.item.create requires an item")
		return
	}
	itemType, _ := item["type"].(string)
	itemID, _ := item["id"].(string)
	if itemID == "" {
		itemID = rtID("item")
	}

	switch itemType {
	case "function_call_output":
		callID, _ := item["call_id"].(string)
		output, _ := item["output"].(string)
		if callID == "" {
			sess.sendErr("invalid_request_error", "invalid_event", "function_call_output requires call_id")
			return
		}
		sess.appendHistory(sidecar.ChatMessage{Role: "tool", ToolCallID: callID, Content: output})
		slog.Info("[OA-RT-S2S] tool result received", "request_id", sess.requestID, "call_id", callID, "output_bytes", len(output))

	case "message":
		// Text turn from the client: content: [{type:"input_text", text:"..."}]
		role, _ := item["role"].(string)
		if role == "" {
			role = "user"
		}
		text := extractMessageText(item)
		if text == "" {
			sess.sendErr("invalid_request_error", "invalid_event", "message item has no text content")
			return
		}
		sess.appendHistory(sidecar.ChatMessage{Role: role, Content: text})
		sess.send(fiber.Map{
			"type":          "conversation.item.input_audio_transcription.completed",
			"event_id":      rtID("evt"),
			"item_id":       itemID,
			"content_index": 0,
			"transcript":    text,
		})

	default:
		sess.sendErr("invalid_request_error", "invalid_event", "unsupported item type: "+itemType)
		return
	}

	sess.send(fiber.Map{
		"type":             "conversation.item.created",
		"event_id":         rtID("evt"),
		"previous_item_id": sess.prevItemID,
		"item": fiber.Map{
			"id": itemID, "object": "realtime.item",
			"type": itemType, "status": "completed",
		},
	})
	sess.prevItemID = itemID
}

func extractMessageText(item map[string]any) string {
	content, _ := item["content"].([]any)
	var b strings.Builder
	for _, part := range content {
		p, _ := part.(map[string]any)
		if t, _ := p["text"].(string); t != "" {
			b.WriteString(t)
		}
	}
	return b.String()
}

// ──────────────────────────────────────────────────────────────
// Sidecar → server ASR events
// ──────────────────────────────────────────────────────────────

func (h *OpenAIRealtimeS2SHandler) handleSidecarEvent(sess *s2sSession, msg []byte) {
	var ev map[string]any
	if err := json.Unmarshal(msg, &ev); err != nil {
		return
	}
	t, _ := ev["type"].(string)
	switch t {
	case "ready":
		// Already covered by session.created.

	case "speech_started":
		sess.send(fiber.Map{
			"type":           "input_audio_buffer.speech_started",
			"event_id":       rtID("evt"),
			"audio_start_ms": int(asFloat(ev["time"]) * 1000),
			"item_id":        sess.curItemID,
		})
		// Barge-in: user speech cancels an in-flight response.
		if h.cfg.RealtimeS2SInterruptions && sess.responding.Load() {
			h.interrupt(sess, "barge_in")
		}

	case "speech_stopped":
		sess.send(fiber.Map{
			"type":         "input_audio_buffer.speech_stopped",
			"event_id":     rtID("evt"),
			"audio_end_ms": int(asFloat(ev["time"]) * 1000),
			"item_id":      sess.curItemID,
		})

	case "partial":
		text, _ := ev["text"].(string)
		sess.send(fiber.Map{
			"type":          "conversation.item.input_audio_transcription.delta",
			"event_id":      rtID("evt"),
			"item_id":       sess.curItemID,
			"content_index": 0,
			"delta":         text,
		})

	case "final":
		text, _ := ev["text"].(string)
		sess.lastActivityAt = time.Now()
		speechFinal, _ := ev["speech_final"].(bool)
		if speechFinal && strings.TrimSpace(text) != "" {
			// Spec ordering at turn end: buffer committed → user item
			// created → transcription completed → (response.created next
			// from startResponse). The sidecar's trailing end_of_turn
			// marker only rotates the item ID below.
			sess.send(fiber.Map{
				"type":             "input_audio_buffer.committed",
				"event_id":         rtID("evt"),
				"previous_item_id": sess.prevItemID,
				"item_id":          sess.curItemID,
			})
			sess.send(fiber.Map{
				"type":             "conversation.item.created",
				"event_id":         rtID("evt"),
				"previous_item_id": sess.prevItemID,
				"item": fiber.Map{
					"id": sess.curItemID, "object": "realtime.item", "type": "message",
					"status": "completed", "role": "user",
					"content": []any{fiber.Map{"type": "input_audio", "transcript": text}},
				},
			})
		}
		sess.send(fiber.Map{
			"type":          "conversation.item.input_audio_transcription.completed",
			"event_id":      rtID("evt"),
			"item_id":       sess.curItemID,
			"content_index": 0,
			"transcript":    text,
		})
		// Response-sent event → Loki. Transcript is PII-redacted; the raw
		// text above goes to the client untouched.
		redactedFinal, piiItems, piiErr := h.redactor.RedactText(context.Background(), text)
		if piiErr != nil {
			h.lm.SendLog(h.lm.BuildLog("PII_REDACTOR_ERROR", "PIIRedactorError", slog.LevelWarn, map[string]interface{}{
				"endpoint":   "/v1/realtime",
				"ip":         sess.ws.IP(),
				"text_len":   len(text),
				"request_id": sess.requestID,
			}, piiErr))
		}
		transcriptFields := map[string]interface{}{
			"endpoint":     "/v1/realtime",
			"ip":           sess.ws.IP(),
			"request_id":   sess.requestID,
			"engine":       sess.engine,
			"transcript":   logging.Redacted(redactedFinal),
			"pii_redacted": len(piiItems),
			"is_final":     true,
			"speech_final": speechFinal,
		}
		if len(piiItems) > 0 {
			transcriptFields["pii_entity_types"] = piiEntityTypes(piiItems)
		}
		h.lm.SendLog(h.lm.BuildLog("OPENAI_REALTIME_S2S_TRANSCRIPT_SENT", "OpenAIRealtimeS2STranscriptSent", slog.LevelInfo, transcriptFields))
		// Turn end (EOU or VAD speech_final) → LLM response, unless the
		// client disabled turn detection or a response is already running.
		if speechFinal && sess.turnDetection != "none" && strings.TrimSpace(text) != "" {
			sess.appendHistory(sidecar.ChatMessage{Role: "user", Content: text})
			sess.turnStart = time.Now()
			h.startResponse(sess)
		}

	case "end_of_turn":
		// Turn marker arrives after the final — rotate the item ID so the
		// next turn gets a fresh one.
		sess.prevItemID = sess.curItemID
		sess.curItemID = rtID("item")

	case "error":
		sess.sendErr("server_error", "sidecar_error", asString(ev["message"]))

	case "done":
		// Connection closes naturally.
	}
}

// ──────────────────────────────────────────────────────────────
// Response pipeline: LLM stream → sentence split → TTS stream
// ──────────────────────────────────────────────────────────────

func (h *OpenAIRealtimeS2SHandler) startResponse(sess *s2sSession) {
	if !sess.responding.CompareAndSwap(false, true) {
		return // a response is already in flight
	}
	gen := sess.gen.Add(1)
	ctx, cancel := context.WithCancel(context.Background())
	sess.cancelResp = cancel
	go h.runResponse(ctx, sess, gen)
}

// interrupt cancels the in-flight response (barge-in or response.cancel).
func (h *OpenAIRealtimeS2SHandler) interrupt(sess *s2sSession, reason string) {
	if !sess.responding.Load() {
		return
	}
	sess.gen.Add(1) // invalidate in-flight emissions first
	if sess.cancelResp != nil {
		sess.cancelResp()
	}
	sess.responding.Store(false)
	sess.interruptions++

	audioEndMs := int(sess.respAudioBytes / 48) // 24kHz 16-bit mono
	sess.send(fiber.Map{
		"type":          "conversation.item.truncated",
		"event_id":      rtID("evt"),
		"item_id":       sess.respItemID,
		"content_index": 0,
		"audio_end_ms":  audioEndMs,
	})
	// Spec: interruption ends the response with response.done carrying
	// status "cancelled" + status_details.reason. The older
	// response.cancelled event is also emitted for SDK compatibility.
	cancelReason := "turn_detected"
	if reason == "client_cancel" {
		cancelReason = "client_cancelled"
	}
	cancelledResp := fiber.Map{
		"id":     sess.curResponseID,
		"object": "realtime.response",
		"status": "cancelled",
		"status_details": fiber.Map{
			"type":   "cancelled",
			"reason": cancelReason,
		},
		"output": []any{},
	}
	sess.send(fiber.Map{
		"type":     "response.cancelled",
		"event_id": rtID("evt"),
		"response": cancelledResp,
	})
	sess.send(fiber.Map{
		"type":     "response.done",
		"event_id": rtID("evt"),
		"response": cancelledResp,
	})

	metrics.RecordRealtimeS2SInterruption()
	slog.Info("[OA-RT-S2S] response interrupted", "request_id", sess.requestID, "turn_id", sess.curResponseID, "reason", reason,
		"audio_end_ms", audioEndMs)
	h.lm.SendLog(h.lm.BuildLog("OPENAI_REALTIME_S2S_INTERRUPTION", "OpenAIRealtimeS2SInterruption", slog.LevelInfo, map[string]interface{}{
		"endpoint":     "/v1/realtime",
		"request_id":   sess.requestID,
		"turn_id":      sess.curResponseID,
		"reason":       reason,
		"audio_end_ms": audioEndMs,
	}))
}

func (h *OpenAIRealtimeS2SHandler) runResponse(ctx context.Context, sess *s2sSession, gen int64) {
	defer sess.responding.Store(false)

	responseID := rtID("resp")
	sess.respItemID = rtID("item")
	sess.curResponseID = responseID
	sess.respAudioBytes = 0
	turnBegin := time.Now()

	emptyPart := fiber.Map{"type": "audio", "transcript": ""}
	if sess.textOnly {
		emptyPart = fiber.Map{"type": "text", "text": ""}
	}
	sess.sendIfCurrent(gen, fiber.Map{
		"type":     "response.created",
		"event_id": rtID("evt"),
		"response": fiber.Map{
			"id":             responseID,
			"object":         "realtime.response",
			"status":         "in_progress",
			"status_details": nil,
			"output":         []any{},
			"usage":          nil,
		},
	})
	sess.sendIfCurrent(gen, fiber.Map{
		"type":         "response.output_item.added",
		"event_id":     rtID("evt"),
		"response_id":  responseID,
		"output_index": 0,
		"item": fiber.Map{
			"id": sess.respItemID, "object": "realtime.item", "type": "message",
			"role": "assistant", "status": "in_progress", "content": []any{},
		},
	})
	sess.sendIfCurrent(gen, fiber.Map{
		"type":          "response.content_part.added",
		"event_id":      rtID("evt"),
		"response_id":   responseID,
		"item_id":       sess.respItemID,
		"output_index":  0,
		"content_index": 0,
		"part":          emptyPart,
	})

	// Build the LLM request from the session config + history snapshot.
	messages := make([]sidecar.ChatMessage, 0, 16)
	if sess.instructions != "" {
		messages = append(messages, sidecar.ChatMessage{Role: "system", Content: sess.instructions})
	}
	messages = append(messages, sess.historySnapshot()...)

	stream, err := h.sc.StreamChat(ctx, sidecar.ChatCompletionRequest{
		Model:       h.cfg.RealtimeS2SModel,
		Messages:    messages,
		Tools:       sess.tools,
		ToolChoice:  sess.toolChoice,
		Temperature: sess.temperature,
		MaxTokens:   sess.maxTokens,
	}, responseID)
	if err != nil {
		if gen == sess.gen.Load() {
			slog.Warn("[OA-RT-S2S] LLM stream failed", "error", err, "request_id", sess.requestID, "turn_id", responseID)
			sess.sendErr("server_error", "llm_unavailable", "language model unavailable: "+err.Error())
		}
		return
	}
	slog.Debug("[OA-RT-S2S] LLM stream started", "request_id", sess.requestID, "turn_id", responseID,
		"history_messages", len(messages), "tools", len(sess.tools))

	splitter := newSentenceSplitter()
	var fullText strings.Builder
	toolAccum := make(map[int]*sidecar.ChatToolCall)
	var finishReason string
	var usage *sidecar.ChatUsage
	var ttftRecorded bool
	var firstAudioRecorded bool
	ttsStats := &s2sTTSStats{}

	for chunk := range stream {
		if sess.gen.Load() != gen || ctx.Err() != nil {
			return // interrupted — stay silent
		}
		if chunk.Err != nil {
			slog.Warn("[OA-RT-S2S] LLM stream error", "error", chunk.Err, "request_id", sess.requestID, "turn_id", responseID)
			sess.sendErr("server_error", "llm_error", chunk.Err.Error())
			return
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if chunk.FinishReason != "" {
			finishReason = chunk.FinishReason
		}
		for _, frag := range chunk.ToolCalls {
			acc, ok := toolAccum[frag.Index]
			if !ok {
				acc = &sidecar.ChatToolCall{Index: frag.Index, Type: "function"}
				toolAccum[frag.Index] = acc
			}
			if frag.ID != "" {
				acc.ID = frag.ID
			}
			if frag.Function.Name != "" {
				acc.Function.Name = frag.Function.Name
			}
			acc.Function.Arguments += frag.Function.Arguments
		}
		if chunk.Content == "" {
			continue
		}
		if !ttftRecorded {
			metrics.RecordRealtimeS2STTFT(h.cfg.RealtimeS2SModel, time.Since(turnBegin).Seconds())
			ttftRecorded = true
		}
		fullText.WriteString(chunk.Content)
		splitter.Write(chunk.Content)

		// Speak every complete sentence as soon as it's ready.
		for {
			sentence, ok := splitter.Next()
			if !ok {
				break
			}
			if !h.speakSentence(ctx, sess, gen, responseID, sentence, &firstAudioRecorded, ttsStats) {
				return // interrupted or TTS failed
			}
		}
	}

	// Flush any trailing text.
	if tail := splitter.Flush(); tail != "" {
		if !h.speakSentence(ctx, sess, gen, responseID, tail, &firstAudioRecorded, ttsStats) {
			return
		}
	}
	if sess.gen.Load() != gen {
		return // interrupted during the final sentence
	}

	if usage != nil {
		sess.promptTokens += usage.PromptTokens
		sess.completionTokens += usage.CompletionTokens
	}

	// ── Tool-call turn: relay to the client, no audio. ──────────
	if len(toolAccum) > 0 || finishReason == "tool_calls" {
		h.finishToolCallTurn(sess, responseID, toolAccum)
		return
	}

	// ── Normal completion. ──────────────────────────────────────
	text := fullText.String()
	sess.appendHistory(sidecar.ChatMessage{Role: "assistant", Content: text})
	sess.turns++
	sess.lastActivityAt = time.Now()

	donePart := fiber.Map{"type": "output_audio", "transcript": text}
	if sess.textOnly {
		donePart = fiber.Map{"type": "text", "text": text}
	}
	doneItem := fiber.Map{
		"id": sess.respItemID, "object": "realtime.item", "type": "message",
		"role": "assistant", "status": "completed", "content": []any{donePart},
	}

	sess.sendIfCurrent(gen, fiber.Map{"type": "response.audio.done", "event_id": rtID("evt"),
		"response_id": responseID, "item_id": sess.respItemID, "output_index": 0, "content_index": 0})
	sess.sendIfCurrent(gen, fiber.Map{"type": "response.audio_transcript.done", "event_id": rtID("evt"),
		"response_id": responseID, "item_id": sess.respItemID, "output_index": 0, "content_index": 0,
		"transcript": text})
	sess.sendIfCurrent(gen, fiber.Map{"type": "response.content_part.done", "event_id": rtID("evt"),
		"response_id": responseID, "item_id": sess.respItemID, "output_index": 0, "content_index": 0,
		"part": donePart})
	sess.sendIfCurrent(gen, fiber.Map{"type": "response.output_item.done", "event_id": rtID("evt"),
		"response_id": responseID, "output_index": 0, "item": doneItem})

	respObj := fiber.Map{
		"id":             responseID,
		"object":         "realtime.response",
		"status":         "completed",
		"status_details": nil,
		"output":         []any{doneItem},
	}
	if usage != nil {
		respObj["usage"] = fiber.Map{
			"input_tokens":  usage.PromptTokens,
			"output_tokens": usage.CompletionTokens,
			"total_tokens":  usage.TotalTokens,
		}
	}
	sess.sendIfCurrent(gen, fiber.Map{
		"type":     "response.done",
		"event_id": rtID("evt"),
		"response": respObj,
	})

	turnLatency := time.Since(sess.turnStart).Seconds()
	metrics.RecordRealtimeS2STurn(sess.engine, turnLatency)
	slog.Info("[OA-RT-S2S] turn completed", "request_id", sess.requestID, "turn_id", responseID,
		"turn_latency_ms", int(turnLatency*1000), "response_chars", len(text),
		"audio_out_ms", int(sess.respAudioBytes/48))
	h.lm.SendLog(h.lm.BuildLog("OPENAI_REALTIME_S2S_TURN_COMPLETED", "OpenAIRealtimeS2STurnCompleted", slog.LevelInfo, map[string]interface{}{
		"endpoint":           "/v1/realtime",
		"request_id":         sess.requestID,
		"turn_id":            responseID,
		"turn":               sess.turns,
		"turn_latency_ms":    int(turnLatency * 1000),
		"response_chars":     len(text),
		"audio_out_ms":       int(sess.respAudioBytes / 48),
		"llm_model":          h.cfg.RealtimeS2SModel,
		"voice":              sess.voice,
		"tts_sentences":      ttsStats.sentences,
		"tts_total_synth_ms": int(ttsStats.totalSynthTime.Milliseconds()),
		"tts_first_chunk_ms": int(ttsStats.firstChunkLatency.Milliseconds()),
	}))
}

// finishToolCallTurn relays accumulated tool calls to the client (the server
// never executes them — the client runs the tool and replies with a
// function_call_output item + response.create).
func (h *OpenAIRealtimeS2SHandler) finishToolCallTurn(sess *s2sSession, responseID string, toolAccum map[int]*sidecar.ChatToolCall) {
	// Order by index for deterministic output.
	indices := make([]int, 0, len(toolAccum))
	for i := range toolAccum {
		indices = append(indices, i)
	}
	for i := 0; i < len(indices); i++ {
		for j := i + 1; j < len(indices); j++ {
			if indices[j] < indices[i] {
				indices[i], indices[j] = indices[j], indices[i]
			}
		}
	}

	// Record the assistant tool-call message so the follow-up LLM call
	// (after the client's function_call_output) has full context.
	assistantMsg := sidecar.ChatMessage{Role: "assistant"}
	outputItems := make([]any, 0, len(indices))

	for _, idx := range indices {
		call := toolAccum[idx]
		if call.ID == "" {
			call.ID = rtID("call")
		}
		assistantMsg.ToolCalls = append(assistantMsg.ToolCalls, *call)
		args := call.Function.Arguments

		fcItemID := rtID("fc")
		fcItem := fiber.Map{
			"id": fcItemID, "object": "realtime.item", "type": "function_call",
			"call_id": call.ID, "name": call.Function.Name,
			"arguments": args, "status": "completed",
		}
		sess.send(fiber.Map{
			"type":         "response.output_item.added",
			"event_id":     rtID("evt"),
			"response_id":  responseID,
			"output_index": idx,
			"item": fiber.Map{
				"id": fcItemID, "object": "realtime.item", "type": "function_call",
				"call_id": call.ID, "name": call.Function.Name,
				"arguments": "", "status": "in_progress",
			},
		})
		// Stream the arguments in chunks like OpenAI does.
		for i := 0; i < len(args); i += 512 {
			end := i + 512
			if end > len(args) {
				end = len(args)
			}
			sess.send(fiber.Map{
				"type":         "response.function_call_arguments.delta",
				"event_id":     rtID("evt"),
				"response_id":  responseID,
				"item_id":      fcItemID,
				"output_index": idx,
				"call_id":      call.ID,
				"delta":        args[i:end],
			})
		}
		sess.send(fiber.Map{
			"type":         "response.function_call_arguments.done",
			"event_id":     rtID("evt"),
			"response_id":  responseID,
			"item_id":      fcItemID,
			"output_index": idx,
			"call_id":      call.ID,
			"name":         call.Function.Name,
			"arguments":    args,
		})
		sess.send(fiber.Map{
			"type":         "response.output_item.done",
			"event_id":     rtID("evt"),
			"response_id":  responseID,
			"output_index": idx,
			"item":         fcItem,
		})
		outputItems = append(outputItems, fcItem)
		sess.toolCalls++
		metrics.RecordRealtimeS2SToolCall()
	}

	sess.appendHistory(assistantMsg)
	sess.send(fiber.Map{
		"type":     "response.done",
		"event_id": rtID("evt"),
		"response": fiber.Map{
			"id": responseID, "object": "realtime.response", "status": "completed",
			"status_details": nil, "output": outputItems,
		},
	})

	slog.Info("[OA-RT-S2S] tool call relayed", "request_id", sess.requestID, "turn_id", responseID, "calls", len(indices))
	h.lm.SendLog(h.lm.BuildLog("OPENAI_REALTIME_S2S_TOOL_CALL", "OpenAIRealtimeS2SToolCall", slog.LevelInfo, map[string]interface{}{
		"endpoint":   "/v1/realtime",
		"request_id": sess.requestID,
		"turn_id":    responseID,
		"calls":      len(indices),
	}))
}

// s2sTTSStats accumulates per-turn streaming TTS telemetry across sentences.
// Logged once per turn on the turn-completed event (per-sentence info logs
// would be too chatty for Loki on long responses).
type s2sTTSStats struct {
	sentences         int
	totalSynthTime    time.Duration
	firstChunkLatency time.Duration
}

// speakSentence synthesizes one sentence via streaming TTS and forwards the
// frames as response.audio.delta events. In text-only mode it emits
// response.text.delta instead. Returns false if interrupted or on TTS error.
func (h *OpenAIRealtimeS2SHandler) speakSentence(ctx context.Context, sess *s2sSession, gen int64, responseID, sentence string, firstAudioRecorded *bool, stats *s2sTTSStats) bool {
	if sess.gen.Load() != gen {
		return false
	}

	// Text modality always carries the sentence.
	sess.sendIfCurrent(gen, fiber.Map{
		"type":          "response.audio_transcript.delta",
		"event_id":      rtID("evt"),
		"response_id":   responseID,
		"item_id":       sess.respItemID,
		"output_index":  0,
		"content_index": 0,
		"delta":         sentence,
	})
	if sess.textOnly {
		sess.sendIfCurrent(gen, fiber.Map{
			"type":          "response.text.delta",
			"event_id":      rtID("evt"),
			"response_id":   responseID,
			"item_id":       sess.respItemID,
			"output_index":  0,
			"content_index": 0,
			"delta":         sentence,
		})
		return true
	}

	body, err := h.sc.SynthesizeStream(ctx, sentence, sess.voice, responseID)
	if err != nil {
		if ctx.Err() == nil && sess.gen.Load() == gen {
			sidecarStatus := 0
			var scErr *sidecar.SidecarError
			if errors.As(err, &scErr) {
				sidecarStatus = scErr.StatusCode
			}
			slog.Warn("[OA-RT-S2S] TTS stream failed", "error", err, "request_id", sess.requestID, "turn_id", responseID)
			h.lm.SendLog(h.lm.BuildLog("RT_S2S_TTS_STREAM_FAILED", "RTS2STTSStreamFailed", slog.LevelError, map[string]interface{}{
				"endpoint":       "/v1/realtime",
				"voice":          sess.voice,
				"sentence_chars": len(sentence),
				"turn_id":        responseID,
				"sidecar_status": sidecarStatus,
				"request_id":     sess.requestID,
			}, err))
			sess.sendErr("server_error", "tts_unavailable", "speech synthesis unavailable: "+err.Error())
		}
		return false
	}
	defer body.Close()

	sentenceStart := time.Now()
	ttsStart := time.Now()
	slog.Debug("[OA-RT-S2S] TTS sentence started", "request_id", sess.requestID, "turn_id", responseID,
		"sentence_chars", len(sentence))
	buf := make([]byte, 4096) // ~85 ms of 24kHz PCM16 per read
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if sess.gen.Load() != gen || ctx.Err() != nil {
				return false
			}
			if !*firstAudioRecorded {
				firstChunk := time.Since(ttsStart)
				metrics.RecordRealtimeS2STTSFirstChunk(firstChunk.Seconds())
				// Headline metric: end-of-speech → first audio byte out.
				metrics.RecordRealtimeS2STurn(sess.engine, time.Since(sess.turnStart).Seconds())
				stats.firstChunkLatency = firstChunk
				*firstAudioRecorded = true
			}
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			atomic.AddInt64(&sess.audioOutBytes, int64(n))
			sess.respAudioBytes += int64(n)
			sess.sendIfCurrent(gen, fiber.Map{
				"type":          "response.audio.delta",
				"event_id":      rtID("evt"),
				"response_id":   responseID,
				"item_id":       sess.respItemID,
				"output_index":  0,
				"content_index": 0,
				"delta":         base64.StdEncoding.EncodeToString(chunk),
			})
		}
		if err == io.EOF {
			stats.sentences++
			stats.totalSynthTime += time.Since(sentenceStart)
			return true
		}
		if err != nil {
			// Cancellation (barge-in) surfaces as a read error — silent.
			if ctx.Err() == nil && sess.gen.Load() == gen {
				slog.Warn("[OA-RT-S2S] TTS read error", "error", err, "request_id", sess.requestID, "turn_id", responseID)
			}
			return ctx.Err() == nil && sess.gen.Load() == gen
		}
	}
}

// ──────────────────────────────────────────────────────────────
// Sentence splitter — token stream → speakable sentences
// ──────────────────────────────────────────────────────────────

// sentenceSplitter buffers streamed LLM tokens and yields speakable chunks:
// terminal punctuation always flushes; a soft boundary (comma, semicolon,
// colon, dash) flushes once the buffer is long enough. The first chunk of a
// response flushes aggressively (fewer words) so TTS starts early; later
// chunks are allowed to grow for TTS throughput.
type sentenceSplitter struct {
	buf        strings.Builder
	flushedAny bool
}

func newSentenceSplitter() *sentenceSplitter { return &sentenceSplitter{} }

func (s *sentenceSplitter) Write(delta string) { s.buf.WriteString(delta) }

const (
	s2sFirstChunkMinWords = 6
	s2sChunkMinWords      = 10
)

// Next returns the next speakable sentence, or ok=false if the buffer
// doesn't hold a complete one yet.
func (s *sentenceSplitter) Next() (string, bool) {
	text := s.buf.String()
	if idx := strings.IndexAny(text, ".!?\n"); idx >= 0 {
		return s.emit(idx + 1)
	}
	minWords := s2sChunkMinWords
	if !s.flushedAny {
		minWords = s2sFirstChunkMinWords
	}
	if wordCount(text) >= minWords {
		// Flush at the LAST soft boundary so the chunk stays long.
		if idx := lastIndexAny(text, ",;:-—–"); idx > 0 {
			return s.emit(idx + 1)
		}
	}
	return "", false
}

// Flush returns whatever remains in the buffer (end of stream).
func (s *sentenceSplitter) Flush() string {
	text := strings.TrimSpace(s.buf.String())
	s.buf.Reset()
	return text
}

func (s *sentenceSplitter) emit(end int) (string, bool) {
	text := s.buf.String()
	out := strings.TrimSpace(text[:end])
	rest := text[end:]
	s.buf.Reset()
	s.buf.WriteString(rest)
	if out == "" {
		return "", false
	}
	s.flushedAny = true
	return out, true
}

func wordCount(s string) int { return len(strings.Fields(s)) }

func lastIndexAny(s, chars string) int {
	last := -1
	for _, c := range chars {
		if idx := strings.LastIndex(s, string(c)); idx > last {
			last = idx
		}
	}
	return last
}

// s2sEngineFromQuery resolves the ASR engine for an S2S session:
// explicit ?engine= wins, then the model name, then the default.
func s2sEngineFromQuery(c *websocket.Conn) string {
	if e := c.Query("engine"); e != "" {
		return e
	}
	if eng, ok := openAIModelToRealtimeEngine(c.Query("model")); ok {
		return eng
	}
	return "eou-320"
}
