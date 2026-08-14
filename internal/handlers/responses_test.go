package handlers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestExtractUsage_Responses(t *testing.T) {
	body := []byte(`{"id":"resp_1","object":"response","usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}`)
	p, c := extractUsage(dialectResponses, body)
	if p != 11 || c != 7 {
		t.Errorf("extractUsage(responses) = (%d, %d), want (11, 7)", p, c)
	}
}

func TestTeeSSE_ResponsesUsage(t *testing.T) {
	stream := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"delta\":\"Hi\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[{\"type\":\"message\"}],\"usage\":{\"input_tokens\":11,\"output_tokens\":7,\"total_tokens\":18}}}\n\n"

	var out bytes.Buffer
	w := bufio.NewWriter(&out)
	usage := teeSSE(strings.NewReader(stream), w, dialectResponses, context.CancelFunc(func() {}), nil)

	if out.String() != stream {
		t.Errorf("stream was not passed through verbatim\ngot:  %q\nwant: %q", out.String(), stream)
	}
	if usage.prompt != 11 || usage.completion != 7 {
		t.Errorf("usage = (%d, %d), want (11, 7)", usage.prompt, usage.completion)
	}
	if len(usage.responseJSON) == 0 {
		t.Fatal("expected responseJSON to capture the terminal response object")
	}
	var resp struct {
		ID     string            `json:"id"`
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(usage.responseJSON, &resp); err != nil || resp.ID != "resp_1" || len(resp.Output) != 1 {
		t.Errorf("captured response = %+v, err = %v", resp, err)
	}
}

func TestNormalizeInputItems_String(t *testing.T) {
	items, err := normalizeInputItems(json.RawMessage(`"hello"`))
	if err != nil || len(items) != 1 {
		t.Fatalf("normalizeInputItems(string) = %v, %v", items, err)
	}
	var item struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(items[0], &item); err != nil {
		t.Fatal(err)
	}
	if item.Type != "message" || item.Role != "user" ||
		len(item.Content) != 1 || item.Content[0].Type != "input_text" || item.Content[0].Text != "hello" {
		t.Errorf("unexpected normalized item: %+v", item)
	}
}

func TestNormalizeInputItems_Array(t *testing.T) {
	items, err := normalizeInputItems(json.RawMessage(`[{"type":"message","role":"user","content":"a"},{"type":"function_call","name":"f"}]`))
	if err != nil || len(items) != 2 {
		t.Errorf("normalizeInputItems(array) = %d items, %v; want 2 items", len(items), err)
	}
}

func TestNormalizeInputItems_Empty(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, []byte(`null`), []byte(``)} {
		items, err := normalizeInputItems(raw)
		if err != nil || items != nil {
			t.Errorf("normalizeInputItems(%q) = %v, %v; want nil, nil", raw, items, err)
		}
	}
	if _, err := normalizeInputItems(json.RawMessage(`42`)); err == nil {
		t.Error("expected error for non-string non-array input")
	}
}

func TestResponsesPeek_ConversationID(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"model":"m","conversation":"conv_123"}`, "conv_123"},
		{`{"model":"m","conversation":{"id":"conv_456"}}`, "conv_456"},
		{`{"model":"m"}`, ""},
		{`{"model":"m","conversation":null}`, ""},
	}
	for _, tc := range cases {
		var peek responsesPeek
		if err := json.Unmarshal([]byte(tc.body), &peek); err != nil {
			t.Fatal(err)
		}
		if got := peek.conversationID(); got != tc.want {
			t.Errorf("conversationID(%s) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestResponsesPeek_StoreEnabled(t *testing.T) {
	var def responsesPeek
	if !def.storeEnabled() {
		t.Error("store should default to enabled")
	}
	off := false
	withOff := responsesPeek{Store: &off}
	if withOff.storeEnabled() {
		t.Error("store=false should disable persistence")
	}
}
