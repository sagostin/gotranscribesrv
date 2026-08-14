package handlers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/shaunagostinho/gotranscribesrv/internal/middleware"
	"github.com/shaunagostinho/gotranscribesrv/internal/models"
	"gorm.io/gorm"
)

// This file implements the OpenAI Responses API surface on the gateway:
//
//	POST /v1/responses — proxied to the LLM sidecar (which speaks the
//	dialect natively). The gateway adds conversation state on top: when the
//	request carries `conversation` or `previous_response_id`, stored items
//	are loaded from Postgres and prepended to `input` before proxying —
//	exactly what the spec says the server does. After a successful
//	completion (unless store=false) the response and its items are
//	persisted so later requests can chain off them.
//
//	/v1/conversations* — CRUD over the stored conversations (conversations.go).

// responsesPeek extracts the fields the gateway needs from a Responses
// request body; the body itself is forwarded (possibly rewritten) to the
// sidecar.
type responsesPeek struct {
	Model              string          `json:"model"`
	Stream             bool            `json:"stream"`
	Store              *bool           `json:"store"`
	Input              json.RawMessage `json:"input"`
	Conversation       json.RawMessage `json:"conversation"` // string id or {"id": ...}
	PreviousResponseID string          `json:"previous_response_id"`
}

// conversationID extracts the conversation id from the `conversation` param
// (string or {"id": "..."} object form).
func (p *responsesPeek) conversationID() string {
	if len(p.Conversation) == 0 {
		return ""
	}
	trimmed := bytes.TrimSpace(p.Conversation)
	if len(trimmed) == 0 {
		return ""
	}
	if trimmed[0] == '"' {
		var id string
		if err := json.Unmarshal(trimmed, &id); err == nil {
			return id
		}
		return ""
	}
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(trimmed, &obj); err == nil {
		return obj.ID
	}
	return ""
}

func (p *responsesPeek) storeEnabled() bool {
	return p.Store == nil || *p.Store
}

// normalizeInputItems converts the Responses `input` param (string shorthand
// or items array) into a flat list of item JSON values.
func normalizeInputItems(raw json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	trimmed := bytes.TrimSpace(raw)
	switch trimmed[0] {
	case '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, fmt.Errorf("invalid input string: %w", err)
		}
		item, _ := json.Marshal(map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]interface{}{
				{"type": "input_text", "text": text},
			},
		})
		return []json.RawMessage{item}, nil
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, fmt.Errorf("invalid input items: %w", err)
		}
		return items, nil
	default:
		return nil, fmt.Errorf("input must be a string or an array of items")
	}
}

// maxResponseChainDepth caps previous_response_id chain walking.
const maxResponseChainDepth = 50

// loadChainItems reconstructs the item history for a previous_response_id
// chain, oldest first: each stored response contributes its input items
// followed by its output items.
func loadChainItems(db *gorm.DB, userID uuid.UUID, responseID string) ([]json.RawMessage, error) {
	var chain []models.ResponseRecord
	current := responseID
	for len(chain) < maxResponseChainDepth && current != "" {
		var rec models.ResponseRecord
		if err := db.Where("id = ? AND user_id = ?", current, userID).First(&rec).Error; err != nil {
			if err == gorm.ErrRecordNotFound && len(chain) == 0 {
				return nil, err
			}
			break // partial chain — use what we have
		}
		chain = append(chain, rec)
		current = rec.PreviousResponseID
	}
	var items []json.RawMessage
	for i := len(chain) - 1; i >= 0; i-- {
		var input, output []json.RawMessage
		_ = json.Unmarshal([]byte(chain[i].Input), &input)
		_ = json.Unmarshal([]byte(chain[i].Output), &output)
		items = append(items, input...)
		items = append(items, output...)
	}
	return items, nil
}

