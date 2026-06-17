package middleware

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// TestRequestIDMiddlewareGenerates verifies the middleware assigns
// a fresh UUID and stashes it in c.Locals + the response header.
func TestRequestIDMiddlewareGenerates(t *testing.T) {
	app := fiber.New()
	app.Use(RequestID())
	app.Get("/", func(c *fiber.Ctx) error {
		id := RequestIDFromCtx(c)
		if id == "" {
			t.Error("RequestIDFromCtx returned empty")
		}
		if len(id) != 36 { // UUID string length
			t.Errorf("expected UUID length 36, got %d (%q)", len(id), id)
		}
		return c.SendString("ok")
	})

	req := httptest.NewRequest("GET", "/", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	got := resp.Header.Get(RequestIDHeader)
	if got == "" {
		t.Error("response missing X-Request-ID header")
	}
	if len(got) != 36 {
		t.Errorf("header has wrong UUID length: %d", len(got))
	}
}

// TestRequestIDMiddlewareUniquePerRequest verifies each request gets
// its own UUID (no leaking across requests).
func TestRequestIDMiddlewareUniquePerRequest(t *testing.T) {
	app := fiber.New()
	app.Use(RequestID())
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString(RequestIDFromCtx(c))
	})

	r1, _ := app.Test(httptest.NewRequest("GET", "/", nil))
	r2, _ := app.Test(httptest.NewRequest("GET", "/", nil))

	id1 := r1.Header.Get(RequestIDHeader)
	id2 := r2.Header.Get(RequestIDHeader)
	if id1 == "" || id2 == "" {
		t.Fatal("missing request id in one or both responses")
	}
	if id1 == id2 {
		t.Errorf("expected unique request ids, got %q twice", id1)
	}
}

// TestRequestIDFromCtxEmptyWhenNoMiddleware verifies the helper
// returns "" safely when the middleware did not run (e.g. tests).
func TestRequestIDFromCtxEmptyWhenNoMiddleware(t *testing.T) {
	app := fiber.New()
	app.Get("/", func(c *fiber.Ctx) error {
		if id := RequestIDFromCtx(c); id != "" {
			t.Errorf("expected empty id, got %q", id)
		}
		return c.SendString("ok")
	})
	_, err := app.Test(httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
}

// TestRequestIDIgnoresInboundHeader verifies that even if a client
// supplies X-Request-ID, the server generates its own.
func TestRequestIDIgnoresInboundHeader(t *testing.T) {
	app := fiber.New()
	app.Use(RequestID())
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("ok")
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", "injected-by-client")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	got := resp.Header.Get(RequestIDHeader)
	if got == "injected-by-client" {
		t.Error("server should not echo inbound X-Request-ID")
	}
	if !strings.HasPrefix(got, "") || len(got) != 36 {
		t.Errorf("expected fresh UUID, got %q", got)
	}
}
