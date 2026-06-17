package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// TestContextHandlerInjectsRequestIDFromContext verifies that when a
// request id is set on the call's context.Context, the handler adds
// it as a "request_id" attribute on the record.
func TestContextHandlerInjectsRequestIDFromContext(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	h := NewContextHandler(base)
	log := slog.New(h)

	ctx := context.WithValue(context.Background(), RequestIDCtxKey, "abc-123")
	log.InfoContext(ctx, "hello", "foo", "bar")

	line := strings.TrimSpace(buf.String())
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(line), &out); err != nil {
		t.Fatalf("invalid JSON: %v line=%q", err, line)
	}
	if out["request_id"] != "abc-123" {
		t.Errorf("request_id missing or wrong: %v", out)
	}
	if out["msg"] != "hello" {
		t.Errorf("msg missing: %v", out)
	}
	if out["foo"] != "bar" {
		t.Errorf("user attr missing: %v", out)
	}
}

// TestContextHandlerPassesThroughWhenNoID verifies that records
// logged without a request id on the context are not corrupted.
func TestContextHandlerPassesThroughWhenNoID(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	h := NewContextHandler(base)
	log := slog.New(h)

	log.InfoContext(context.Background(), "background work", "k", "v")

	var out map[string]interface{}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, present := out["request_id"]; present {
		t.Errorf("request_id should not be present: %v", out)
	}
	if out["msg"] != "background work" {
		t.Errorf("msg missing: %v", out)
	}
}

// TestRequestIDFromContext verifies the helper extraction.
func TestRequestIDFromContext(t *testing.T) {
	if got := RequestIDFromContext(context.Background()); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
	if got := RequestIDFromContext(nil); got != "" {
		t.Errorf("expected empty for nil ctx, got %q", got)
	}
	ctx := context.WithValue(context.Background(), RequestIDCtxKey, "xyz")
	if got := RequestIDFromContext(ctx); got != "xyz" {
		t.Errorf("expected xyz, got %q", got)
	}
}

// TestContextHandlerWithAttrs verifies that WithAttrs returns a
// working handler and that request_id is still injected on the new
// handler (not lost during attribute propagation).
func TestContextHandlerWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	h := NewContextHandler(base)
	log := slog.New(h).With("service", "gotranscribesrv")

	ctx := context.WithValue(context.Background(), RequestIDCtxKey, "req-1")
	log.InfoContext(ctx, "hi")

	var out map[string]interface{}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if out["request_id"] != "req-1" {
		t.Errorf("request_id lost on WithAttrs: %v", out)
	}
	if out["service"] != "gotranscribesrv" {
		t.Errorf("baked attr lost: %v", out)
	}
}
