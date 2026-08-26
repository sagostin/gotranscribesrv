package handlers

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ws "github.com/fasthttp/websocket"
	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/shaunagostinho/gotranscribesrv/internal/config"
	"github.com/shaunagostinho/gotranscribesrv/internal/logging"
	"github.com/shaunagostinho/gotranscribesrv/internal/metrics"
	"github.com/shaunagostinho/gotranscribesrv/internal/pii"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
)

// mockRealtimeSidecarAppWithText mimics the sidecar's /stream/realtime
// protocol: ready on connect, then a single final (with the given text
// and speech_final flag) after the first binary audio frame.
func mockRealtimeSidecarAppWithText(finalText string, speechFinal bool) *fiber.App {
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
				"text":         finalText,
				"speech_final": speechFinal,
				"time":         1.0,
			})
			return // close → ends the handler session
		}
	}))
	return app
}

// fakePresidioJohnServer returns a Presidio analyzer stub that tags
// "john" as PERSON and "212-555-1234" as PHONE_NUMBER.
func fakePresidioJohnServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]sidecar.PresidioEntity{
			{Start: 5, End: 9, Score: 0.95, EntityType: "PERSON"},
			{Start: 13, End: 25, Score: 0.98, EntityType: "PHONE_NUMBER"},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeOpenAIAppend sends one input_audio_buffer.append event (base64
// PCM) — the only way audio enters the OpenAI realtime protocol.
func writeOpenAIAppend(t *testing.T, conn *ws.Conn) {
	t.Helper()
	frame := base64.StdEncoding.EncodeToString(make([]byte, 640))
	if err := conn.WriteJSON(fiber.Map{
		"type":  "input_audio_buffer.append",
		"audio": frame,
	}); err != nil {
		t.Fatalf("write append: %v", err)
	}
}

// readUntilType reads client events until one with the given "type"
// arrives (intervening events are discarded).
func readUntilType(t *testing.T, conn *ws.Conn, wantType string) map[string]any {
	t.Helper()
	for {
		var ev map[string]any
		if err := conn.ReadJSON(&ev); err != nil {
			t.Fatalf("read event (waiting for %s): %v", wantType, err)
		}
		if ev["type"] == wantType {
			return ev
		}
	}
}

// TestOpenAIRealtimeFinalRedactsPIIForLokiButNotClient verifies the
// privacy boundary on the OpenAI realtime transcription proxy: the
// client receives the RAW transcript, while the
// OPENAI_REALTIME_FINAL_SENT event shipped toward Loki carries the
// PII-redacted form plus redaction metadata.
func TestOpenAIRealtimeFinalRedactsPIIForLokiButNotClient(t *testing.T) {
	const rawText = "call john at 212-555-1234"

	presidio := fakePresidioJohnServer(t)
	redactor := pii.NewRedactor(sidecar.NewPresidioClient(presidio.URL, time.Second), true, nil, 0)
	lm := captureLogManager()

	sc := sidecar.NewClient("http://unused", startTestApp(t, mockRealtimeSidecarAppWithText(rawText, true)), "http://unused-llm")
	metricsOnce.Do(func() { metrics.Init(true) })
	h := NewOpenAIRealtimeHandler(sc, redactor, lm, nil)

	app := fiber.New()
	app.Use("/v1/realtime", func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})
	app.Get("/v1/realtime", h.Upgrade())
	handlerWS := startTestApp(t, app)

	conn, _, err := ws.DefaultDialer.Dial(handlerWS+"/v1/realtime?model=gpt-4o-realtime", nil)
	if err != nil {
		t.Fatalf("dial handler: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	readUntilType(t, conn, "session.created")
	writeOpenAIAppend(t, conn)

	// Client must receive the RAW transcript — redaction is log-path only.
	completed := readUntilType(t, conn, "conversation.item.input_audio_transcription.completed")
	if completed["transcript"] != rawText {
		t.Errorf("client transcript = %v, want raw %q (redaction must not leak into client responses)",
			completed["transcript"], rawText)
	}

	finalSent := waitForLogEvent(lm, "OPENAI_REALTIME_FINAL_SENT", 2*time.Second)
	if finalSent == nil {
		t.Fatal("OPENAI_REALTIME_FINAL_SENT never emitted")
	}
	got, _ := finalSent.AdditionalData["transcript"].(string)
	want := "call <PERSON> at <PHONE_NUMBER>"
	if got != want {
		t.Errorf("Loki transcript = %q, want redacted %q", got, want)
	}
	if n, _ := finalSent.AdditionalData["pii_redacted"].(int); n != 2 {
		t.Errorf("pii_redacted = %v, want 2", finalSent.AdditionalData["pii_redacted"])
	}
}

// TestOpenAIRealtimeS2SFinalRedactsPIIForLokiButNotClient verifies the
// same boundary on the speech-to-speech handler: raw transcript to the
// client, PII-redacted transcript on the
// OPENAI_REALTIME_S2S_TRANSCRIPT_SENT Loki event.
//
// speech_final=false so the turn never ends — no LLM round-trip is
// triggered and the test stays hermetic.
func TestOpenAIRealtimeS2SFinalRedactsPIIForLokiButNotClient(t *testing.T) {
	const rawText = "call john at 212-555-1234"

	presidio := fakePresidioJohnServer(t)
	redactor := pii.NewRedactor(sidecar.NewPresidioClient(presidio.URL, time.Second), true, nil, 0)
	lm := captureLogManager()

	sc := sidecar.NewClient("http://unused", startTestApp(t, mockRealtimeSidecarAppWithText(rawText, false)), "http://unused-llm")
	metricsOnce.Do(func() { metrics.Init(true) })
	h := NewOpenAIRealtimeS2SHandler(sc, redactor, lm, nil, &config.Config{})

	app := fiber.New()
	app.Use("/v1/realtime", func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})
	app.Get("/v1/realtime", h.Upgrade())
	handlerWS := startTestApp(t, app)

	conn, _, err := ws.DefaultDialer.Dial(handlerWS+"/v1/realtime?model=gpt-realtime", nil)
	if err != nil {
		t.Fatalf("dial handler: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	readUntilType(t, conn, "session.created")
	writeOpenAIAppend(t, conn)

	completed := readUntilType(t, conn, "conversation.item.input_audio_transcription.completed")
	if completed["transcript"] != rawText {
		t.Errorf("client transcript = %v, want raw %q (redaction must not leak into client responses)",
			completed["transcript"], rawText)
	}

	sent := waitForLogEvent(lm, "OPENAI_REALTIME_S2S_TRANSCRIPT_SENT", 2*time.Second)
	if sent == nil {
		t.Fatal("OPENAI_REALTIME_S2S_TRANSCRIPT_SENT never emitted")
	}
	got, _ := sent.AdditionalData["transcript"].(string)
	want := "call <PERSON> at <PHONE_NUMBER>"
	if got != want {
		t.Errorf("Loki transcript = %q, want redacted %q", got, want)
	}
	if n, _ := sent.AdditionalData["pii_redacted"].(int); n != 2 {
		t.Errorf("pii_redacted = %v, want 2", sent.AdditionalData["pii_redacted"])
	}
}

// TestLogGuardMasksRawTranscriptField verifies the central guard
// end-to-end through a handler LogManager: if any code path ever puts
// a raw (non-Redacted) string under a sensitive key, the event that
// lands on the Loki channel carries the mask sentinel instead.
func TestLogGuardMasksRawTranscriptField(t *testing.T) {
	lm := captureLogManager()
	lm.SendLog(lm.BuildLog("X", "GenericError", 0, map[string]interface{}{
		"transcript": "call john at 212-555-1234",
	}))

	select {
	case ev := <-lm.LogChannel:
		got, _ := ev.AdditionalData["transcript"].(string)
		if got != logging.UnsafeMaskSentinel {
			t.Errorf("transcript = %q, want mask sentinel %q", got, logging.UnsafeMaskSentinel)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event never reached the Loki channel")
	}
}
