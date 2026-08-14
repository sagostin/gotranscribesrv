package audio

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ElevenLabs output_format support.
//
// ElevenLabs encodes the target as codec_samplerate[_bitrate], e.g.
// "mp3_44100_128", "pcm_24000", "ulaw_8000". The sidecar always emits
// 24 kHz mono s16le (WAV-wrapped for batch), so wav_24000 and pcm_24000
// are served from the sidecar bytes directly (fast path, no ffmpeg) and
// everything else is resampled/encoded through ffmpeg with the client-
// requested rate/bitrate (unlike the OpenAI path's fixed quality ladder —
// ElevenLabs clients, Twilio in particular, depend on exact rates).

// ELFormat is a parsed ElevenLabs output_format value.
type ELFormat struct {
	Codec      string // mp3, pcm, wav, opus, ulaw, alaw
	SampleRate int    // e.g. 44100
	Bitrate    int    // kbps; 0 for pcm/wav/ulaw/alaw
	Raw        string // original output_format string (for logs)
}

// ElevenLabsDefaultFormat is used when output_format is not supplied.
const ElevenLabsDefaultFormat = "mp3_44100_128"

// ParseElevenLabsFormat parses an output_format query value.
func ParseElevenLabsFormat(s string) (ELFormat, error) {
	f := ELFormat{Raw: s}
	parts := strings.Split(strings.ToLower(strings.TrimSpace(s)), "_")
	if len(parts) < 2 || len(parts) > 3 {
		return f, fmt.Errorf("invalid output_format %q (want codec_samplerate[_bitrate])", s)
	}

	f.Codec = parts[0]
	switch f.Codec {
	case "mp3", "pcm", "wav", "opus", "ulaw", "alaw":
	default:
		return f, fmt.Errorf("unknown output_format codec %q (valid: mp3, pcm, wav, opus, ulaw, alaw)", f.Codec)
	}

	rate, err := strconv.Atoi(parts[1])
	if err != nil || rate <= 0 {
		return f, fmt.Errorf("invalid sample rate in output_format %q", s)
	}
	f.SampleRate = rate

	if len(parts) == 3 {
		br, err := strconv.Atoi(parts[2])
		if err != nil || br <= 0 {
			return f, fmt.Errorf("invalid bitrate in output_format %q", s)
		}
		f.Bitrate = br
	}
	if (f.Codec == "mp3" || f.Codec == "opus") && f.Bitrate == 0 {
		return f, fmt.Errorf("output_format %q missing bitrate", s)
	}
	return f, nil
}

// NeedsFFmpeg reports whether producing this format requires an ffmpeg
// encode/resample pass. wav_24000 and pcm_24000 match the sidecar's native
// output and are served directly.
func (f ELFormat) NeedsFFmpeg() bool {
	if f.SampleRate == SampleRate && (f.Codec == "wav" || f.Codec == "pcm") {
		return false
	}
	return true
}

// ContentType returns the HTTP Content-Type for the encoded audio.
func (f ELFormat) ContentType() string {
	switch f.Codec {
	case "mp3":
		return "audio/mpeg"
	case "opus":
		return "audio/ogg"
	case "wav":
		return "audio/wav"
	case "ulaw", "alaw":
		return "audio/basic" // Twilio convention
	default: // pcm
		return fmt.Sprintf("audio/L16; rate=%d; channels=1", f.SampleRate)
	}
}

// ffmpegArgs builds the output-side ffmpeg arguments for the format.
func (f ELFormat) ffmpegArgs() []string {
	rate := strconv.Itoa(f.SampleRate)
	switch f.Codec {
	case "mp3":
		return []string{"-c:a", "libmp3lame", "-b:a", strconv.Itoa(f.Bitrate) + "k", "-ar", rate, "-ac", "1", "-f", "mp3"}
	case "opus":
		return []string{"-c:a", "libopus", "-b:a", strconv.Itoa(f.Bitrate) + "k", "-ar", rate, "-ac", "1", "-f", "ogg"}
	case "wav":
		return []string{"-ar", rate, "-ac", "1", "-f", "wav"}
	case "ulaw":
		return []string{"-c:a", "pcm_mulaw", "-ar", rate, "-ac", "1", "-f", "mulaw"}
	case "alaw":
		return []string{"-c:a", "pcm_alaw", "-ar", rate, "-ac", "1", "-f", "alaw"}
	default: // pcm — raw s16le
		return []string{"-ar", rate, "-ac", "1", "-f", "s16le"}
	}
}

// TranscodePCMEL converts raw 24 kHz mono s16le PCM to the requested
// ElevenLabs format (resampling included). Callers strip the WAV header
// before calling.
func TranscodePCMEL(pcm []byte, f ELFormat) ([]byte, error) {
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "s16le", "-ar", strconv.Itoa(SampleRate), "-ac", "1", "-i", "pipe:0",
	}
	args = append(args, f.ffmpegArgs()...)
	args = append(args, "pipe:1")

	cmd := exec.Command(ffmpegPath, args...)
	cmd.Stdin = bytes.NewReader(pcm)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg %s encode: %w: %s", f.Raw, err, stderr.String())
	}
	if stdout.Len() == 0 {
		return nil, fmt.Errorf("ffmpeg %s encode produced no output", f.Raw)
	}
	return stdout.Bytes(), nil
}
