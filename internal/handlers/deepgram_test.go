package handlers

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	ws "github.com/fasthttp/websocket"
	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/shaunagostinho/gotranscribesrv/internal/logging"
	"github.com/shaunagostinho/gotranscribesrv/internal/metrics"
	"github.com/shaunagostinho/gotranscribesrv/internal/pii"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
)

// metricsOnce guards metrics.Init — prometheus.MustRegister panics on
// duplicate registration, and only Init(true) allocates the gauge vecs
// the handlers touch.
var metricsOnce sync.Once

// mockSidecarApp mimics the audio sidecar's WS /stream protocol:
// ready on connect, ignores binary audio, answers Finalize with a
// from_finalize=true final event (session stays open), answers
// CloseStream with a from_finalize=false final + done.
func mockSidecarApp() *fiber.App {
	return mockSidecarAppWithFinalText("hello world")
}

func mockSidecarAppWithFinalText(finalText string) *fiber.App {
	app := fiber.New()
	app.Use("/stream", func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})
	app.Get("/stream", websocket.New(func(c *websocket.Conn) {
		defer c.Close()
		_ = c.WriteJSON(fiber.Map{"type": "ready"})
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.TextMessage {
				continue // binary audio — ignore
			}
			var ctrl map[string]any
			if json.Unmarshal(msg, &ctrl) != nil {
				continue
			}
			typ, _ := ctrl["type"].(string)
			switch typ {
			case "Finalize":
				_ = c.WriteJSON(fiber.Map{
					"type":  "final",
					"text":  finalText,
					"start": 0.0,
					"end":   1.0,
					"words": []any{
						map[string]any{"word": "hello", "start": 0.0, "end": 0.5},
						map[string]any{"word": "world", "start": 0.5, "end": 1.0},
					},
					"is_final":      true,
					"from_finalize": true,
				})
			case "CloseStream":
				_ = c.WriteJSON(fiber.Map{
					"type":          "final",
					"text":          finalText,
					"start":         0.0,
					"end":           1.0,
					"words":         []any{},
					"is_final":      true,
					"from_finalize": false,
				})
				_ = c.WriteJSON(fiber.Map{"type": "done"})
				return
			}
		}
	}))
	return app
}

func deepgramTestApp(t *testing.T, sidecarWSURL string, redactor *pii.Redactor, lm *logging.LogManager) *fiber.App {
	t.Helper()
	sc := sidecar.NewClient("http://unused", sidecarWSURL, "http://unused-llm")
	metricsOnce.Do(func() { metrics.Init(true) })
	h := NewDeepgramHandler(sc, redactor, nil, true, lm)

	app := fiber.New()
	app.Use("/v1/listen", func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})
	app.Get("/v1/listen", h.Upgrade())
	return app
}

func startTestApp(t *testing.T, app *fiber.App) (wsURL string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = app.Listener(ln) }()
	t.Cleanup(func() { _ = app.Shutdown() })
	return "ws://" + ln.Addr().String()
}

