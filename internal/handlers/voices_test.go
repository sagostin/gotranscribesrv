package handlers

import (
	"bytes"
	"testing"
)

func TestTruncateVoiceEmbedding(t *testing.T) {
	frame := func(fill byte) []byte {
		return bytes.Repeat([]byte{fill}, voiceEmbeddingFrameBytes)
	}
	embedding := func(frames int) []byte {
		var b []byte
		for i := 0; i < frames; i++ {
			b = append(b, frame(byte(i%251+1))...)
		}
		return b
	}

	t.Run("250 frames truncated to 125 with prefix preserved", func(t *testing.T) {
		in := embedding(250) // legacy FluidAudio ≤0.13.6 max
		out, ok := truncateVoiceEmbedding(in)
		if !ok {
			t.Fatal("expected truncation to be reported")
		}
		if len(out) != maxVoiceEmbeddingBytes {
			t.Fatalf("expected %d bytes, got %d", maxVoiceEmbeddingBytes, len(out))
		}
		if !bytes.Equal(out, in[:maxVoiceEmbeddingBytes]) {
			t.Fatal("output is not the input prefix")
		}
	})

	t.Run("exactly 125 frames untouched", func(t *testing.T) {
		in := embedding(125)
		out, ok := truncateVoiceEmbedding(in)
		if ok {
			t.Fatal("did not expect truncation")
		}
		if !bytes.Equal(out, in) {
			t.Fatal("data changed")
		}
	})

	t.Run("100 frames untouched", func(t *testing.T) {
		in := embedding(100)
		if _, ok := truncateVoiceEmbedding(in); ok {
			t.Fatal("did not expect truncation")
		}
	})

	t.Run("empty untouched", func(t *testing.T) {
		if _, ok := truncateVoiceEmbedding(nil); ok {
			t.Fatal("did not expect truncation")
		}
	})

	t.Run("oversize but not a whole number of frames untouched", func(t *testing.T) {
		in := make([]byte, maxVoiceEmbeddingBytes+100) // not divisible by frame size
		out, ok := truncateVoiceEmbedding(in)
		if ok {
			t.Fatal("corrupt-size data must not be truncated")
		}
		if len(out) != len(in) {
			t.Fatal("data changed")
		}
	})
}