// Responses handles POST /v1/responses (OpenAI Responses API).
func (h *LLMHandler) Responses(c *fiber.Ctx) error {
	start := time.Now()
	body := c.Body()

	var peek responsesPeek
	if err := json.Unmarshal(body, &peek); err != nil {
		return llmError(c, fiber.StatusBadRequest, "invalid_request_error", "malformed JSON body")
	}
	if peek.Model == "" {
		return llmError(c, fiber.StatusBadRequest, "invalid_request_error", "model is required")
	}

	userIDStr, _ := c.Locals("user_id").(string)
	userID, _ := uuid.Parse(userIDStr)
	apiKeyIDStr, _ := c.Locals("api_key_id").(string)

	// ── Conversation state: resolve history and rewrite the body ─────
	conversationID := peek.conversationID()
	var history []json.RawMessage
	if conversationID != "" {
		items, err := loadConversationItems(h.db, userID, conversationID)
		if err != nil {
			return llmError(c, fiber.StatusNotFound, "invalid_request_error",
				"Conversation not found: "+conversationID)
		}
		history = items
	} else if peek.PreviousResponseID != "" {
		items, err := loadChainItems(h.db, userID, peek.PreviousResponseID)
		if err != nil {
			return llmError(c, fiber.StatusNotFound, "invalid_request_error",
				"Response not found: "+peek.PreviousResponseID)
		}
		history = items
	}

	if len(history) > 0 {
		clientItems, err := normalizeInputItems(peek.Input)
		if err != nil {
			return llmError(c, fiber.StatusBadRequest, "invalid_request_error", err.Error())
		}
		var rewritten map[string]interface{}
		if err := json.Unmarshal(body, &rewritten); err != nil {
			return llmError(c, fiber.StatusBadRequest, "invalid_request_error", "malformed JSON body")
		}
		combined := append(history, clientItems...)
		raw := make([]interface{}, 0, len(combined))
		for _, item := range combined {
			var v interface{}
			if err := json.Unmarshal(item, &v); err == nil {
				raw = append(raw, v)
			}
		}
		rewritten["input"] = raw
		if body, err = json.Marshal(rewritten); err != nil {
			return llmError(c, fiber.StatusBadRequest, "invalid_request_error", "failed to rebuild request body")
		}
	}

	slog.DebugContext(c.UserContext(), "proxying Responses request",
		"model", peek.Model, "stream", peek.Stream,
		"conversation", conversationID, "previous_response_id", peek.PreviousResponseID,
		"history_items", len(history))

	// Cancellable upstream context — see proxy() for the cancellation rules.
	ctx, cancel := context.WithCancel(context.Background())

	resp, err := h.sc.ProxyLLM(ctx, fiber.MethodPost, "/v1/responses", body, "responses")
	if err != nil {
		cancel()
		slog.WarnContext(c.UserContext(), "llm sidecar unavailable", "endpoint", "llm_responses", "error", err)
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
			prompt, completion := extractUsage(dialectResponses, respBody)
			c.Locals("usage_meta", map[string]interface{}{
				"model":             peek.Model,
				"prompt_tokens":     prompt,
				"completion_tokens": completion,
				"total_tokens":      prompt + completion,
				"stream":            false,
			})
			h.persistResponse(userID, apiKeyIDStr, peek, respBody)
		}
		return passthrough(c, resp.StatusCode, resp.Header.Get("Content-Type"), respBody)
	}

	// ── Streaming (SSE) ───────────────────────────────────────────
	if resp.StatusCode != fiber.StatusOK {
		defer resp.Body.Close()
		defer cancel()
		respBody, _ := io.ReadAll(resp.Body)
		return passthrough(c, resp.StatusCode, resp.Header.Get("Content-Type"), respBody)
	}

	// Usage is only known after the handler returns — see proxy().
	c.Locals("endpoint_override", "-")

	requestID := middleware.RequestIDFromCtx(c)
	model := peek.Model

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Status(fiber.StatusOK)

	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		defer resp.Body.Close()
		defer cancel()

		usage := teeSSE(resp.Body, w, dialectResponses, cancel)

		processMs := int(time.Since(start).Milliseconds())
		middleware.LogLLMUsage(h.db, userIDStr, apiKeyIDStr, "llm_responses", model,
			usage.prompt, usage.completion, processMs, true)

		if len(usage.responseJSON) > 0 {
			h.persistResponse(userID, apiKeyIDStr, peek, usage.responseJSON)
		}

		h.lm.SendLog(h.lm.BuildLog("LLM_STREAM_COMPLETED", "LLMStreamCompleted", slog.LevelInfo, map[string]interface{}{
			"endpoint":          "llm_responses",
			"model":             model,
			"prompt_tokens":     usage.prompt,
			"completion_tokens": usage.completion,
			"process_ms":        processMs,
			"request_id":        requestID,
		}))
	})
	return nil
}

// persistResponse stores a completed Responses-API response (id + output
// items) so later requests can chain via previous_response_id, and appends
// the turn's items to the conversation when one is attached. Failures are
// logged, never fatal — the client already got its response.
func (h *LLMHandler) persistResponse(userID uuid.UUID, apiKeyID string, peek responsesPeek, responseBody []byte) {
	if !peek.storeEnabled() {
		return
	}
	var parsed struct {
		ID     string            `json:"id"`
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(responseBody, &parsed); err != nil || parsed.ID == "" {
		slog.Warn("responses: could not parse response for persistence", "error", err)
		return
	}

	inputItems, err := normalizeInputItems(peek.Input)
	if err != nil {
		inputItems = nil
	}
	inputJSON, _ := json.Marshal(inputItems)
	outputJSON, _ := json.Marshal(parsed.Output)

	rec := models.ResponseRecord{
		ID:                 parsed.ID,
		UserID:             userID,
		PreviousResponseID: peek.PreviousResponseID,
		ConversationID:     peek.conversationID(),
		Model:              peek.Model,
		Input:              string(inputJSON),
		Output:             string(outputJSON),
	}
	if apiKeyID != "" {
		if akID, err := uuid.Parse(apiKeyID); err == nil {
			rec.APIKeyID = &akID
		}
	}
	if err := h.db.Create(&rec).Error; err != nil {
		slog.Warn("responses: failed to persist response record", "id", parsed.ID, "error", err)
	}

	if rec.ConversationID != "" {
		items := append(inputItems, parsed.Output...)
		if err := appendConversationItems(h.db, rec.ConversationID, items); err != nil {
			slog.Warn("responses: failed to append conversation items",
				"conversation", rec.ConversationID, "error", err)
		}
	}
}