// TestDeepgramFinalizeFlow verifies the Deepgram streaming protocol
// surface of /v1/listen: Metadata carries the spec-required fields,
// and a client Finalize message yields a Results event with
// from_finalize=true (previously Finalize was silently dropped).
func TestDeepgramFinalizeFlow(t *testing.T) {
	sidecarWS := startTestApp(t, mockSidecarApp())
	lm := logging.NewLogManager(nil, false)
	t.Cleanup(lm.CloseLogManager)
	handlerWS := startTestApp(t, deepgramTestApp(t, sidecarWS, pii.NewRedactor(nil, false, nil, 0), lm))

	conn, _, err := ws.DefaultDialer.Dial(handlerWS+"/v1/listen", nil)
	if err != nil {
		t.Fatalf("dial handler: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	// 1. Metadata — spec requires transaction_key + sha256.
	var meta map[string]any
	if err := conn.ReadJSON(&meta); err != nil {
		t.Fatalf("read Metadata: %v", err)
	}
	if meta["type"] != "Metadata" {
		t.Errorf("first event type = %v, want Metadata", meta["type"])
	}
	if meta["transaction_key"] == nil {
		t.Error("Metadata missing spec-required transaction_key")
	}
	if s, _ := meta["sha256"].(string); len(s) != 64 {
		t.Errorf("Metadata sha256 = %q, want 64-char hex", s)
	}
	if meta["request_id"] == nil || meta["request_id"] == "" {
		t.Error("Metadata missing request_id")
	}

	// 2. Send an audio frame so the session has data.
	if err := conn.WriteMessage(ws.BinaryMessage, make([]byte, 3200)); err != nil {
		t.Fatalf("write audio: %v", err)
	}

	// 3. Finalize → Results with from_finalize=true.
	if err := conn.WriteMessage(ws.TextMessage, []byte(`{"type":"Finalize"}`)); err != nil {
		t.Fatalf("write Finalize: %v", err)
	}
	var res map[string]any
	if err := conn.ReadJSON(&res); err != nil {
		t.Fatalf("read Results after Finalize: %v", err)
	}
	if res["type"] != "Results" {
		t.Fatalf("event after Finalize type = %v, want Results", res["type"])
	}
	if res["from_finalize"] != true {
		t.Errorf("Results.from_finalize = %v, want true", res["from_finalize"])
	}
	if res["is_final"] != true {
		t.Errorf("Results.is_final = %v, want true", res["is_final"])
	}
	ch, _ := res["channel"].(map[string]any)
	alts, _ := ch["alternatives"].([]any)
	if len(alts) == 0 {
		t.Fatal("Results.channel.alternatives empty")
	}
	alt0, _ := alts[0].(map[string]any)
	if alt0["transcript"] != "hello world" {
		t.Errorf("transcript = %v, want %q", alt0["transcript"], "hello world")
	}
	if md, ok := res["metadata"].(map[string]any); !ok || md["request_id"] == "" {
		t.Error("Results missing spec-required metadata.request_id")
	}

	// 4. CloseStream → Results with from_finalize=false.
	if err := conn.WriteMessage(ws.TextMessage, []byte(`{"type":"CloseStream"}`)); err != nil {
		t.Fatalf("write CloseStream: %v", err)
	}
	var res2 map[string]any
	if err := conn.ReadJSON(&res2); err != nil {
		t.Fatalf("read Results after CloseStream: %v", err)
	}
	if res2["type"] != "Results" {
		t.Fatalf("event after CloseStream type = %v, want Results", res2["type"])
	}
	if res2["from_finalize"] != false {
		t.Errorf("CloseStream Results.from_finalize = %v, want false", res2["from_finalize"])
	}
	if res2["is_final"] != true || res2["speech_final"] != true {
		t.Errorf("CloseStream Results is_final=%v speech_final=%v, want both true",
			res2["is_final"], res2["speech_final"])
	}

	// The server ends the session after "done" and closes the conn.
	// Wait for the close so the session-end log path finishes before
	// the test tears down the LogManager.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
}

// TestDeepgramFinalRedactsPIIForLokiButNotClient verifies the privacy
// boundary on response logging: the client receives the RAW transcript,
// while the DEEPGRAM_FINAL_SENT event shipped toward Loki carries the
// PII-redacted form plus redaction metadata.
func TestDeepgramFinalRedactsPIIForLokiButNotClient(t *testing.T) {
	const rawText = "call john at 212-555-1234"

	// Fake Presidio analyzer: john → PERSON, 212-555-1234 → PHONE_NUMBER.
	presidio := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]sidecar.PresidioEntity{
			{Start: 5, End: 9, Score: 0.95, EntityType: "PERSON"},
			{Start: 13, End: 25, Score: 0.98, EntityType: "PHONE_NUMBER"},
		})
	}))
	defer presidio.Close()

	redactor := pii.NewRedactor(sidecar.NewPresidioClient(presidio.URL, time.Second), true, nil, 0)

	// Hand-rolled LogManager (no consumer goroutine) so the test can
	// drain LogChannel and inspect the Loki-bound events directly.
	lm := &logging.LogManager{
		Templates:   make(map[string]string),
		LokiEnabled: false,
		LogChannel:  make(chan *logging.LoggingFormat, 64),
	}
	lm.LoadTemplates()

	sidecarWS := startTestApp(t, mockSidecarAppWithFinalText(rawText))
	handlerWS := startTestApp(t, deepgramTestApp(t, sidecarWS, redactor, lm))

	conn, _, err := ws.DefaultDialer.Dial(handlerWS+"/v1/listen", nil)
	if err != nil {
		t.Fatalf("dial handler: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	var meta map[string]any
	if err := conn.ReadJSON(&meta); err != nil {
		t.Fatalf("read Metadata: %v", err)
	}
	if err := conn.WriteMessage(ws.BinaryMessage, make([]byte, 3200)); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	if err := conn.WriteMessage(ws.TextMessage, []byte(`{"type":"CloseStream"}`)); err != nil {
		t.Fatalf("write CloseStream: %v", err)
	}

	// Client must receive the RAW transcript — redaction is log-path only.
	var res map[string]any
	if err := conn.ReadJSON(&res); err != nil {
		t.Fatalf("read Results: %v", err)
	}
	ch, _ := res["channel"].(map[string]any)
	alts, _ := ch["alternatives"].([]any)
	if len(alts) == 0 {
		t.Fatal("Results.channel.alternatives empty")
	}
	alt0, _ := alts[0].(map[string]any)
	if alt0["transcript"] != rawText {
		t.Errorf("client transcript = %v, want raw %q (redaction must not leak into client responses)",
			alt0["transcript"], rawText)
	}

	// Drain Loki-bound events until DEEPGRAM_FINAL_SENT appears.
	deadline := time.Now().Add(2 * time.Second)
	var finalSent *logging.LoggingFormat
	for time.Now().Before(deadline) && finalSent == nil {
		select {
		case ev := <-lm.LogChannel:
			if ev.Type == "DEEPGRAM_FINAL_SENT" {
				finalSent = ev
			}
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	if finalSent == nil {
		t.Fatal("DEEPGRAM_FINAL_SENT event never emitted")
	}

	got, _ := finalSent.AdditionalData["transcript"].(string)
	want := "call <PERSON> at <PHONE_NUMBER>"
	if got != want {
		t.Errorf("Loki transcript = %q, want redacted %q", got, want)
	}
	if n, _ := finalSent.AdditionalData["pii_redacted"].(int); n != 2 {
		t.Errorf("pii_redacted = %v, want 2", finalSent.AdditionalData["pii_redacted"])
	}
	types, _ := finalSent.AdditionalData["pii_entity_types"].([]string)
	if len(types) != 2 {
		t.Errorf("pii_entity_types = %v, want [PERSON PHONE_NUMBER]", types)
	}
}

// ── Logging lifecycle tests ─────────────────────────────────────────

// captureLogManager returns a LogManager with no consumer goroutine so
// tests can drain LogChannel and assert on the Loki-bound events.
func captureLogManager() *logging.LogManager {
	lm := &logging.LogManager{
		Templates:   make(map[string]string),
		LokiEnabled: false,
		LogChannel:  make(chan *logging.LoggingFormat, 64),
	}
	lm.LoadTemplates()
	return lm
}

// waitForLogEvent drains lm.LogChannel until an event of the given type
// appears or the timeout elapses. Intervening events are discarded —
// call it in emission order.
func waitForLogEvent(lm *logging.LogManager, eventType string, timeout time.Duration) *logging.LoggingFormat {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case ev := <-lm.LogChannel:
			if ev.Type == eventType {
				return ev
			}
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	return nil
}

// TestDeepgramSessionLifecycleLogging asserts the v1 session emits the
// full Loki event lifecycle — STARTED (with request metadata), FINAL_SENT,
// ENDED (with usage stats) and CLIENT_READ_ERROR on disconnect — with the
// fields operators need for Whisper-style observability.
func TestDeepgramSessionLifecycleLogging(t *testing.T) {
	lm := captureLogManager()
	sidecarWS := startTestApp(t, mockSidecarApp())
	handlerWS := startTestApp(t, deepgramTestApp(t, sidecarWS, pii.NewRedactor(nil, false, nil, 0), lm))

	header := http.Header{"User-Agent": []string{"dg-lifecycle-test/1.0"}}
	conn, _, err := ws.DefaultDialer.Dial(handlerWS+"/v1/listen?language=en&sample_rate=16000", header)
	if err != nil {
		t.Fatalf("dial handler: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	var meta map[string]any
	if err := conn.ReadJSON(&meta); err != nil {
		t.Fatalf("read Metadata: %v", err)
	}

	// SESSION_STARTED — request metadata must be present.
	started := waitForLogEvent(lm, "DEEPGRAM_SESSION_STARTED", 2*time.Second)
	if started == nil {
		t.Fatal("DEEPGRAM_SESSION_STARTED never emitted")
	}
	if started.Message != "Deepgram-compat session started" {
		t.Errorf("started message = %q, template not resolved?", started.Message)
	}
	if ua, _ := started.AdditionalData["user_agent"].(string); ua != "dg-lifecycle-test/1.0" {
		t.Errorf("user_agent = %v, want dg-lifecycle-test/1.0", ua)
	}
	if m, _ := started.AdditionalData["model"].(string); m == "" {
		t.Error("model missing from SESSION_STARTED")
	}
	if lang, _ := started.AdditionalData["language"].(string); lang != "en" {
		t.Errorf("language = %v, want en", lang)
	}

	// Drive a full session: audio → CloseStream → final + done.
	if err := conn.WriteMessage(ws.BinaryMessage, make([]byte, 3200)); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	if err := conn.WriteMessage(ws.TextMessage, []byte(`{"type":"CloseStream"}`)); err != nil {
		t.Fatalf("write CloseStream: %v", err)
	}
	var res map[string]any
	if err := conn.ReadJSON(&res); err != nil {
		t.Fatalf("read Results: %v", err)
	}

	if waitForLogEvent(lm, "DEEPGRAM_FINAL_SENT", 2*time.Second) == nil {
		t.Fatal("DEEPGRAM_FINAL_SENT never emitted")
	}

	// SESSION_ENDED — usage stats must be present and consistent.
	ended := waitForLogEvent(lm, "DEEPGRAM_SESSION_ENDED", 2*time.Second)
	if ended == nil {
		t.Fatal("DEEPGRAM_SESSION_ENDED never emitted")
	}
	if ended.Message != "Deepgram-compat session ended" {
		t.Errorf("ended message = %q, template not resolved?", ended.Message)
	}
	if b, _ := ended.AdditionalData["audio_bytes"].(int); b != 3200 {
		t.Errorf("audio_bytes = %v, want 3200", b)
	}
	if fc, _ := ended.AdditionalData["frame_count"].(int); fc != 1 {
		t.Errorf("frame_count = %v, want 1", fc)
	}
	if d, _ := ended.AdditionalData["audio_duration_ms"].(int); d != 100 {
		t.Errorf("audio_duration_ms = %v, want 100 (3200 bytes / 32)", d)
	}

	// Normal teardown (sidecar done) must NOT emit CLIENT_READ_ERROR —
	// the event is reserved for client-initiated disconnects. Verify by
	// abruptly closing a second connection.
	conn2, _, err := ws.DefaultDialer.Dial(handlerWS+"/v1/listen", nil)
	if err != nil {
		t.Fatalf("dial handler (conn2): %v", err)
	}
	var meta2 map[string]any
	if err := conn2.ReadJSON(&meta2); err != nil {
		t.Fatalf("read Metadata (conn2): %v", err)
	}
	if err := conn2.WriteMessage(ws.BinaryMessage, make([]byte, 640)); err != nil {
		t.Fatalf("write audio (conn2): %v", err)
	}
	_ = conn2.Close() // abrupt client disconnect ends the session

	readErr := waitForLogEvent(lm, "DEEPGRAM_CLIENT_READ_ERROR", 2*time.Second)
	if readErr == nil {
		t.Fatal("DEEPGRAM_CLIENT_READ_ERROR never emitted after abrupt client close")
	}
	if b, _ := readErr.AdditionalData["total_bytes"].(int); b != 640 {
		t.Errorf("total_bytes = %v, want 640", b)
	}
}

// mockRealtimeSidecarApp mimics the sidecar's /stream/realtime protocol:
// ready on connect, one speech_final final after the first audio frame,
// then the connection closes (ending the session).
func mockRealtimeSidecarApp() *fiber.App {
	app := fiber.New()
	app.Use("/stream/realtime", func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})
	app.Get("/stream/realtime", websocket.New(func(c *websocket.Conn) {
		defer c.Close()
		_ = c.WriteJSON(fiber.Map{"type": "ready"})
		for {
			mt, _, err := c.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.BinaryMessage {
				continue
			}
			_ = c.WriteJSON(fiber.Map{
				"type":         "final",
				"text":         "realtime hello",
				"speech_final": true,
				"time":         1.0,
			})
			return // close → ends the handler session
		}
	}))
	return app
}

func deepgramRealtimeTestApp(t *testing.T, sidecarWSURL string, lm *logging.LogManager) *fiber.App {
	t.Helper()
	sc := sidecar.NewClient("http://unused", sidecarWSURL, "http://unused-llm")
	metricsOnce.Do(func() { metrics.Init(true) })
	h := NewDeepgramRealtimeHandler(sc, pii.NewRedactor(nil, false, nil, 0), lm, nil)

	app := fiber.New()
	app.Use("/v2/listen", func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})
	app.Get("/v2/listen", h.Upgrade())
	return app
}

// TestDeepgramRealtimeSessionLifecycleLogging asserts the /v2/listen
// session emits the same lifecycle event coverage as the legacy handler:
// STARTED (with engine + request metadata), FINAL_SENT, ENDED (with
// usage stats) and CLIENT_READ_ERROR on teardown.
func TestDeepgramRealtimeSessionLifecycleLogging(t *testing.T) {
	lm := captureLogManager()
	sidecarWS := startTestApp(t, mockRealtimeSidecarApp())
	handlerWS := startTestApp(t, deepgramRealtimeTestApp(t, sidecarWS, lm))

	header := http.Header{"User-Agent": []string{"dg-rt-lifecycle-test/1.0"}}
	conn, _, err := ws.DefaultDialer.Dial(handlerWS+"/v2/listen?model=nova-3", header)
	if err != nil {
		t.Fatalf("dial handler: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	// First frame to the client is the Deepgram Metadata event.
	var meta map[string]any
	if err := conn.ReadJSON(&meta); err != nil {
		t.Fatalf("read Metadata: %v", err)
	}

	started := waitForLogEvent(lm, "DEEPGRAM_REALTIME_STARTED", 2*time.Second)
	if started == nil {
		t.Fatal("DEEPGRAM_REALTIME_STARTED never emitted")
	}
	if started.Message != "Deepgram-realtime session started" {
		t.Errorf("started message = %q, template not resolved?", started.Message)
	}
	if eng, _ := started.AdditionalData["engine"].(string); eng != "eou-320" {
		t.Errorf("engine = %v, want eou-320 (nova-3 mapping)", eng)
	}
	if ua, _ := started.AdditionalData["user_agent"].(string); ua != "dg-rt-lifecycle-test/1.0" {
		t.Errorf("user_agent = %v, want dg-rt-lifecycle-test/1.0", ua)
	}

	// One audio frame → mock sidecar emits a speech_final final + closes.
	if err := conn.WriteMessage(ws.BinaryMessage, make([]byte, 6400)); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	var res map[string]any
	if err := conn.ReadJSON(&res); err != nil {
		t.Fatalf("read Results: %v", err)
	}
	if res["type"] != "Results" || res["is_final"] != true {
		t.Errorf("event = %v, want final Results", res)
	}

	finalSent := waitForLogEvent(lm, "DEEPGRAM_REALTIME_FINAL_SENT", 2*time.Second)
	if finalSent == nil {
		t.Fatal("DEEPGRAM_REALTIME_FINAL_SENT never emitted")
	}
	if tr, _ := finalSent.AdditionalData["transcript"].(string); tr != "realtime hello" {
		t.Errorf("transcript = %v, want %q", tr, "realtime hello")
	}

	ended := waitForLogEvent(lm, "DEEPGRAM_REALTIME_ENDED", 2*time.Second)
	if ended == nil {
		t.Fatal("DEEPGRAM_REALTIME_ENDED never emitted")
	}
	if ended.Message != "Deepgram-realtime session ended" {
		t.Errorf("ended message = %q, template not resolved?", ended.Message)
	}
	if b, _ := ended.AdditionalData["audio_bytes"].(int); b != 6400 {
		t.Errorf("audio_bytes = %v, want 6400", b)
	}
	if fc, _ := ended.AdditionalData["frame_count"].(int); fc != 1 {
		t.Errorf("frame_count = %v, want 1", fc)
	}
	if eng, _ := ended.AdditionalData["engine"].(string); eng != "eou-320" {
		t.Errorf("engine = %v, want eou-320", eng)
	}

	// Abrupt client disconnect → REALTIME_CLIENT_READ_ERROR (see the v1
	// lifecycle test for why normal teardown must not emit it).
	conn2, _, err := ws.DefaultDialer.Dial(handlerWS+"/v2/listen", nil)
	if err != nil {
		t.Fatalf("dial handler (conn2): %v", err)
	}
	var meta2 map[string]any
	if err := conn2.ReadJSON(&meta2); err != nil {
		t.Fatalf("read Metadata (conn2): %v", err)
	}
	if err := conn2.WriteMessage(ws.BinaryMessage, make([]byte, 640)); err != nil {
		t.Fatalf("write audio (conn2): %v", err)
	}
	_ = conn2.Close()

	readErr := waitForLogEvent(lm, "DEEPGRAM_REALTIME_CLIENT_READ_ERROR", 2*time.Second)
	if readErr == nil {
		t.Fatal("DEEPGRAM_REALTIME_CLIENT_READ_ERROR never emitted after abrupt client close")
	}
}

// TestDeepgramRealtimeConnectFailedEmitsEvent asserts that a failed
// sidecar dial produces a DEEPGRAM_REALTIME_CONNECT_FAILED Loki event
// (previously the failure was only visible to the WebSocket client).
func TestDeepgramRealtimeConnectFailedEmitsEvent(t *testing.T) {
	// Grab a port and close it — guaranteed refused connection.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := ln.Addr().String()
	_ = ln.Close()

	lm := captureLogManager()
	handlerWS := startTestApp(t, deepgramRealtimeTestApp(t, "ws://"+deadAddr, lm))

	conn, _, err := ws.DefaultDialer.Dial(handlerWS+"/v2/listen", nil)
	if err != nil {
		t.Fatalf("dial handler: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	// Client still receives a protocol-level Error frame.
	var errEvt map[string]any
	if err := conn.ReadJSON(&errEvt); err != nil {
		t.Fatalf("read Error event: %v", err)
	}
	if errEvt["type"] != "Error" {
		t.Errorf("client event type = %v, want Error", errEvt["type"])
	}

	if waitForLogEvent(lm, "DEEPGRAM_REALTIME_STARTED", 2*time.Second) == nil {
		t.Fatal("DEEPGRAM_REALTIME_STARTED never emitted")
	}
	failed := waitForLogEvent(lm, "DEEPGRAM_REALTIME_CONNECT_FAILED", 2*time.Second)
	if failed == nil {
		t.Fatal("DEEPGRAM_REALTIME_CONNECT_FAILED never emitted")
	}
	if failed.Message == "DeepgramRealtimeConnectFailed" {
		t.Error("message is the raw template name — template not registered")
	}
	if eng, _ := failed.AdditionalData["engine"].(string); eng != "eou-320" {
		t.Errorf("engine = %v, want eou-320 (default)", eng)
	}
}

// ── Flush / Finalize teardown tests ─────────────────────────────────

// mockSidecarAppWithFinalizeDelay behaves like mockSidecarApp but waits
// finalizeDelay before answering a Finalize — simulating a sidecar that
// needs real time to transcribe the buffered audio.
func mockSidecarAppWithFinalizeDelay(finalText string, finalizeDelay time.Duration) *fiber.App {
	app := fiber.New()
	app.Use("/stream", func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})
	app.Get("/stream", websocket.New(func(c *websocket.Conn) {
		defer c.Close()
		_ = c.WriteJSON(fiber.Map{"type": "ready"})
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.TextMessage {
				continue
			}
			var ctrl map[string]any
			if json.Unmarshal(msg, &ctrl) != nil {
				continue
			}
			switch typ, _ := ctrl["type"].(string); typ {
			case "Finalize":
				time.Sleep(finalizeDelay)
				_ = c.WriteJSON(fiber.Map{
					"type":          "final",
					"text":          finalText,
					"start":         0.0,
					"end":           1.0,
					"words":         []any{},
					"is_final":      true,
					"from_finalize": true,
				})
			case "CloseStream":
				_ = c.WriteJSON(fiber.Map{
					"type":          "final",
					"text":          finalText,
					"start":         0.0,
					"end":           1.0,
					"words":         []any{},
					"is_final":      true,
					"from_finalize": false,
				})
				_ = c.WriteJSON(fiber.Map{"type": "done"})
				return
			}
		}
	}))
	return app
}

// mockSidecarDiesOnFinalize closes the connection when it receives a
// Finalize — simulating a sidecar crash mid-flush.
func mockSidecarDiesOnFinalize() *fiber.App {
	app := fiber.New()
	app.Use("/stream", func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})
	app.Get("/stream", websocket.New(func(c *websocket.Conn) {
		defer c.Close()
		_ = c.WriteJSON(fiber.Map{"type": "ready"})
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.TextMessage {
				continue
			}
			var ctrl map[string]any
			if json.Unmarshal(msg, &ctrl) != nil {
				continue
			}
			if typ, _ := ctrl["type"].(string); typ == "Finalize" {
				return // die without answering
			}
		}
	}))
	return app
}

// halfClose shuts down the write side of a test client's TCP conn,
// mimicking Deepgram-client behavior of closing right after Finalize
// while still expecting the final Results (the read side stays open).
func halfClose(t *testing.T, conn *ws.Conn) {
	t.Helper()
	type closeWriter interface{ CloseWrite() error }
	cw, ok := conn.NetConn().(closeWriter)
	if !ok {
		t.Skip("test client conn does not support CloseWrite")
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
}

// TestDeepgramFinalizeSurvivesClientHalfClose is the regression test for
// the lost-Finalize bug: a client that disconnects (here: half-close)
// immediately after sending Finalize must still receive the final
// Results event — teardown must wait for the pending flush instead of
// killing the sidecar connection instantly.
func TestDeepgramFinalizeSurvivesClientHalfClose(t *testing.T) {
	lm := captureLogManager()
	sidecarWS := startTestApp(t, mockSidecarAppWithFinalizeDelay("flushed transcript", 150*time.Millisecond))
	handlerWS := startTestApp(t, deepgramTestApp(t, sidecarWS, pii.NewRedactor(nil, false, nil, 0), lm))

	conn, _, err := ws.DefaultDialer.Dial(handlerWS+"/v1/listen", nil)
	if err != nil {
		t.Fatalf("dial handler: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	var meta map[string]any
	if err := conn.ReadJSON(&meta); err != nil {
		t.Fatalf("read Metadata: %v", err)
	}
	if err := conn.WriteMessage(ws.BinaryMessage, make([]byte, 3200)); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	if err := conn.WriteMessage(ws.TextMessage, []byte(`{"type":"Finalize"}`)); err != nil {
		t.Fatalf("write Finalize: %v", err)
	}

	// Disconnect immediately — before the sidecar answers (150ms delay).
	halfClose(t, conn)

	// The final Results must still arrive on the half-closed conn.
	var res map[string]any
	if err := conn.ReadJSON(&res); err != nil {
		t.Fatalf("read Results after half-close: %v (final was lost to teardown)", err)
	}
	if res["type"] != "Results" {
		t.Fatalf("event type = %v, want Results", res["type"])
	}
	if res["from_finalize"] != true {
		t.Errorf("from_finalize = %v, want true", res["from_finalize"])
	}
	ch, _ := res["channel"].(map[string]any)
	alts, _ := ch["alternatives"].([]any)
	if len(alts) == 0 {
		t.Fatal("Results.channel.alternatives empty")
	}
	if alt0, _ := alts[0].(map[string]any); alt0["transcript"] != "flushed transcript" {
		t.Errorf("transcript = %v, want %q", alt0["transcript"], "flushed transcript")
	}
}

// TestDeepgramCloseStreamReturnsTerminalMetadata verifies the Deepgram
// spec behavior on CloseStream: after the final Results the server sends
// a terminal Metadata summary before terminating the connection.
func TestDeepgramCloseStreamReturnsTerminalMetadata(t *testing.T) {
	lm := captureLogManager()
	sidecarWS := startTestApp(t, mockSidecarApp())
	handlerWS := startTestApp(t, deepgramTestApp(t, sidecarWS, pii.NewRedactor(nil, false, nil, 0), lm))

	conn, _, err := ws.DefaultDialer.Dial(handlerWS+"/v1/listen", nil)
	if err != nil {
		t.Fatalf("dial handler: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	var meta map[string]any
	if err := conn.ReadJSON(&meta); err != nil {
		t.Fatalf("read Metadata: %v", err)
	}
	if err := conn.WriteMessage(ws.BinaryMessage, make([]byte, 32000)); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	if err := conn.WriteMessage(ws.TextMessage, []byte(`{"type":"CloseStream"}`)); err != nil {
		t.Fatalf("write CloseStream: %v", err)
	}

	// 1. Final Results.
	var res map[string]any
	if err := conn.ReadJSON(&res); err != nil {
		t.Fatalf("read Results: %v", err)
	}
	if res["type"] != "Results" || res["is_final"] != true {
		t.Fatalf("first event = %v, want final Results", res)
	}

	// 2. Terminal Metadata summary (Deepgram spec).
	var term map[string]any
	if err := conn.ReadJSON(&term); err != nil {
		t.Fatalf("read terminal Metadata: %v", err)
	}
	if term["type"] != "Metadata" {
		t.Fatalf("terminal event type = %v, want Metadata", term["type"])
	}
	if term["transaction_key"] != "deprecated" {
		t.Errorf("transaction_key = %v, want deprecated", term["transaction_key"])
	}
	if rid, _ := term["request_id"].(string); rid == "" {
		t.Error("terminal Metadata missing request_id")
	}
	if s, _ := term["sha256"].(string); len(s) != 64 {
		t.Errorf("sha256 = %q, want 64-char hex", s)
	}
	if d, _ := term["duration"].(float64); d != 1.0 {
		t.Errorf("duration = %v, want 1.0 (32000 bytes @ 32kB/s PCM16)", d)
	}
	if ch, _ := term["channels"].(float64); ch != 1 {
		t.Errorf("channels = %v, want 1", ch)
	}

	// 3. Server terminates the connection.
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Error("expected connection to terminate after terminal Metadata")
	}
}

// TestDeepgramTeardownPromptWhenSidecarDiesMidFlush verifies the grace
// wait bails out immediately when the sidecar goroutine exits, instead
// of blocking teardown for the full grace period.
func TestDeepgramTeardownPromptWhenSidecarDiesMidFlush(t *testing.T) {
	lm := captureLogManager()
	sidecarWS := startTestApp(t, mockSidecarDiesOnFinalize())
	handlerWS := startTestApp(t, deepgramTestApp(t, sidecarWS, pii.NewRedactor(nil, false, nil, 0), lm))

	conn, _, err := ws.DefaultDialer.Dial(handlerWS+"/v1/listen", nil)
	if err != nil {
		t.Fatalf("dial handler: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	var meta map[string]any
	if err := conn.ReadJSON(&meta); err != nil {
		t.Fatalf("read Metadata: %v", err)
	}
	if err := conn.WriteMessage(ws.BinaryMessage, make([]byte, 3200)); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	if err := conn.WriteMessage(ws.TextMessage, []byte(`{"type":"Finalize"}`)); err != nil {
		t.Fatalf("write Finalize: %v", err)
	}
	halfClose(t, conn)

	// Flush is pending but the sidecar died — SESSION_ENDED must arrive
	// promptly, well under the 10s grace period.
	start := time.Now()
	ended := waitForLogEvent(lm, "DEEPGRAM_SESSION_ENDED", 5*time.Second)
	if ended == nil {
		t.Fatal("DEEPGRAM_SESSION_ENDED never emitted")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("teardown took %v with a dead sidecar — grace wait did not bail out early", elapsed)
	}
}

// TestDeepgramBinaryControlMessage verifies Postel-law handling of
// clients that send Deepgram control messages as BINARY frames (spec
// says text): a binary {"type":"Finalize"} must still flush the stream
// and yield a from_finalize Results event, while binary audio that
// happens to start with '{' must NOT be misclassified.
func TestDeepgramBinaryControlMessage(t *testing.T) {
	lm := captureLogManager()
	sidecarWS := startTestApp(t, mockSidecarApp())
	handlerWS := startTestApp(t, deepgramTestApp(t, sidecarWS, pii.NewRedactor(nil, false, nil, 0), lm))

	conn, _, err := ws.DefaultDialer.Dial(handlerWS+"/v1/listen", nil)
	if err != nil {
		t.Fatalf("dial handler: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	var meta map[string]any
	if err := conn.ReadJSON(&meta); err != nil {
		t.Fatalf("read Metadata: %v", err)
	}

	// Audio frame deliberately starting with '{' — must be treated as
	// audio (it is not valid JSON), not a control message.
	audio := append([]byte("{not json, just pcm coincidence"), make([]byte, 958)...)
	if err := conn.WriteMessage(ws.BinaryMessage, audio); err != nil {
		t.Fatalf("write audio: %v", err)
	}

	// Finalize sent as a BINARY frame — misbehaving-client compat.
	if err := conn.WriteMessage(ws.BinaryMessage, []byte(`{"type":"Finalize"}`)); err != nil {
		t.Fatalf("write binary Finalize: %v", err)
	}

	var res map[string]any
	if err := conn.ReadJSON(&res); err != nil {
		t.Fatalf("read Results after binary Finalize: %v", err)
	}
	if res["type"] != "Results" {
		t.Fatalf("event type = %v, want Results", res["type"])
	}
	if res["from_finalize"] != true {
		t.Errorf("from_finalize = %v, want true", res["from_finalize"])
	}

	// The '{'-prefixed audio frame must have been counted as audio
	// (975 bytes), proving it was not misclassified as a control frame.
	// Close the session to observe the stats.
	if err := conn.WriteMessage(ws.TextMessage, []byte(`{"type":"CloseStream"}`)); err != nil {
		t.Fatalf("write CloseStream: %v", err)
	}
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
	ended := waitForLogEvent(lm, "DEEPGRAM_SESSION_ENDED", 2*time.Second)
	if ended == nil {
		t.Fatal("DEEPGRAM_SESSION_ENDED never emitted")
	}
	if b, _ := ended.AdditionalData["audio_bytes"].(int); b != len(audio) {
		t.Errorf("audio_bytes = %v, want %d ('{'-prefixed frame counted as audio)", b, len(audio))
	}
}

// TestDGSessionStats verifies the mutex-guarded session counters used
// by both Deepgram handlers (run with -race to catch regressions).
func TestDGSessionStats(t *testing.T) {
	s := &dgSessionStats{}

	if got := s.addAudio(100); got != 1 {
		t.Errorf("first addAudio returned %d, want 1", got)
	}
	if got := s.addAudio(200); got != 2 {
		t.Errorf("second addAudio returned %d, want 2", got)
	}
	s.markResult()

	bytes, frames, first, last := s.snapshot()
	if bytes != 300 {
		t.Errorf("bytes = %d, want 300", bytes)
	}
	if frames != 2 {
		t.Errorf("frames = %d, want 2", frames)
	}
	if first.IsZero() {
		t.Error("firstAudioAt should be set after addAudio")
	}
	if last.IsZero() {
		t.Error("lastResultAt should be set after markResult")
	}
	if last.Before(first) {
		t.Errorf("lastResultAt %v before firstAudioAt %v", last, first)
	}

	// Concurrent access must not race (verify with go test -race).
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.addAudio(32)
			s.markResult()
			_, _, _, _ = s.snapshot()
		}()
	}
	wg.Wait()
	if _, frames, _, _ := s.snapshot(); frames != 10 {
		t.Errorf("frames after concurrent adds = %d, want 10", frames)
	}
}
