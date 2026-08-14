package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/shaunagostinho/gotranscribesrv/internal/logging"
	"github.com/shaunagostinho/gotranscribesrv/internal/middleware"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
	"gorm.io/gorm"
)

// LLMHandler proxies the LLM sidecar's OpenAI- and Anthropic-compatible
// endpoints through the Go gateway. The sidecar speaks both dialects
// natively, so the handler is a transparent proxy: request and response
// bodies pass through verbatim. What the gateway adds on top:
//
//   - Auth (JWT / API key) + rate limiting via the authed route group
//   - Per-model token usage tracking (usage_logs.metadata JSONB)
//   - Prometheus metrics + structured logs
//
// Usage extraction by dialect:
//
//	OpenAI non-streaming   → response usage.{prompt,completion}_tokens
//	OpenAI streaming       → terminal chunk's usage object (tee'd from SSE)
//	Anthropic non-streaming→ response usage.{input,output}_tokens
//	Anthropic streaming    → message_start usage.input_tokens +
//	                         message_delta usage.output_tokens (tee'd from SSE)
//	Responses non-streaming→ response usage.{input,output}_tokens
//	Responses streaming    → response.completed event's response.usage +
//	                         full response object (tee'd from SSE)
type LLMHandler struct {
	sc *sidecar.Client
	db *gorm.DB
	lm *logging.LogManager
}

// NewLLMHandler constructs the handler.
func NewLLMHandler(sc *sidecar.Client, db *gorm.DB, lm *logging.LogManager) *LLMHandler {
	return &LLMHandler{sc: sc, db: db, lm: lm}
}

// llmDialect identifies the response wire format for usage extraction.
type llmDialect int

const (
	dialectOpenAI llmDialect = iota
	dialectAnthropic
	dialectResponses // OpenAI Responses API — usage.{input,output}_tokens
	dialectNone      // images — no usage object
)

// ChatCompletions handles POST /v1/chat/completions (OpenAI).
func (h *LLMHandler) ChatCompletions(c *fiber.Ctx) error {
	return h.proxy(c, "/v1/chat/completions", "llm_chat", dialectOpenAI)
}

// Completions handles POST /v1/completions (OpenAI legacy).
func (h *LLMHandler) Completions(c *fiber.Ctx) error {
	return h.proxy(c, "/v1/completions", "llm_completion", dialectOpenAI)
}

// Embeddings handles POST /v1/embeddings (OpenAI).
func (h *LLMHandler) Embeddings(c *fiber.Ctx) error {
	return h.proxy(c, "/v1/embeddings", "llm_embeddings", dialectOpenAI)
}

// Images handles POST /v1/images/generations (OpenAI).
func (h *LLMHandler) Images(c *fiber.Ctx) error {
	return h.proxy(c, "/v1/images/generations", "llm_images", dialectNone)
}

// Messages handles POST /v1/messages (Anthropic).
func (h *LLMHandler) Messages(c *fiber.Ctx) error {
	return h.proxy(c, "/v1/messages", "llm_messages", dialectAnthropic)
}

// ── Model management (admin-only; registered on the admin group) ─────

// ModelStatus proxies GET /models/:id/status.
func (h *LLMHandler) ModelStatus(c *fiber.Ctx) error { return h.modelAction(c, "status") }

// ModelDownload proxies POST /models/:id/download.
func (h *LLMHandler) ModelDownload(c *fiber.Ctx) error { return h.modelAction(c, "download") }

// ModelLoad proxies POST /models/:id/load.
func (h *LLMHandler) ModelLoad(c *fiber.Ctx) error { return h.modelAction(c, "load") }

// ModelUnload proxies POST /models/:id/unload.
func (h *LLMHandler) ModelUnload(c *fiber.Ctx) error { return h.modelAction(c, "unload") }

func (h *LLMHandler) modelAction(c *fiber.Ctx, action string) error {
	id := c.Params("id")
	if id == "" {
		return llmError(c, fiber.StatusBadRequest, "invalid_request_error", "model id is required")
	}
	status, body, err := h.sc.LLMModelAction(c.UserContext(), id, action)
	if err != nil {
		slog.WarnContext(c.UserContext(), "llm sidecar model action failed",
			"model", id, "action", action, "error", err)
		return llmError(c, fiber.StatusBadGateway, "server_error", "LLM service unavailable")
	}
	return passthrough(c, status, "application/json", body)
}

