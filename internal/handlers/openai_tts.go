package handlers

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	audioenc "github.com/shaunagostinho/gotranscribesrv/internal/audio"
	"github.com/shaunagostinho/gotranscribesrv/internal/logging"
	"github.com/shaunagostinho/gotranscribesrv/internal/middleware"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
)

// OpenAITTSHandler exposes OpenAI-compatible TTS endpoints.
//
// POST /v1/audio/speech — accepts OpenAI's schema
//
//	{ "model": "tts-1"|"tts-1-hd"|"kokoro"|..., "voice": "...", "input": "...",
//	  "response_format": "wav"|"pcm"|..., "speed": 0.25..4.0 }
//
// `model` selects the sidecar TTS backend when recognized:
//
//	tts-1, tts-1-hd, gpt-4o-mini-tts  → PocketTTS (low-latency, supports streaming & voice cloning)
//	kokoro, kokoro-82m, tts-1-hd-kokoro → Kokoro (multilingual, higher quality, batch only)
//
// When `model` is omitted or unrecognized, the server falls back to
// `TTS_DEFAULT_BACKEND` (default "kokoro") so voice-agent clients get the
// higher-quality path automatically. Set TTS_DEFAULT_BACKEND=pocket to
// preserve the prior default.
//
// `voice` is forwarded to the sidecar as-is (PocketTTS IDs like "alba"/"jane"
// or Kokoro IDs like "af_heart"/"zf_001").
//
// `response_format` controls the output encoding. "wav" (default) and "pcm"
// are served from the sidecar bytes directly (pcm strips the WAV header);
// "mp3"/"opus"/"flac"/"aac" are transcoded from the PCM via ffmpeg with
// fixed, server-chosen quality settings (see internal/audio) — clients
// cannot request arbitrary bitrates. When ffmpeg is not installed those
// formats return 501 with a clear message.
type OpenAITTSHandler struct {
	sidecar        *sidecar.Client
	lm             *logging.LogManager
	defaultBackend string // "pocket" or "kokoro" — TTS_DEFAULT_BACKEND
}

// NewOpenAITTSHandler creates a new OpenAITTSHandler. Pass defaultBackend=""
// to fall back to "kokoro"; pass "pocket" to preserve legacy defaults.
func NewOpenAITTSHandler(sc *sidecar.Client, lm *logging.LogManager, defaultBackend string) *OpenAITTSHandler {
	if defaultBackend == "" {
		defaultBackend = "kokoro"
	}
	return &OpenAITTSHandler{sidecar: sc, lm: lm, defaultBackend: defaultBackend}
}

// OpenAITTSBody mirrors OpenAI's /v1/audio/speech request schema.
type OpenAITTSBody struct {
	Model          string  `json:"model"`
	Voice          string  `json:"voice"`
	Input          string  `json:"input"`
	ResponseFormat string  `json:"response_format"`
	Speed          float64 `json:"speed"`
}

