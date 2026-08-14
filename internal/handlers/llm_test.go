package handlers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestExtractUsage_OpenAI(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-1","usage":{"prompt_tokens":14,"completion_tokens":2,"total_tokens":16}}`)
	p, c := extractUsage(dialectOpenAI, body)
	if p != 14 || c != 2 {
		t.Errorf("extractUsage(openai) = (%d, %d), want (14, 2)", p, c)
	}
}

func TestExtractUsage_Anthropic(t *testing.T) {
	body := []byte(`{"id":"msg_1","usage":{"input_tokens":9,"output_tokens":12}}`)
	p, c := extractUsage(dialectAnthropic, body)
	if p != 9 || c != 12 {
		t.Errorf("extractUsage(anthropic) = (%d, %d), want (9, 12)", p, c)
	}
}

func TestExtractUsage_Missing(t *testing.T) {
	for _, dialect := range []llmDialect{dialectOpenAI, dialectAnthropic, dialectNone} {
		if p, c := extractUsage(dialect, []byte(`{"data":[]}`)); p != 0 || c != 0 {
			t.Errorf("extractUsage(%d) on usage-less body = (%d, %d), want (0, 0)", dialect, p, c)
		}
	}
	if p, c := extractUsage(dialectNone, []byte(`{"usage":{"prompt_tokens":5}}`)); p != 0 || c != 0 {
		t.Errorf("extractUsage(none) = (%d, %d), want (0, 0)", p, c)
	}
}

func TestTeeSSE_PassthroughAndOpenAIUsage(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":14,\"completion_tokens\":2,\"total_tokens\":16}}\n\n" +
		"data: [DONE]\n\n"

	var out bytes.Buffer
	w := bufio.NewWriter(&out)
	usage := teeSSE(strings.NewReader(stream), w, dialectOpenAI, context.CancelFunc(func() {}), nil)

	if out.String() != stream {
		t.Errorf("stream was not passed through verbatim\ngot:  %q\nwant: %q", out.String(), stream)
	}
	if usage.prompt != 14 || usage.completion != 2 {
		t.Errorf("usage = (%d, %d), want (14, 2)", usage.prompt, usage.completion)
	}
	if usage.outcome != streamCompleted {
		t.Errorf("outcome = %q, want %q", usage.outcome, streamCompleted)
	}
	if usage.chunks != 2 || usage.chars != 2 {
		t.Errorf("chunks/chars = (%d, %d), want (2, 2)", usage.chunks, usage.chars)
	}
}

func TestTeeSSE_AnthropicUsage(t *testing.T) {
	stream := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":25,\"output_tokens\":0}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	var out bytes.Buffer
	w := bufio.NewWriter(&out)
	usage := teeSSE(strings.NewReader(stream), w, dialectAnthropic, context.CancelFunc(func() {}), nil)

	if out.String() != stream {
		t.Errorf("stream was not passed through verbatim")
	}
	if usage.prompt != 25 || usage.completion != 7 {
		t.Errorf("usage = (%d, %d), want (25, 7)", usage.prompt, usage.completion)
	}
	if usage.outcome != streamCompleted {
		t.Errorf("outcome = %q, want %q (message_stop marks terminal)", usage.outcome, streamCompleted)
	}
	if usage.chars != 2 {
		t.Errorf("chars = %d, want 2", usage.chars)
	}
}

func TestTeeSSE_NoUsageFrames(t *testing.T) {
	// Legacy completions stream has no usage object on the wire.
	stream := "data: {\"object\":\"text_completion\",\"choices\":[{\"index\":0,\"text\":\"Hi\",\"finish_reason\":null}]}\n\n" +
		"data: [DONE]\n\n"

	var out bytes.Buffer
	w := bufio.NewWriter(&out)
	usage := teeSSE(strings.NewReader(stream), w, dialectOpenAI, context.CancelFunc(func() {}), nil)

	if out.String() != stream {
		t.Errorf("stream was not passed through verbatim")
	}
	if usage.prompt != 0 || usage.completion != 0 {
		t.Errorf("usage = (%d, %d), want (0, 0)", usage.prompt, usage.completion)
	}
	if usage.outcome != streamCompleted {
		t.Errorf("outcome = %q, want %q ([DONE] marks terminal)", usage.outcome, streamCompleted)
	}
}

func TestTeeSSE_UpstreamEOFWithoutTerminal(t *testing.T) {
	// Connection drops mid-stream: no usage chunk, no [DONE].
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n"

	var out bytes.Buffer
	w := bufio.NewWriter(&out)
	usage := teeSSE(strings.NewReader(stream), w, dialectOpenAI, context.CancelFunc(func() {}), nil)

	if usage.outcome != streamUpstreamEOF {
		t.Errorf("outcome = %q, want %q", usage.outcome, streamUpstreamEOF)
	}
	if usage.chunks != 2 || usage.chars != 5 {
		t.Errorf("chunks/chars = (%d, %d), want (2, 5)", usage.chunks, usage.chars)
	}
}