// requestPeek extracts just the fields the gateway needs from the
// proxied body; the body itself is forwarded verbatim.
type requestPeek struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func (h *LLMHandler) proxy(c *fiber.Ctx, path, endpoint string, dialect llmDialect) error {
	start := time.Now()
	body := c.Body()

	var peek requestPeek
	if err := json.Unmarshal(body, &peek); err != nil {
		return llmError(c, fiber.StatusBadRequest, "invalid_request_error", "malformed JSON body")
	}
	if peek.Model == "" {
		return llmError(c, fiber.StatusBadRequest, "invalid_request_error", "model is required")
	}

	slog.DebugContext(c.UserContext(), "proxying LLM request",
		"endpoint", endpoint, "model", peek.Model, "stream", peek.Stream)

	// Cancellable upstream context — canceled when the response is fully
	// forwarded or the client disconnects mid-stream. NOTE: no defer here —
	// the streaming branch's body writer runs after this handler returns,
	// so each branch cancels explicitly when it's truly done.
	ctx, cancel := context.WithCancel(context.Background())

	resp, err := h.sc.ProxyLLM(ctx, fiber.MethodPost, path, body, strings.TrimPrefix(endpoint, "llm_"))
	if err != nil {
		cancel()
		slog.WarnContext(c.UserContext(), "llm sidecar unavailable", "endpoint", endpoint, "error", err)
		return llmError(c, fiber.StatusBadGateway, "server_error", "LLM service unavailable")
	}

	// ── Non-streaming ─────────────────────────────────────────────
	if !peek.Stream {
		defer resp.Body.Close()
		defer cancel()
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return llmError(c, fiber.StatusBadGateway, "server_error", "failed to read LLM response")
		}

		if resp.StatusCode == fiber.StatusOK {
			prompt, completion := extractUsage(dialect, respBody)
			c.Locals("usage_meta", map[string]interface{}{
				"model":             peek.Model,
				"prompt_tokens":     prompt,
				"completion_tokens": completion,
				"total_tokens":      prompt + completion,
				"stream":            false,
			})
		} else {
			logUpstreamError(c, endpoint, peek.Model, resp.StatusCode, respBody)
		}
		// Non-2xx: the usage middleware's failure path logs a RequestLog;
		// the sidecar's OpenAI-style error envelope passes through verbatim.
		return passthrough(c, resp.StatusCode, resp.Header.Get("Content-Type"), respBody)
	}

	// ── Streaming (SSE) ───────────────────────────────────────────
	if resp.StatusCode != fiber.StatusOK {
		// Upstream rejected before streaming started — buffer + passthrough
		// so the usage middleware's failure path records it.
		defer resp.Body.Close()
		defer cancel()
		respBody, _ := io.ReadAll(resp.Body)
		logUpstreamError(c, endpoint, peek.Model, resp.StatusCode, respBody)
		return passthrough(c, resp.StatusCode, resp.Header.Get("Content-Type"), respBody)
	}

	// Usage for streams is only known after the handler returns, so the
	// handler logs it directly at stream end; the "-" sentinel tells the
	// usage middleware to skip classification for this request.
	c.Locals("endpoint_override", "-")

	// Capture everything the stream writer needs BEFORE returning —
	// Fiber recycles the Ctx once the handler exits.
	userID, _ := c.Locals("user_id").(string)
	apiKeyID, _ := c.Locals("api_key_id").(string)
	requestID := middleware.RequestIDFromCtx(c)
	model := peek.Model

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Status(fiber.StatusOK)

	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		defer resp.Body.Close()
		defer cancel()

		usage := teeSSE(resp.Body, w, dialect, cancel)

		processMs := int(time.Since(start).Milliseconds())
		middleware.LogLLMUsage(h.db, userID, apiKeyID, endpoint, model,
			usage.prompt, usage.completion, processMs, true)

		h.lm.SendLog(h.lm.BuildLog("LLM_STREAM_COMPLETED", "LLMStreamCompleted", slog.LevelInfo, map[string]interface{}{
			"endpoint":          endpoint,
			"model":             model,
			"prompt_tokens":     usage.prompt,
			"completion_tokens": usage.completion,
			"process_ms":        processMs,
			"request_id":        requestID,
		}))
	})
	return nil
}

// streamUsage accumulates token counts tee'd from an SSE stream.
type streamUsage struct {
	prompt     int
	completion int
	// responseJSON carries the full terminal response object for dialects
	// that emit one (Responses API response.completed) so the handler can
	// persist it after the stream ends.
	responseJSON []byte
}

