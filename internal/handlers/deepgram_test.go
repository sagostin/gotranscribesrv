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
