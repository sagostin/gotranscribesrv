package handlers

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"
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
	usage := teeSSE(strings.NewReader(stream), w, dialectOpenAI, context.CancelFunc(func() {}))

	if out.String() != stream {
		t.Errorf("stream was not passed through verbatim\ngot:  %q\nwant: %q", out.String(), stream)
	}
	if usage.prompt != 14 || usage.completion != 2 {
		t.Errorf("usage = (%d, %d), want (14, 2)", usage.prompt, usage.completion)
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
	usage := teeSSE(strings.NewReader(stream), w, dialectAnthropic, context.CancelFunc(func() {}))

	if out.String() != stream {
		t.Errorf("stream was not passed through verbatim")
	}
	if usage.prompt != 25 || usage.completion != 7 {
		t.Errorf("usage = (%d, %d), want (25, 7)", usage.prompt, usage.completion)
	}
}

func TestTeeSSE_NoUsageFrames(t *testing.T) {
	// Legacy completions stream has no usage object on the wire.
	stream := "data: {\"object\":\"text_completion\",\"choices\":[{\"index\":0,\"text\":\"Hi\",\"finish_reason\":null}]}\n\n" +
		"data: [DONE]\n\n"

	var out bytes.Buffer
	w := bufio.NewWriter(&out)
	usage := teeSSE(strings.NewReader(stream), w, dialectOpenAI, context.CancelFunc(func() {}))

	if out.String() != stream {
		t.Errorf("stream was not passed through verbatim")
	}
	if usage.prompt != 0 || usage.completion != 0 {
		t.Errorf("usage = (%d, %d), want (0, 0)", usage.prompt, usage.completion)
	}
}