// teeSSE copies an SSE stream from src to w frame by frame, flushing after
// every line, while parsing data: payloads for usage stats. cancel is called
// if the client disconnects (write error) so the upstream request aborts.
func teeSSE(src io.Reader, w *bufio.Writer, dialect llmDialect, cancel context.CancelFunc) streamUsage {
	var usage streamUsage
	reader := bufio.NewReader(src)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if payload, ok := sseDataPayload(line); ok {
				parseStreamUsage(dialect, payload, &usage)
			}
			if _, werr := w.Write(line); werr != nil {
				cancel() // client gone — abort upstream
				return usage
			}
			if ferr := w.Flush(); ferr != nil {
				cancel()
				return usage
			}
		}
		if err != nil {
			return usage // EOF or upstream error — stream over
		}
	}
}

// sseDataPayload returns the JSON payload of an SSE "data: " line.
func sseDataPayload(line []byte) ([]byte, bool) {
	s := strings.TrimSpace(string(line))
	if !strings.HasPrefix(s, "data:") {
		return nil, false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	if payload == "" || payload == "[DONE]" {
		return nil, false
	}
	return []byte(payload), true
}

// parseStreamUsage updates usage from one SSE data payload according to
// the response dialect.
func parseStreamUsage(dialect llmDialect, payload []byte, usage *streamUsage) {
	switch dialect {
	case dialectOpenAI:
		// Terminal chat chunk carries the full usage object.
		var chunk struct {
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(payload, &chunk) == nil && chunk.Usage != nil {
			usage.prompt = chunk.Usage.PromptTokens
			usage.completion = chunk.Usage.CompletionTokens
		}
	case dialectResponses:
		// Terminal response.completed event carries the full response object.
		var ev struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
		}
		if json.Unmarshal(payload, &ev) != nil || ev.Type != "response.completed" {
			return
		}
		usage.responseJSON = ev.Response
		var r struct {
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(ev.Response, &r) == nil && r.Usage != nil {
			usage.prompt = r.Usage.InputTokens
			usage.completion = r.Usage.OutputTokens
		}
	case dialectAnthropic:
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Usage struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(payload, &ev) != nil {
			return
		}
		switch ev.Type {
		case "message_start":
			usage.prompt = ev.Message.Usage.InputTokens
		case "message_delta":
			usage.completion = ev.Usage.OutputTokens
		}
	}
}

// extractUsage pulls token counts from a non-streaming response body.
func extractUsage(dialect llmDialect, body []byte) (prompt, completion int) {
	switch dialect {
	case dialectOpenAI:
		var resp struct {
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(body, &resp) == nil && resp.Usage != nil {
			return resp.Usage.PromptTokens, resp.Usage.CompletionTokens
		}
	case dialectResponses:
		var resp struct {
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(body, &resp) == nil && resp.Usage != nil {
			return resp.Usage.InputTokens, resp.Usage.OutputTokens
		}
	case dialectAnthropic:
		var resp struct {
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(body, &resp) == nil && resp.Usage != nil {
			return resp.Usage.InputTokens, resp.Usage.OutputTokens
		}
	}
	return 0, 0
}

// upstreamErrorSummary pulls message/type/code out of a sidecar error
// envelope (OpenAI-style {"error": {...}}) for logging.
func upstreamErrorSummary(body []byte) (errType, message, code string) {
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil {
		return env.Error.Type, env.Error.Message, env.Error.Code
	}
	return "", "", ""
}

// logUpstreamError warns with the sidecar's rejection reason so a 4xx/5xx
// passthrough is diagnosable from gateway logs alone (the usage middleware
// only records the status + code, not the message).
func logUpstreamError(c *fiber.Ctx, endpoint, model string, status int, body []byte) {
	errType, message, code := upstreamErrorSummary(body)
	slog.WarnContext(c.UserContext(), "llm sidecar rejected request",
		"endpoint", endpoint, "model", model, "status", status,
		"error_type", errType, "error_code", code, "error_message", message)
}

// passthrough writes an upstream response to the client unchanged.
func passthrough(c *fiber.Ctx, status int, contentType string, body []byte) error {
	if contentType != "" {
		c.Set("Content-Type", contentType)
	}
	return c.Status(status).Send(body)
}

// llmError renders an OpenAI-style error envelope (matches the sidecar's
// own error shape so clients see one consistent format).
func llmError(c *fiber.Ctx, status int, errType, message string) error {
	return c.Status(status).JSON(fiber.Map{
		"error": fiber.Map{
			"message": message,
			"type":    errType,
			"code":    nil,
		},
	})
}
