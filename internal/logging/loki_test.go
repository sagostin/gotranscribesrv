package logging

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestLokiClientPushLogEndToEnd verifies that a real PushLog call
// marshals to the documented wire format and the server sees the
// expected stream structure.
func TestLokiClientPushLogEndToEnd(t *testing.T) {
	var got LokiPushData
	var hits int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if !strings.HasSuffix(r.URL.Path, "/loki/api/v1/push") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("missing content-type")
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("server decode failed: %v body=%s", err, string(body))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	lc := NewLokiClient(srv.URL, "", "")
	labels := map[string]string{
		"job":   "test",
		"type":  "ASR_COMPLETED",
		"level": "INFO",
	}
	entry := LogEntry{
		Timestamp: time.Unix(0, 1700000000000000000),
		Line:      `{"message":"hi"}`,
	}
	if err := lc.PushLog(labels, entry); err != nil {
		t.Fatalf("PushLog failed: %v", err)
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("expected 1 hit, got %d", got)
	}
	if len(got.Streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(got.Streams))
	}
	stream := got.Streams[0]
	if stream.Stream["type"] != "ASR_COMPLETED" {
		t.Errorf("label not preserved: %v", stream.Stream)
	}
	if len(stream.Values) != 1 {
		t.Fatalf("expected 1 value tuple, got %d", len(stream.Values))
	}
	if stream.Values[0][0] != "1700000000000000000" {
		t.Errorf("timestamp ns wrong: %q", stream.Values[0][0])
	}
	if stream.Values[0][1] != `{"message":"hi"}` {
		t.Errorf("line wrong: %q", stream.Values[0][1])
	}
}

// TestLokiClientBasicAuth verifies the Authorization header is set
// when both username and password are non-empty.
func TestLokiClientBasicAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	lc := NewLokiClient(srv.URL, "user", "pass")
	_ = lc.PushLog(map[string]string{"k": "v"}, LogEntry{Timestamp: time.Now(), Line: "x"})

	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Errorf("expected Basic auth header, got %q", gotAuth)
	}
}

// TestLokiClientNoAuthWhenEmpty verifies no Authorization header
// is set when username or password is empty.
func TestLokiClientNoAuthWhenEmpty(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	lc := NewLokiClient(srv.URL, "", "")
	_ = lc.PushLog(map[string]string{"k": "v"}, LogEntry{Timestamp: time.Now(), Line: "x"})

	if gotAuth != "" {
		t.Errorf("expected no auth header, got %q", gotAuth)
	}
}

// TestLokiClientRejectsNonSuccess verifies that 4xx/5xx responses
// surface as errors (the consumer goroutine logs and drops them).
func TestLokiClientRejectsNonSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	lc := NewLokiClient(srv.URL, "", "")
	err := lc.PushLog(map[string]string{"k": "v"}, LogEntry{Timestamp: time.Now(), Line: "x"})
	if err == nil {
		t.Fatal("expected error on 500 response, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status 500: %v", err)
	}
}