// Speech — POST /v1/audio/speech
func (h *OpenAITTSHandler) Speech(c *fiber.Ctx) error {
	var body OpenAITTSBody
	if err := c.BodyParser(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "INVALID_INPUT",
				"message": "Invalid JSON body",
				"status":  400,
			},
		})
	}

	if strings.TrimSpace(body.Input) == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "MISSING_INPUT",
				"message": "`input` field is required",
				"status":  400,
			},
		})
	}
	if len(body.Input) > 5000 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "INPUT_TOO_LONG",
				"message": "input must be 5,000 characters or less",
				"status":  400,
			},
		})
	}

	// Defaults — match OpenAI's defaults
	if body.Model == "" {
		body.Model = "tts-1"
	}
	if body.Voice == "" {
		body.Voice = "alloy"
	}
	if body.ResponseFormat == "" {
		body.ResponseFormat = "wav"
	}
	if body.Speed == 0 {
		body.Speed = 1.0
	}

	backend := openAIModelToBackend(body.Model, h.defaultBackend)

	// Format support — wav/pcm are served from the sidecar bytes directly;
	// mp3/opus/flac/aac are transcoded from the PCM via ffmpeg with fixed
	// quality settings (see internal/audio). When ffmpeg isn't installed,
	// compressed formats fall back to a clear 501 rather than silently
	// returning the wrong format.
	format := strings.ToLower(body.ResponseFormat)
	var (
		outCT       string
		stripWavHdr bool
		transcode   bool
	)
	switch format {
	case "wav":
		outCT = "audio/wav"
	case "pcm":
		outCT = "audio/L16; rate=24000; channels=1"
		stripWavHdr = true
	case "mp3", "opus", "flac", "aac":
		if !audioenc.Available() {
			return c.Status(fiber.StatusNotImplemented).JSON(fiber.Map{
				"error": fiber.Map{
					"code":    "UNSUPPORTED_FORMAT",
					"message": fmt.Sprintf("response_format=%q requires ffmpeg for transcoding, which is not installed on this server; use wav or pcm", format),
					"status":  501,
				},
			})
		}
		outCT = audioenc.ContentType(format)
		transcode = true
	default:
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "INVALID_FORMAT",
				"message": fmt.Sprintf("unknown response_format=%q", format),
				"status":  400,
			},
		})
	}

	// Map OpenAI alloy/ash/coral/etc. → PocketTTS voice IDs for the default
	// backend. For Kokoro, the voice ID is passed through unchanged so users
	// can use Kokoro-native names ("af_heart", "zf_001") directly.
	pocketVoice := body.Voice
	if backend == "pocket" {
		pocketVoice = openAIVoiceToPocket(body.Voice)
	}

	// OpenAI's alloy/ash/coral/sage/verse/etc. are abstract voice names; we
	// map them to PocketTTS presets. Kokoro-native IDs (af_heart, bf_emma,
	// zf_001, etc.) bypass the mapping when Kokoro is selected.
	req := sidecar.SynthesizeRequest{
		Text:   body.Input,
		Voice:  pocketVoice,
		Speed:  body.Speed,
		Format: format,
	}

	h.lm.SendLog(h.lm.BuildLog("OPENAI_TTS_REQUEST", "OpenAITTSRequest", slog.LevelInfo, map[string]interface{}{
		"endpoint":        "/v1/audio/speech",
		"ip":              c.IP(),
		"model":           body.Model,
		"backend":         backend,
		"voice":           pocketVoice,
		"response_format": format,
		"speed":           body.Speed,
		"input_length":    len(body.Input),
		"request_id":      middleware.RequestIDFromCtx(c),
	}))

	synthStart := time.Now()
	audio, _, backendUsed, err := h.sidecar.SynthesizeWithBackend(c.UserContext(), req, backend)
	synthDuration := time.Since(synthStart)
	if err != nil {
		h.lm.SendLog(h.lm.BuildLog("OPENAI_TTS_FAILED", "OpenAITTSFailed", slog.LevelError, map[string]interface{}{
			"endpoint":      "/v1/audio/speech",
			"ip":            c.IP(),
			"model":         body.Model,
			"backend":       backend,
			"voice":         pocketVoice,
			"input_length":  len(body.Input),
			"synth_time_ms": int(synthDuration.Milliseconds()),
			"request_id":    middleware.RequestIDFromCtx(c),
		}, err))
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "SIDECAR_ERROR",
				"message": "TTS service unavailable",
				"status":  502,
			},
		})
	}

	// Strip 44-byte WAV header for raw PCM output and for transcode input
	// (ffmpeg reads raw s16le, not the WAV wrapper).
	pcmBytes := len(audio)
	if (stripWavHdr || transcode) && len(audio) > audioenc.WAVHeaderLen {
		audio = audio[audioenc.WAVHeaderLen:]
	}
	pcmBytes = len(audio)

	// Duration is computed from the PCM length — after transcoding the
	// byte count no longer maps linearly to time.
	outputDurationMs := 0
	if pcmBytes > 0 {
		// 24 kHz, 16-bit, mono = 48000 bytes/sec
		outputDurationMs = pcmBytes * 1000 / audioenc.BytesPerSec
	}

	transcodeMs := 0
	if transcode {
		tcStart := time.Now()
		encoded, err := audioenc.TranscodePCM(audio, format)
		transcodeMs = int(time.Since(tcStart).Milliseconds())
		if err != nil {
			h.lm.SendLog(h.lm.BuildLog("OPENAI_TTS_TRANSCODE_FAILED", "OpenAITTSTranscodeFailed", slog.LevelWarn, map[string]interface{}{
				"endpoint":        "/v1/audio/speech",
				"ip":              c.IP(),
				"model":           body.Model,
				"backend":         backendUsed,
				"response_format": format,
				"input_length":    len(body.Input),
				"request_id":      middleware.RequestIDFromCtx(c),
			}, err))
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": fiber.Map{
					"code":    "TRANSCODE_FAILED",
					"message": fmt.Sprintf("failed to encode response_format=%q", format),
					"status":  500,
				},
			})
		}
		audio = encoded
	}

	h.lm.SendLog(h.lm.BuildLog("OPENAI_TTS_COMPLETED", "OpenAITTSCompleted", slog.LevelInfo, map[string]interface{}{
		"endpoint":           "/v1/audio/speech",
		"ip":                 c.IP(),
		"model":              body.Model,
		"backend":            backendUsed,
		"voice":              pocketVoice,
		"response_format":    format,
		"input_length":       len(body.Input),
		"output_bytes":       len(audio),
		"output_duration_ms": outputDurationMs,
		"synth_time_ms":      int(synthDuration.Milliseconds()),
		"transcode_time_ms":  transcodeMs,
		"request_id":         middleware.RequestIDFromCtx(c),
	}))

	c.Set("Content-Type", outCT)
	c.Set("X-Audio-Sample-Rate", "24000")
	c.Set("X-TTS-Backend", backendUsed)
	c.Set("X-TTS-Model", body.Model)
	return c.Send(audio)
}