// failAfterWriter fails writes after n bytes — simulates a client
// disconnecting mid-stream.
type failAfterWriter struct {
	n int
}

func (f *failAfterWriter) Write(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errClientGone
	}
	if len(p) > f.n {
		n := f.n
		f.n = 0
		return n, nil
	}
	f.n -= len(p)
	return len(p), nil
}

var errClientGone = errors.New("client disconnected")

func TestTeeSSE_ClientDisconnect(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"two\"}}]}\n\n" +
		"data: [DONE]\n\n"

	cancelled := false
	w := bufio.NewWriter(&failAfterWriter{n: 10}) // first line is ~48 bytes — write fails
	usage := teeSSE(strings.NewReader(stream), w, dialectOpenAI, context.CancelFunc(func() { cancelled = true }), nil)

	if usage.outcome != streamClientDisconnected {
		t.Errorf("outcome = %q, want %q", usage.outcome, streamClientDisconnected)
	}
	if !cancelled {
		t.Error("cancel was not called on client disconnect")
	}
}

func TestTeeSSE_ToolCallNames(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"city\\\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":30,\"completion_tokens\":9}}\n\n" +
		"data: [DONE]\n\n"

	var out bytes.Buffer
	w := bufio.NewWriter(&out)
	usage := teeSSE(strings.NewReader(stream), w, dialectOpenAI, context.CancelFunc(func() {}), nil)

	if len(usage.toolCalls) != 1 || usage.toolCalls[0] != "get_weather" {
		t.Errorf("toolCalls = %v, want [get_weather]", usage.toolCalls)
	}
	if usage.outcome != streamCompleted {
		t.Errorf("outcome = %q, want %q", usage.outcome, streamCompleted)
	}
}

func TestTeeSSE_ResponsesOutcomeAndTools(t *testing.T) {
	stream := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello\"}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"name\":\"search\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"usage\":{\"input_tokens\":11,\"output_tokens\":7}}}\n\n"

	var out bytes.Buffer
	w := bufio.NewWriter(&out)
	usage := teeSSE(strings.NewReader(stream), w, dialectResponses, context.CancelFunc(func() {}), nil)

	if usage.outcome != streamCompleted {
		t.Errorf("outcome = %q, want %q", usage.outcome, streamCompleted)
	}
	if usage.prompt != 11 || usage.completion != 7 {
		t.Errorf("usage = (%d, %d), want (11, 7)", usage.prompt, usage.completion)
	}
	if usage.chars != 5 {
		t.Errorf("chars = %d, want 5", usage.chars)
	}
	if len(usage.toolCalls) != 1 || usage.toolCalls[0] != "search" {
		t.Errorf("toolCalls = %v, want [search]", usage.toolCalls)
	}
	if len(usage.responseJSON) == 0 {
		t.Error("responseJSON empty — response.completed payload not captured")
	}
}

func TestUpstreamErrorSummary(t *testing.T) {
	body := []byte(`{"error":{"message":"Unknown model: gpt-4o. Available models: mistral-7b-int4","type":"invalid_request_error","code":"model_not_found"}}`)
	typ, msg, code := upstreamErrorSummary(body)
	if typ != "invalid_request_error" || code != "model_not_found" || !strings.Contains(msg, "Unknown model: gpt-4o") {
		t.Errorf("upstreamErrorSummary = (%q, %q, %q)", typ, msg, code)
	}
	// Vapor default 404 (no OpenAI envelope) — all empty.
	if typ, msg, code := upstreamErrorSummary([]byte(`{"error":true,"reason":"Not Found"}`)); typ != "" || msg != "" || code != "" {
		t.Errorf("upstreamErrorSummary(non-envelope) = (%q, %q, %q), want all empty", typ, msg, code)
	}
}

func TestLLMErrorCode(t *testing.T) {
	app := fiber.New()
	app.Get("/t", func(c *fiber.Ctx) error {
		return llmError(c, fiber.StatusBadRequest, "invalid_request_error", "model is required", "model_required")
	})
	app.Get("/t-nocode", func(c *fiber.Ctx) error {
		return llmError(c, fiber.StatusBadRequest, "invalid_request_error", "malformed JSON body")
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/t", nil))
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Error struct {
			Code    *string `json:"code"`
			Message string  `json:"message"`
		} `json:"error"`
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code == nil || *body.Error.Code != "model_required" {
		t.Errorf("code = %v, want model_required", body.Error.Code)
	}

	resp2, _ := app.Test(httptest.NewRequest("GET", "/t-nocode", nil))
	var body2 struct {
		Error struct {
			Code *string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&body2)
	if body2.Error.Code != nil {
		t.Errorf("code = %v, want null when no code given", *body2.Error.Code)
	}
}
