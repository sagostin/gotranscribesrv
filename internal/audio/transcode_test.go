package audio

import (
	"bytes"
	"testing"
)

// testPCM synthesizes 100ms of 24 kHz mono s16le silence (a valid, if
// boring, PCM buffer — ffmpeg happily encodes silence).
func testPCM() []byte {
	return make([]byte, BytesPerSec/10)
}

func TestTranscodableFormats(t *testing.T) {
	for _, f := range []string{"mp3", "opus", "flac", "aac"} {
		if !Transcodable(f) {
			t.Errorf("Transcodable(%q) = false, want true", f)
		}
		if ContentType(f) == "" {
			t.Errorf("ContentType(%q) empty", f)
		}
	}
	for _, f := range []string{"wav", "pcm", "ogg", "midi", ""} {
		if Transcodable(f) {
			t.Errorf("Transcodable(%q) = true, want false", f)
		}
	}
}

func TestTranscodePCMRejectsUnknownFormat(t *testing.T) {
	if _, err := TranscodePCM(testPCM(), "wav"); err == nil {
		t.Error("TranscodePCM(wav) should fail — wav is served directly, not transcoded")
	}
}

// The encode tests below need a real ffmpeg binary; skip when absent so
// CI without ffmpeg still passes (the handler falls back to 501 there).
func requireFFmpeg(t *testing.T) {
	t.Helper()
	if !Available() {
		t.Skip("ffmpeg not installed")
	}
}

func TestTranscodePCMFormats(t *testing.T) {
	requireFFmpeg(t)
	pcm := testPCM()

	magic := map[string][]byte{
		"mp3":  nil, // checked separately — may or may not have an ID3 header
		"opus": []byte("OggS"),
		"flac": []byte("fLaC"),
		"aac":  {0xFF, 0xF1}, // ADTS sync (also accepts 0xFFF9 below)
	}
	for format, want := range magic {
		out, err := TranscodePCM(pcm, format)
		if err != nil {
			t.Errorf("TranscodePCM(%s): %v", format, err)
			continue
		}
		if len(out) == 0 {
			t.Errorf("TranscodePCM(%s): empty output", format)
			continue
		}
		switch format {
		case "mp3":
			isID3 := bytes.HasPrefix(out, []byte("ID3"))
			isFrameSync := out[0] == 0xFF && out[1]&0xE0 == 0xE0
			if !isID3 && !isFrameSync {
				t.Errorf("mp3 output has neither ID3 header nor frame sync: % x", out[:min(4, len(out))])
			}
		case "aac":
			if out[0] != 0xFF || (out[1] != 0xF1 && out[1] != 0xF9) {
				t.Errorf("aac output missing ADTS sync: % x", out[:min(4, len(out))])
			}
		default:
			if !bytes.HasPrefix(out, want) {
				t.Errorf("%s output missing magic %q: % x", format, want, out[:min(8, len(out))])
			}
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
