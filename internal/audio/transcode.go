// Package audio provides audio format conversion for the TTS endpoints.
// The TTS sidecar emits 24 kHz mono 16-bit PCM (in a WAV wrapper); clients
// of the OpenAI-compatible API may request compressed formats (mp3, opus,
// flac, aac), which are produced here by piping the PCM through ffmpeg.
//
// Quality is deliberately NOT client-controlled: each format has a fixed
// encoder setting chosen as "generous for 24 kHz mono speech". This keeps
// output quality and encode cost bounded regardless of what the client
// asks for.
package audio

import (
	"bytes"
	"fmt"
	"os/exec"
)

// Source PCM properties emitted by the TTS sidecar (PocketTTS / Kokoro).
const (
	SampleRate   = 24000
	Channels     = 1
	BytesPerSec  = 48000 // 24 kHz * 16-bit * mono
	WAVHeaderLen = 44
)

// formatSpec describes one transcodable output format: ffmpeg encoder args
// (fixed quality ladder) and the HTTP content type.
type formatSpec struct {
	args        []string
	contentType string
}

// transcodable is the set of compressed formats we can produce from PCM.
// wav/pcm are NOT here — they are served directly from the sidecar bytes
// with no encode pass.
var transcodable = map[string]formatSpec{
	// ~165 kbps VBR — transparent for 24 kHz mono speech.
	"mp3": {[]string{"-c:a", "libmp3lame", "-q:a", "4", "-f", "mp3"}, "audio/mpeg"},
	// Opus in an Ogg container (what OpenAI returns for response_format=opus).
	"opus": {[]string{"-c:a", "libopus", "-b:a", "96k", "-f", "ogg"}, "audio/ogg"},
	// AAC in ADTS framing.
	"aac": {[]string{"-c:a", "aac", "-b:a", "96k", "-f", "adts"}, "audio/aac"},
	// Lossless.
	"flac": {[]string{"-c:a", "flac", "-compression_level", "5", "-f", "flac"}, "audio/flac"},
}

// ffmpegPath is resolved lazily; kept in a var so tests can stub it.
var ffmpegPath = "ffmpeg"

// Transcodable reports whether format is a compressed format we can
// produce via ffmpeg.
func Transcodable(format string) bool {
	_, ok := transcodable[format]
	return ok
}

// ContentType returns the HTTP content type for a transcoded format.
func ContentType(format string) string {
	return transcodable[format].contentType
}

// Available reports whether an ffmpeg binary is on PATH. When false,
// handlers should fall back to rejecting compressed formats with 501.
func Available() bool {
	_, err := exec.LookPath(ffmpegPath)
	return err == nil
}

// TranscodePCM converts raw 24 kHz mono s16le PCM to the target format.
// The format must be one of Transcodable(); callers strip the WAV header
// before calling. Returns the encoded bytes.
func TranscodePCM(pcm []byte, format string) ([]byte, error) {
	spec, ok := transcodable[format]
	if !ok {
		return nil, fmt.Errorf("unsupported transcode format %q", format)
	}

	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "s16le", "-ar", "24000", "-ac", "1", "-i", "pipe:0",
	}
	args = append(args, spec.args...)
	args = append(args, "pipe:1")

	cmd := exec.Command(ffmpegPath, args...)
	cmd.Stdin = bytes.NewReader(pcm)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg %s encode: %w: %s", format, err, stderr.String())
	}
	if stdout.Len() == 0 {
		return nil, fmt.Errorf("ffmpeg %s encode produced no output", format)
	}
	return stdout.Bytes(), nil
}
