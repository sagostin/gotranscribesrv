package sidecar

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/shaunagostinho/gotranscribesrv/internal/metrics"
)

// ──────────────────────────────────────────────────────────────
// LLM sidecar (Vapor, :8080) — chat, completions, embeddings,
// image generation. Speaks OpenAI (/v1/chat/completions,
// /v1/completions, /v1/embeddings, /v1/images/generations,
// /v1/models) and Anthropic (/v1/messages) dialects natively.
// The Go server proxies these verbatim — auth, rate limiting,
// and per-model token usage tracking live in the gateway.
// ──────────────────────────────────────────────────────────────

// LLMModel is one entry from the LLM sidecar's GET /v1/models.
// Extra fields beyond the OpenAI model-object spec (kind, runtime,
// status, ...) are passed through to clients.
type LLMModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	Kind    string `json:"kind,omitempty"`    // chat | image | embedding
	Runtime string `json:"runtime,omitempty"` // standard | coreml-llm
	Repo    string `json:"repo,omitempty"`
	Status  string `json:"status,omitempty"`
	Preload bool   `json:"preload"`
	Notes   string `json:"notes,omitempty"`
}

// LLMModelList is the OpenAI-style list envelope from the LLM sidecar.
type LLMModelList struct {
	Object string     `json:"object"`
	Data   []LLMModel `json:"data"`
}

// ProxyLLM forwards a request to the LLM sidecar and returns the raw
// upstream response. The caller owns resp.Body. Uses streamClient (no
// global timeout) so SSE responses can stay open as long as needed —
// pass a cancellable ctx so client disconnects abort the upstream.
// operation is the metrics label (e.g. "chat", "messages").
func (c *Client) ProxyLLM(ctx context.Context, method, path string, body []byte, operation string) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, method, c.llmURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	start := time.Now()
	resp, err := c.streamClient.Do(httpReq)
	durationMs := int(time.Since(start).Milliseconds())
	metrics.RecordSidecarLatency("llm", operation, durationMs, err)
	if err != nil {
		return nil, fmt.Errorf("llm sidecar request failed: %w", err)
	}
	return resp, nil
}

// ──────────────────────────────────────────────────────────────
// Streaming chat (used by the realtime speech-to-speech
// orchestrator — see docs/realtime.md). Unlike ProxyLLM, which
// forwards raw bytes for the gateway proxy handlers, StreamChat
// parses the SSE stream into typed chunks for the orchestrator.
// ──────────────────────────────────────────────────────────────

// ChatMessage is one OpenAI-style chat message. ToolCalls carries the
// assistant's tool invocations; ToolCallID links a role:"tool" result
// message back to its call.
type ChatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
	ToolCalls  []ChatToolCall `json:"tool_calls,omitempty"`
}

// ChatToolCall is one tool invocation. In streamed deltas it arrives as a
// fragment — Index identifies which call, ID/Name appear on the first
// fragment, and Arguments is concatenated across fragments.
type ChatToolCall struct {
	Index    int                  `json:"index,omitempty"`
	ID       string               `json:"id,omitempty"`
	Type     string               `json:"type,omitempty"` // "function"
	Function ChatToolCallFunction `json:"function"`
}

// ChatToolCallFunction holds the function name and (possibly partial)
// JSON arguments string.
type ChatToolCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ChatCompletionRequest is the OpenAI chat-completions request body the
// orchestrator sends to the LLM sidecar. Tools/ToolChoice pass through
// verbatim from the realtime session config.
type ChatCompletionRequest struct {
	Model       string            `json:"model"`
	Messages    []ChatMessage     `json:"messages"`
	Tools       []json.RawMessage `json:"tools,omitempty"`
	ToolChoice  any               `json:"tool_choice,omitempty"`
	Temperature float64           `json:"temperature,omitempty"`
	MaxTokens   int               `json:"max_tokens,omitempty"`
	Stream      bool              `json:"stream"`
}

// ChatUsage is the token-usage block attached to the final SSE chunk.
type ChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatStreamChunk is one parsed SSE event from a streaming chat completion.
// Err is set exactly once on the terminal chunk when the stream fails;
// the channel closes immediately after.
type ChatStreamChunk struct {
	Content      string         // delta text (may be "")
	ToolCalls    []ChatToolCall // delta tool-call fragments
	FinishReason string         // "stop" | "tool_calls" | "length" | ...
	Usage        *ChatUsage     // set on the final chunk, if provided
	Err          error
}

// StreamChat posts a streaming chat completion to the LLM sidecar
// (POST /v1/chat/completions, stream:true) and returns a channel of parsed
// chunks. The channel closes when the stream ends (after "data: [DONE]" or
// on error/cancellation). Cancel ctx to abort mid-stream (barge-in).
// requestID is sent as X-Request-ID so sidecar logs correlate with the
// orchestrator's turn (empty = no header).
func (c *Client) StreamChat(ctx context.Context, req ChatCompletionRequest, requestID string) (<-chan ChatStreamChunk, error) {
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.llmURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if requestID != "" {
		httpReq.Header.Set("X-Request-ID", requestID)
	}

	start := time.Now()
	resp, err := c.streamClient.Do(httpReq)
	metrics.RecordSidecarLatency("llm", "realtime_chat", int(time.Since(start).Milliseconds()), err)
	if err != nil {
		return nil, fmt.Errorf("llm sidecar request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("llm sidecar returned %d: %s", resp.StatusCode, string(errBody))
	}

	ch := make(chan ChatStreamChunk, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue // comments, event: lines, blank keep-alives
			}
			payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if bytes.Equal(payload, []byte("[DONE]")) {
				return
			}

			var raw struct {
				Choices []struct {
					Delta struct {
						Content   string         `json:"content"`
						ToolCalls []ChatToolCall `json:"tool_calls"`
					} `json:"delta"`
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
				Usage *ChatUsage `json:"usage"`
			}
			if err := json.Unmarshal(payload, &raw); err != nil {
				continue // tolerate keep-alive junk / partial frames
			}
			var chunk ChatStreamChunk
			if len(raw.Choices) > 0 {
				chunk.Content = raw.Choices[0].Delta.Content
				chunk.ToolCalls = raw.Choices[0].Delta.ToolCalls
				chunk.FinishReason = raw.Choices[0].FinishReason
			}
			chunk.Usage = raw.Usage
			select {
			case ch <- chunk:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			ch <- ChatStreamChunk{Err: fmt.Errorf("read llm stream: %w", err)}
		}
	}()
	return ch, nil
}

// LLMModels fetches the live model registry from the LLM sidecar
// (GET /v1/models). Returns nil (no error) when the sidecar is
// unreachable so callers can degrade gracefully.
func (c *Client) LLMModels() *LLMModelList {
	if c.llmURL == "" {
		return nil
	}
	resp, err := c.httpClient.Get(c.llmURL + "/v1/models")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var list LLMModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil
	}
	return &list
}

// LLMModelAction proxies a model-management call to the LLM sidecar.
// action is one of "status" (GET), "download", "load", "unload" (POST).
// Returns the upstream status code and body verbatim.
func (c *Client) LLMModelAction(ctx context.Context, id, action string) (int, []byte, error) {
	method := http.MethodPost
	if action == "status" {
		method = http.MethodGet
	}
	resp, err := c.ProxyLLM(ctx, method, "/models/"+id+"/"+action, nil, "model_"+action)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("read llm sidecar response: %w", err)
	}
	return resp.StatusCode, respBody, nil
}
