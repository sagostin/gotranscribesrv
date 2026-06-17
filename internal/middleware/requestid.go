package middleware

import (
	"context"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/shaunagostinho/gotranscribesrv/internal/logging"
)

const (
	// RequestIDLocalKey is the c.Locals key under which the generated
	// request id is stored. Handlers read it via RequestIDFromCtx.
	RequestIDLocalKey = "request_id"

	// RequestIDHeader is the response header name clients can use
	// to correlate their own logs with server-side events.
	RequestIDHeader = "X-Request-ID"
)

// RequestID returns a Fiber middleware that assigns a fresh UUID to
// every incoming request, stashes it in c.Locals (for handlers) and
// in c.UserContext (for slog.InfoContext and friends), and echoes it
// on the response.
//
// The id is always generated server-side — inbound X-Request-ID
// headers are intentionally ignored. Internal correlation should
// never trust a client-provided value (it could be malformed,
// duplicated, or weaponized for log injection).
//
// Register this as early as possible (before auth/rate-limit) so
// every log line for the request, including 401/429 failures, is
// correlated.
func RequestID() fiber.Handler {
	return func(c *fiber.Ctx) error {
		id := uuid.New().String()
		c.Locals(RequestIDLocalKey, id)
		c.Set(RequestIDHeader, id)
		// Also surface on the user context so slog handlers can
		// read it via context.Value. The ContextHandler installed
		// in main.go auto-injects it as a "request_id" attr.
		ctx := context.WithValue(c.UserContext(), logging.RequestIDCtxKey, id)
		c.SetUserContext(ctx)
		return c.Next()
	}
}

// RequestIDFromCtx returns the request id stashed by the RequestID
// middleware, or empty string if none was set. Safe to call from
// any handler/middleware that runs after RequestID().
func RequestIDFromCtx(c *fiber.Ctx) string {
	if id, ok := c.Locals(RequestIDLocalKey).(string); ok {
		return id
	}
	return ""
}
