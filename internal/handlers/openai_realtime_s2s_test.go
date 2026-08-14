package handlers

import (
	"strings"
	"testing"
)

func TestIsS2SModel(t *testing.T) {
	cases := map[string]bool{
		"gpt-realtime":            true,
		"gpt-realtime-mini":       true,
		"gpt-realtime-2025-08-28": true,
		"GPT-Realtime":            true,
		" gpt-realtime ":          true,
		"gpt-4o-transcribe":       false,
		"gpt-4o-mini-transcribe":  false,
		"gpt-4o-realtime-preview": false,
		"gpt-4o-realtime":         false,
		"eou-320":                 false,
		"nova-3":                  false,
		"":                        false,
	}
	for model, want := range cases {
		if got := IsS2SModel(model); got != want {
			t.Errorf("IsS2SModel(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestSentenceSplitterTerminalPunctuation(t *testing.T) {
	s := newSentenceSplitter()
	s.Write("Hello there.")
	sentence, ok := s.Next()
	if !ok || sentence != "Hello there." {
		t.Fatalf("Next() = %q, %v; want %q, true", sentence, ok, "Hello there.")
	}
	if _, ok := s.Next(); ok {
		t.Fatal("Next() should be false after buffer drained")
	}
}

func TestSentenceSplitterTokenFragments(t *testing.T) {
	s := newSentenceSplitter()
	for _, tok := range []string{"The", " capital", " of", " Japan", " is", " Tokyo", "."} {
		s.Write(tok)
	}
	sentence, ok := s.Next()
	if !ok || sentence != "The capital of Japan is Tokyo." {
		t.Fatalf("Next() = %q, %v", sentence, ok)
	}
}

func TestSentenceSplitterMultipleSentences(t *testing.T) {
	s := newSentenceSplitter()
	s.Write("First one. Second two! And a third?")
	got := []string{}
	for {
		sentence, ok := s.Next()
		if !ok {
			break
		}
		got = append(got, sentence)
	}
	want := []string{"First one.", "Second two!", "And a third?"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSentenceSplitterSoftBoundaryFirstChunkAggressive(t *testing.T) {
	// First chunk: soft boundary flushes at >= 6 words.
	s := newSentenceSplitter()
	s.Write("Well let me think about that, and then")
	sentence, ok := s.Next()
	if !ok || sentence != "Well let me think about that," {
		t.Fatalf("first chunk: Next() = %q, %v", sentence, ok)
	}

	// Later chunks: soft boundary requires >= 10 words.
	s2 := newSentenceSplitter()
	s2.Write("A first sentence.")
	if _, ok := s2.Next(); !ok {
		t.Fatal("expected first sentence flush")
	}
	s2.Write("one two three four five six seven, more")
	if _, ok := s2.Next(); ok {
		t.Fatal("later chunk should wait for 10 words before soft-boundary flush")
	}
	s2.Write(" eight nine ten, trailing")
	sentence, ok = s2.Next()
	if !ok || sentence != "one two three four five six seven, more eight nine ten," {
		t.Fatalf("later chunk: Next() = %q, %v", sentence, ok)
	}
}

func TestSentenceSplitterFlush(t *testing.T) {
	s := newSentenceSplitter()
	s.Write("trailing text with no terminator")
	if _, ok := s.Next(); ok {
		t.Fatal("Next() should not flush without a boundary")
	}
	if tail := s.Flush(); tail != "trailing text with no terminator" {
		t.Fatalf("Flush() = %q", tail)
	}
	if tail := s.Flush(); tail != "" {
		t.Fatalf("second Flush() = %q, want empty", tail)
	}
}

func TestExtractMessageText(t *testing.T) {
	item := map[string]any{
		"type": "message",
		"role": "user",
		"content": []any{
			map[string]any{"type": "input_text", "text": "Hello"},
			map[string]any{"type": "input_text", "text": " world"},
		},
	}
	if got := extractMessageText(item); got != "Hello world" {
		t.Fatalf("extractMessageText = %q, want %q", got, "Hello world")
	}
	if got := extractMessageText(map[string]any{"type": "message"}); got != "" {
		t.Fatalf("extractMessageText(empty) = %q, want empty", got)
	}
}