// openAIModelToBackend maps OpenAI's model ID space to a sidecar backend.
// Recognized IDs map to a specific backend; unrecognized / empty IDs
// fall back to the server-wide default (TTS_DEFAULT_BACKEND).
func openAIModelToBackend(model string, defaultBackend string) string {
	if defaultBackend == "" {
		defaultBackend = "kokoro"
	}
	m := strings.ToLower(strings.TrimSpace(model))
	switch m {
	case "tts-1", "tts-1-hd", "gpt-4o-mini-tts", "tts-1-1106", "tts-1-hd-1106":
		return "pocket"
	case "kokoro", "kokoro-82m", "tts-1-hd-kokoro":
		return "kokoro"
	}
	// Heuristic: any model name containing "kokoro" routes to Kokoro.
	if strings.Contains(m, "kokoro") {
		return "kokoro"
	}
	// Unknown / unrecognized model — fall back to server default.
	return defaultBackend
}

// openAIVoiceToPocket maps OpenAI's abstract voice names to PocketTTS voices.
// Kokoro voice IDs (af_heart, bf_emma, zf_001, jf_alpha, etc.) are not in
// this table and pass through unchanged when the Kokoro backend is selected.
func openAIVoiceToPocket(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "alloy":
		return "jane"
	case "ash":
		return "charles"
	case "ballad":
		return "mary"
	case "coral":
		return "eve"
	case "echo":
		return "alba"
	case "sage":
		return "george"
	case "shimmer":
		return "anna"
	case "verse":
		return "michael"
	case "onyx":
		return "paul"
	case "nova":
		return "vera"
	case "fable":
		return "jean"
	}
	// Treat as already a PocketTTS or Kokoro ID and forward unchanged —
	// sidecar rejects unknown voices, surfacing the misconfig clearly.
	return v
}
