package logging

import (
	"context"
	"log/slog"
)

// ContextKey is the unexported type used for context.Value keys inside
// this package, following the stdlib convention that prevents
// collisions with keys defined in other packages.
type ContextKey int

const (
	// RequestIDCtxKey is the context key under which a request id
	// is stored. Set it via c.SetUserContext(ctx with this key) at
	// the start of a request; the ContextHandler will surface it
	// as a slog attr on every record logged within that context.
	RequestIDCtxKey ContextKey = iota
)

// RequestIDFromContext returns the request id stored on the context,
// or empty string if none was set. Useful when a handler wants to
// pass an id explicitly to a non-LogManager API.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(RequestIDCtxKey).(string); ok {
		return v
	}
	return ""
}

// ContextHandler is a slog.Handler that injects a request_id pulled
// from the call's context.Context into every record, then delegates
// formatting to a wrapped base handler.
//
// This is what lets every existing slog.Info / slog.Error / slog.Debug
// call in a handler pick up the request id automatically — provided
// the call is made with the *Context variant of the slog method
// (e.g. slog.InfoContext(ctx, ...)) or via a Logger derived from a
// request-scoped one.
//
// Records logged without a request id on the context pass through
// unchanged, so background goroutines (usage tracker flush, startup,
// admin seed) continue to work normally.
type ContextHandler struct {
	base slog.Handler
}

// NewContextHandler wraps the given base handler. The base is
// responsible for actual formatting (text/json) and the configured
// level.
func NewContextHandler(base slog.Handler) *ContextHandler {
	return &ContextHandler{base: base}
}

// Enabled delegates to the base handler.
func (h *ContextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level)
}

// Handle reads request_id from ctx, prepends it to the record's
// attributes (if present), and forwards to the base handler.
func (h *ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if rid := RequestIDFromContext(ctx); rid != "" {
		r.AddAttrs(slog.String("request_id", rid))
	}
	return h.base.Handle(ctx, r)
}

// WithAttrs returns a new handler that has the given attrs baked in.
// Each record still gets request_id from its own ctx, so nesting is
// safe.
func (h *ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ContextHandler{base: h.base.WithAttrs(attrs)}
}

// WithGroup returns a new handler with the given group applied.
func (h *ContextHandler) WithGroup(name string) slog.Handler {
	return &ContextHandler{base: h.base.WithGroup(name)}
}
