package logging

import (
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRoundTripper is a minimal http.RoundTripper for tests. We avoid
// pulling in a full HTTP mock library; we just need to count how many
// times PushLog runs and to optionally simulate failures.
type fakeRT struct {
	mu       sync.Mutex
	calls    int32
	failNext atomic.Bool
}

func (f *fakeRT) RoundTrip(_ interface{ Header() interface{} }) (interface{}, error) {
	return nil, nil
}

// TestNewLogManagerSeedsTemplates verifies that the catalog is loaded
// on construction and all expected event names are present.
func TestNewLogManagerSeedsTemplates(t *testing.T) {
	lm := NewLogManager(nil, false)
	defer lm.CloseLogManager()

	expected := []string{
		"ASRRequestReceived", "ASRCompleted", "ASRFailed",
		"WhisperCompleted", "DeepgramSessionStarted", "DeepgramSessionEnded",
		"WatsonSessionStarted", "WatsonSessionEnded",
		"WSASRSessionStarted", "WSASRSessionEnded",
		"TTSCompleted", "TTSFailed",
		"VoiceCloneCompleted", "VoiceCloneFailed",
		"LLMProcessCompleted", "LLMProcessFailed",
		"GenericError",
	}
	for _, name := range expected {
		if _, ok := lm.Templates[strings.ToUpper(name)]; !ok {
			t.Errorf("missing template: %s", name)
		}
	}
}

// TestBuildLogFormatsTemplate verifies fmt.Sprintf is applied to the
// template's %v verbs using the trailing args.
func TestBuildLogFormatsTemplate(t *testing.T) {
	lm := NewLogManager(nil, false)
	defer lm.CloseLogManager()

	log := lm.BuildLog("X", "GenericError", slog.LevelError, map[string]interface{}{"k": "v"}, "boom")
	if log.Message != "An error occurred: boom" {
		t.Errorf("expected formatted message, got %q", log.Message)
	}
	if log.Type != "X" {
		t.Errorf("type not uppercased, got %q", log.Type)
	}
	if log.Level != slog.LevelError {
		t.Errorf("level not preserved, got %v", log.Level)
	}
	if log.AdditionalData["k"] != "v" {
		t.Errorf("additional data not preserved")
	}
	if log.Timestamp.IsZero() {
		t.Errorf("timestamp not set")
	}
}

// TestAddFieldMutatesEntry verifies the helper for late field addition.
func TestAddFieldMutatesEntry(t *testing.T) {
	lm := NewLogManager(nil, false)
	defer lm.CloseLogManager()

	log := lm.BuildLog("X", "ASRCompleted", slog.LevelInfo, nil)
	log.AddField("transcript", "hello world")
	if log.AdditionalData["transcript"] != "hello world" {
		t.Errorf("AddField did not store value")
	}
}

// TestLoggingFormatJSON verifies the wire format includes message,
// type, level, additional_data, timestamp.
func TestLoggingFormatJSON(t *testing.T) {
	lm := NewLogManager(nil, false)
	defer lm.CloseLogManager()

	log := lm.BuildLog("ASR_COMPLETED", "ASRCompleted", slog.LevelInfo, map[string]interface{}{
		"transcript": "hello",
		"word_count": 1,
	})

	var out map[string]interface{}
	if err := json.Unmarshal([]byte(log.String()), &out); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if out["type"] != "ASR_COMPLETED" {
		t.Errorf("type missing in JSON: %v", out)
	}
	if out["message"] != "ASR transcription completed" {
		t.Errorf("message missing in JSON: %v", out)
	}
	ad, ok := out["additional_data"].(map[string]interface{})
	if !ok {
		t.Fatalf("additional_data missing or wrong type: %v", out)
	}
	if ad["transcript"] != "hello" {
		t.Errorf("transcript field not in additional_data: %v", ad)
	}
	if _, ok := out["timestamp"]; !ok {
		t.Errorf("timestamp missing in JSON: %v", out)
	}
}

// TestSendLogNonBlockingWhenDisabled verifies the consumer goroutine
// does not deadlock and SendLog returns immediately when Loki is off.
func TestSendLogNonBlockingWhenDisabled(t *testing.T) {
	lm := NewLogManager(nil, false)
	defer lm.CloseLogManager()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			lm.SendLog(lm.BuildLog("X", "GenericError", slog.LevelInfo, nil, "x"))
		}
		close(done)
	}()
	select {
	case <-done:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("SendLog blocked when Loki disabled")
	}
}

// TestCloseManagerIsIdempotent verifies that calling Close twice does
// not panic.
func TestCloseManagerIsIdempotent(t *testing.T) {
	lm := NewLogManager(nil, false)
	lm.CloseLogManager()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("double-close panicked: %v", r)
		}
	}()
	lm.CloseLogManager()
}

// TestFormatTemplateUnknownFallsBack verifies that an unknown
// template name is used as the format string itself.
func TestFormatTemplateUnknownFallsBack(t *testing.T) {
	lm := NewLogManager(nil, false)
	defer lm.CloseLogManager()

	got := lm.formatTemplate("Unknown%sTemplate", "data")
	if got != "UnknowndataTemplate" {
		t.Errorf("expected fallback format, got %q", got)
	}
}
