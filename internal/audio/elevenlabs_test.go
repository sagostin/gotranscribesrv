package audio

import (
	"bytes"
	"testing"
)

func TestParseElevenLabsFormat(t *testing.T) {
	valid := []struct {
		in      string
		codec   string
		rate    int
		bitrate int
	}{
		{"mp3_44100_128", "mp3", 44100, 128},
		{"mp3_22050_32", "mp3", 22050, 32},
		{"opus_48000_96", "opus", 48000, 96},
		{"pcm_24000", "pcm", 24000, 0},
		{"pcm_16000", "pcm", 16000, 0},
		{"wav_24000", "wav", 24000, 0},
		{"ulaw_8000", "ulaw", 8000, 0},
		{"alaw_8000", "alaw", 8000, 0},
		{"MP3_44100_128", "mp3", 44100, 128}, // case-insensitive
	}
	for _, tc := range valid {
		f, err := ParseElevenLabsFormat(tc.in)
		if err != nil {
			t.Errorf("ParseElevenLabsFormat(%q): %v", tc.in, err)
			continue
		}
		if f.Codec != tc.codec || f.SampleRate != tc.rate || f.Bitrate != tc.bitrate {
			t.Errorf("ParseElevenLabsFormat(%q) = {%s %d %d}, want {%s %d %d}",
				tc.in, f.Codec, f.SampleRate, f.Bitrate, tc.codec, tc.rate, tc.bitrate)
		}
	}

	invalid := []string{"", "mp3", "mp3_44100", "pcm_abc", "midi_24000", "mp3_44100_128_extra", "pcm_-1"}
	for _, in := range invalid {
		if _, err := ParseElevenLabsFormat(in); err == nil {
			t.Errorf("ParseElevenLabsFormat(%q) should fail", in)
		}
	}
}

func TestELFormatFastPaths(t *testing.T) {
	// pcm_24000 / wav_24000 match the sidecar's native output — no ffmpeg.
	for _, raw := range []string{"pcm_24000", "wav_24000"} {
		f, err := ParseElevenLabsFormat(raw)
		if err != nil {
			t.Fatal(err)
		}
		if f.NeedsFFmpeg() {
			t.Errorf("%s should be a no-ffmpeg fast path", raw)
		}
	}
	// Everything else needs an encode/resample pass.
	for _, raw := range []string{"mp3_44100_128", "pcm_16000", "wav_44100", "ulaw_8000", "opus_48000_64"} {
		f, err := ParseElevenLabsFormat(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !f.NeedsFFmpeg() {
			t.Errorf("%s should require ffmpeg", raw)
		}
	}
}

func TestELFormatContentTypes(t *testing.T) {
	want := map[string]string{
		"mp3_44100_128": "audio/mpeg",
		"opus_48000_64": "audio/ogg",
		"wav_24000":     "audio/wav",
		"ulaw_8000":     "audio/basic",
		"pcm_24000":     "audio/L16; rate=24000; channels=1",
	}
	for raw, ct := range want {
		f, err := ParseElevenLabsFormat(raw)
		if err != nil {
			t.Fatal(err)
		}
		if f.ContentType() != ct {
			t.Errorf("%s ContentType() = %q, want %q", raw, f.ContentType(), ct)
		}
	}
}

func TestTranscodePCMELFormats(t *testing.T) {
	requireFFmpeg(t)
	pcm := testPCM()

	mustParse := func(raw string) ELFormat {
		t.Helper()
		f, err := ParseElevenLabsFormat(raw)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}

	// mp3: ID3 header or frame sync.
	out, err := TranscodePCMEL(pcm, mustParse("mp3_44100_128"))
	if err != nil {
		t.Fatalf("mp3_44100_128: %v", err)
	}
	if !bytes.HasPrefix(out, []byte("ID3")) && !(out[0] == 0xFF && out[1]&0xE0 == 0xE0) {
		t.Errorf("mp3 output missing ID3/frame sync: % x", out[:min(4, len(out))])
	}

	// opus → Ogg container magic.
	out, err = TranscodePCMEL(pcm, mustParse("opus_48000_64"))
	if err != nil {
		t.Fatalf("opus_48000_64: %v", err)
	}
	if !bytes.HasPrefix(out, []byte("OggS")) {
		t.Errorf("opus output missing OggS magic: % x", out[:min(8, len(out))])
	}

	// wav_16000 → RIFF header.
	out, err = TranscodePCMEL(pcm, mustParse("wav_16000"))
	if err != nil {
		t.Fatalf("wav_16000: %v", err)
	}
	if !bytes.HasPrefix(out, []byte("RIFF")) {
		t.Errorf("wav output missing RIFF magic: % x", out[:min(8, len(out))])
	}

	// pcm_16000 → raw s16le: 100ms at 16 kHz * 2 bytes = 3200 bytes.
	out, err = TranscodePCMEL(pcm, mustParse("pcm_16000"))
	if err != nil {
		t.Fatalf("pcm_16000: %v", err)
	}
	if want := 16000 / 10 * 2; len(out) != want {
		t.Errorf("pcm_16000 length = %d, want %d", len(out), want)
	}

	// ulaw_8000 → 1 byte/sample: 100ms at 8 kHz = 800 bytes.
	out, err = TranscodePCMEL(pcm, mustParse("ulaw_8000"))
	if err != nil {
		t.Fatalf("ulaw_8000: %v", err)
	}
	if want := 8000 / 10; len(out) != want {
		t.Errorf("ulaw_8000 length = %d, want %d", len(out), want)
	}
}
