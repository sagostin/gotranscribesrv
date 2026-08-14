package handlers

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	audioenc "github.com/shaunagostinho/gotranscribesrv/internal/audio"
	"github.com/shaunagostinho/gotranscribesrv/internal/logging"
	"github.com/shaunagostinho/gotranscribesrv/internal/metrics"
	"github.com/shaunagostinho/gotranscribesrv/internal/middleware"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
)

// ElevenLabsHandler exposes an ElevenLabs-compatible TTS API surface so
// unmodified ElevenLabs SDKs/clients work by pointing base_url at this
// gateway and using an API key as xi-api-key.
//
// Routes:
//
//	POST /v1/text-to-speech/:voice_id          — batch synthesis
//	POST /v1/text-to-speech/:voice_id/stream   — streaming synthesis
//	GET  /v1/voices                            — voice list (legacy shape)
//	GET  /v2/voices                            — voice list (search shape)
//	GET  /v1/voices/:voice_id                  — single voice
//	GET  /v1/models                            — ElevenLabs model list
//	                                             (shared path with OpenAI;
//	                                             routed here when xi-api-key
//	                                             is present — see main.go)
//
// Voice IDs accepted in the path:
//   - PocketTTS voice names ("jane", "alba", ...) and Kokoro IDs
//     ("af_heart", "zf_001", ...) — forwarded to the sidecar, whose
//     VoiceResolver validates/reroutes per backend.
//   - UUIDs of cloned voices (from /api/v1/voices) — the stored embedding
//     is loaded and sent as voice_data (PocketTTS backend).
//   - "default" — the server default backend's default voice.
//
// model_id mapping: "kokoro"/"kokoro_82m" → kokoro backend, "pocket"/
// "pocket_tts"/"pocket-tts-1" → pocket, anything else (incl. eleven_* IDs)
// → the server default backend.
//
// output_format: ElevenLabs codec_samplerate[_bitrate] strings. wav_24000
// and pcm_24000 are served without an encode pass; other formats require
// ffmpeg (501 when absent). voice_settings other than speed are accepted
// and ignored (PocketTTS/Kokoro expose no stability/similarity controls).
type ElevenLabsHandler struct {
	sidecar        *sidecar.Client
	voiceHandler   *VoiceHandler
	lm             *logging.LogManager
	defaultBackend string
}

// NewElevenLabsHandler creates the handler. defaultBackend is
// TTS_DEFAULT_BACKEND ("pocket"/"kokoro") for unrecognized model IDs.
func NewElevenLabsHandler(sc *sidecar.Client, voiceHandler *VoiceHandler, lm *logging.LogManager, defaultBackend string) *ElevenLabsHandler {
	if defaultBackend == "" {
		defaultBackend = "kokoro"
	}
	return &ElevenLabsHandler{sidecar: sc, voiceHandler: voiceHandler, lm: lm, defaultBackend: defaultBackend}
}

// ── Request/response schemas ────────────────────────────────────────────

// ElevenLabsTTSBody mirrors the ElevenLabs text-to-speech request body.
// Fields PocketTTS/Kokoro cannot honor (stability, similarity_boost, style,
// seeds, request stitching, pronunciation dictionaries) are parsed so the
// JSON decodes cleanly but otherwise ignored.
type ElevenLabsTTSBody struct {
	Text          string `json:"text"`
	ModelID       string `json:"model_id"`
	LanguageCode  string `json:"language_code"`
	VoiceSettings *struct {
		Stability       float64 `json:"stability"`
		SimilarityBoost float64 `json:"similarity_boost"`
		Style           float64 `json:"style"`
		UseSpeakerBoost *bool   `json:"use_speaker_boost"`
		Speed           float64 `json:"speed"`
	} `json:"voice_settings"`
}

// ElevenLabsVoice is a voice entry in ElevenLabs list/get responses.
// Only the fields clients actually read are populated.
type ElevenLabsVoice struct {
	VoiceID     string                   `json:"voice_id"`
	Name        string                   `json:"name,omitempty"`
	Category    string                   `json:"category,omitempty"` // "premade" | "cloned"
	Description string                   `json:"description,omitempty"`
	Labels      map[string]string        `json:"labels,omitempty"`
	Settings    *ElevenLabsVoiceSettings `json:"settings,omitempty"`
}

// ElevenLabsVoiceSettings mirrors ElevenLabs' voice settings object.
type ElevenLabsVoiceSettings struct {
	Stability       float64 `json:"stability"`
	SimilarityBoost float64 `json:"similarity_boost"`
	Style           float64 `json:"style"`
	UseSpeakerBoost bool    `json:"use_speaker_boost"`
	Speed           float64 `json:"speed"`
}

var defaultELVoiceSettings = &ElevenLabsVoiceSettings{
	Stability: 0.5, SimilarityBoost: 0.75, Style: 0, UseSpeakerBoost: true, Speed: 1,
}

// elError renders an ElevenLabs-shaped error: {"detail":{status,message}}.
func elError(c *fiber.Ctx, httpStatus int, status, message string) error {
	return c.Status(httpStatus).JSON(fiber.Map{
		"detail": fiber.Map{"status": status, "message": message},
	})
}

// ── Synthesis ───────────────────────────────────────────────────────────

// Convert handles POST /v1/text-to-speech/:voice_id — batch synthesis.
func (h *ElevenLabsHandler) Convert(c *fiber.Ctx) error {
	return h.synthesize(c, false)
}

// ConvertStream handles POST /v1/text-to-speech/:voice_id/stream.
// pcm_24000 streams chunked straight from the sidecar (PocketTTS); every
// other format falls back to batch synthesize + transcode (still a valid
// HTTP body — SDKs consume it identically, just without early chunks).
func (h *ElevenLabsHandler) ConvertStream(c *fiber.Ctx) error {
	return h.synthesize(c, true)
}

func (h *ElevenLabsHandler) synthesize(c *fiber.Ctx, streaming bool) error {
	voiceID := c.Params("voice_id")
	endpoint := "/v1/text-to-speech/" + voiceID
	if streaming {
		endpoint += "/stream"
	}

	var body ElevenLabsTTSBody
	if err := c.BodyParser(&body); err != nil {
		h.logELValidation(endpoint, voiceID, "invalid_json", c)
		return elError(c, fiber.StatusBadRequest, "invalid_request", "Invalid JSON body")
	}
	if strings.TrimSpace(body.Text) == "" {
		h.logELValidation(endpoint, voiceID, "missing_text", c)
		return elError(c, fiber.StatusBadRequest, "invalid_request", "`text` field is required")
	}
	if len(body.Text) > 5000 {
		h.logELValidation(endpoint, voiceID, "text_too_long", c)
		return elError(c, fiber.StatusUnprocessableEntity, "invalid_request", "text must be 5,000 characters or less")
	}

	format, err := audioenc.ParseElevenLabsFormat(c.Query("output_format", audioenc.ElevenLabsDefaultFormat))
	if err != nil {
		h.logELValidation(endpoint, voiceID, "invalid_output_format", c)
		return elError(c, fiber.StatusUnprocessableEntity, "invalid_output_format", err.Error())
	}
	if format.NeedsFFmpeg() && !audioenc.Available() {
		h.logELValidation(endpoint, voiceID, "format_needs_ffmpeg", c)
		return elError(c, fiber.StatusNotImplemented, "unsupported_format",
			fmt.Sprintf("output_format=%q requires ffmpeg for transcoding, which is not installed on this server; use pcm_24000 or wav_24000", format.Raw))
	}

	speed := 1.0
	if body.VoiceSettings != nil && body.VoiceSettings.Speed > 0 {
		speed = body.VoiceSettings.Speed
	}

	backend := elevenLabsModelToBackend(body.ModelID)
	req, backend, resolveErr := h.buildSynthRequest(c, voiceID, body.Text, speed, backend)
	if resolveErr != nil {
		return resolveErr // response already written
	}

	h.lm.SendLog(h.lm.BuildLog("ELEVENLABS_TTS_REQUEST", "ElevenLabsTTSRequest", slog.LevelInfo, map[string]interface{}{
		"endpoint":      endpoint,
		"ip":            c.IP(),
		"voice_id":      voiceID,
		"model_id":      body.ModelID,
		"backend":       backend,
		"output_format": format.Raw,
		"speed":         speed,
		"text_length":   len(body.Text),
		"streaming":     streaming,
		"request_id":    middleware.RequestIDFromCtx(c),
	}))

	// True chunked streaming is only available for the sidecar's native
	// PCM (PocketTTS /synthesize/stream, L16 24 kHz).
	if streaming && !format.NeedsFFmpeg() && format.Codec == "pcm" {
		return h.streamPCM(c, endpoint, req, voiceID, body.ModelID, backend, format)
	}

	synthStart := time.Now()
	audio, _, backendUsed, err := h.sidecar.SynthesizeWithBackend(c.UserContext(), req, backend)
	synthDuration := time.Since(synthStart)
	if err != nil {
		return h.handleSynthError(c, endpoint, voiceID, body.ModelID, backend, len(body.Text), synthDuration, err, streaming)
	}

	// Sidecar emits WAV-wrapped 24 kHz PCM.
	pcm := audio
	if len(audio) > audioenc.WAVHeaderLen {
		pcm = audio[audioenc.WAVHeaderLen:]
	}
	outputDurationMs := 0
	if len(pcm) > 0 {
		outputDurationMs = len(pcm) * 1000 / audioenc.BytesPerSec
	}

	out := audio
	outCT := format.ContentType()
	transcodeMs := 0
	switch {
	case format.Codec == "wav" && !format.NeedsFFmpeg():
		// wav_24000 — sidecar bytes as-is
		outCT = "audio/wav"
	case format.Codec == "pcm" && !format.NeedsFFmpeg():
		// pcm_24000 — strip the WAV header
		out = pcm
	default:
		tcStart := time.Now()
		encoded, err := audioenc.TranscodePCMEL(pcm, format)
		transcodeMs = int(time.Since(tcStart).Milliseconds())
		if err != nil {
			h.lm.SendLog(h.lm.BuildLog("ELEVENLABS_TTS_FAILED", "ElevenLabsTTSFailed", slog.LevelError, map[string]interface{}{
				"endpoint":      endpoint,
				"ip":            c.IP(),
				"voice_id":      voiceID,
				"model_id":      body.ModelID,
				"backend":       backendUsed,
				"output_format": format.Raw,
				"stage":         "transcode",
				"request_id":    middleware.RequestIDFromCtx(c),
			}, err))
			return elError(c, fiber.StatusInternalServerError, "transcode_failed",
				fmt.Sprintf("failed to encode output_format=%q", format.Raw))
		}
		out = encoded
	}

	metrics.RecordTTSUsage(voiceID, int(synthDuration.Milliseconds()))

	h.lm.SendLog(h.lm.BuildLog("ELEVENLABS_TTS_COMPLETED", "ElevenLabsTTSCompleted", slog.LevelInfo, map[string]interface{}{
		"endpoint":           endpoint,
		"ip":                 c.IP(),
		"voice_id":           voiceID,
		"model_id":           body.ModelID,
		"backend":            backendUsed,
		"output_format":      format.Raw,
		"text_length":        len(body.Text),
		"output_bytes":       len(out),
		"output_duration_ms": outputDurationMs,
		"synth_time_ms":      int(synthDuration.Milliseconds()),
		"transcode_time_ms":  transcodeMs,
		"streaming":          streaming,
		"request_id":         middleware.RequestIDFromCtx(c),
	}))

	c.Locals("audio_duration_ms", outputDurationMs)
	c.Locals("usage_meta", map[string]interface{}{
		"text_length":        len(body.Text),
		"voice":              voiceID,
		"backend":            backendUsed,
		"output_format":      format.Raw,
		"output_bytes":       len(out),
		"output_duration_ms": outputDurationMs,
		"synth_time_ms":      int(synthDuration.Milliseconds()),
	})

	c.Set("Content-Type", outCT)
	return c.Send(out)
}

// streamPCM proxies the sidecar's chunked L16 24 kHz stream (PocketTTS)
// straight through to the client.
func (h *ElevenLabsHandler) streamPCM(c *fiber.Ctx, endpoint string, req sidecar.SynthesizeRequest, voiceID, modelID, backend string, format audioenc.ELFormat) error {
	synthStart := time.Now()
	body, err := h.sidecar.SynthesizeStream(c.UserContext(), req.Text, req.Voice, middleware.RequestIDFromCtx(c))
	if err != nil {
		return h.handleSynthError(c, endpoint, voiceID, modelID, backend, len(req.Text), time.Since(synthStart), err, true)
	}

	requestID := middleware.RequestIDFromCtx(c)
	c.Set("Content-Type", format.ContentType())
	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		defer body.Close()
		start := time.Now()
		n, copyErr := io.Copy(w, body)
		if copyErr != nil {
			slog.Warn("elevenlabs stream copy failed", "error", copyErr, "request_id", requestID)
			return
		}
		_ = w.Flush()
		outputDurationMs := int(n) * 1000 / audioenc.BytesPerSec
		metrics.RecordTTSUsage(voiceID, int(time.Since(start).Milliseconds()))
		h.lm.SendLog(h.lm.BuildLog("ELEVENLABS_TTS_COMPLETED", "ElevenLabsTTSCompleted", slog.LevelInfo, map[string]interface{}{
			"endpoint":           endpoint,
			"voice_id":           voiceID,
			"model_id":           modelID,
			"backend":            "pocket",
			"output_format":      format.Raw,
			"text_length":        len(req.Text),
			"output_bytes":       n,
			"output_duration_ms": outputDurationMs,
			"synth_time_ms":      int(time.Since(start).Milliseconds()),
			"streaming":          true,
			"request_id":         requestID,
		}))
	})
	return nil
}

// handleSynthError maps a sidecar synthesis failure onto an
// ElevenLabs-shaped response: 4xx rejections (unknown voice, cloning on
// kokoro, ...) are forwarded; everything else is a 502 outage.
func (h *ElevenLabsHandler) handleSynthError(c *fiber.Ctx, endpoint, voiceID, modelID, backend string, textLen int, synthDuration time.Duration, err error, streaming bool) error {
	var scErr *sidecar.SidecarError
	sidecarStatus := 0
	if errors.As(err, &scErr) {
		sidecarStatus = scErr.StatusCode
	}
	h.lm.SendLog(h.lm.BuildLog("ELEVENLABS_TTS_FAILED", "ElevenLabsTTSFailed", slog.LevelError, map[string]interface{}{
		"endpoint":       endpoint,
		"ip":             c.IP(),
		"voice_id":       voiceID,
		"model_id":       modelID,
		"backend":        backend,
		"text_length":    textLen,
		"synth_time_ms":  int(synthDuration.Milliseconds()),
		"sidecar_status": sidecarStatus,
		"streaming":      streaming,
		"request_id":     middleware.RequestIDFromCtx(c),
	}, err))

	if scErr != nil && scErr.StatusCode >= 400 && scErr.StatusCode < 500 {
		status := "invalid_voice"
		if scErr.StatusCode == fiber.StatusNotImplemented {
			status = "not_supported"
		}
		return elError(c, scErr.StatusCode, status, scErr.Reason)
	}
	return elError(c, fiber.StatusBadGateway, "tts_unavailable", "TTS service unavailable")
}

// buildSynthRequest resolves the ElevenLabs voice_id path param into a
// sidecar request. Cloned-voice UUIDs load the stored embedding; named
// voices pass through to the sidecar VoiceResolver.
func (h *ElevenLabsHandler) buildSynthRequest(c *fiber.Ctx, voiceID, text string, speed float64, backend string) (sidecar.SynthesizeRequest, string, error) {
	req := sidecar.SynthesizeRequest{
		Text:   text,
		Voice:  voiceID,
		Speed:  speed,
		Format: "wav",
	}
	if req.Voice == "" {
		req.Voice = "default"
	}

	// Cloned voice? voice_id is the UUID from /api/v1/voices.
	if voiceUUID, err := uuid.Parse(voiceID); err == nil {
		userID, err := middleware.ParseUserID(c)
		if err != nil {
			return req, backend, elError(c, fiber.StatusUnauthorized, "unauthorized", "Authentication required")
		}
		voiceData, err := h.voiceHandler.LoadVoiceData(voiceUUID, userID)
		if err != nil {
			h.lm.SendLog(h.lm.BuildLog("TTS_VOICE_LOAD_FAILED", "TTSVoiceLoadFailed", slog.LevelWarn, map[string]interface{}{
				"endpoint":   "/v1/text-to-speech",
				"ip":         c.IP(),
				"voice_id":   voiceID,
				"request_id": middleware.RequestIDFromCtx(c),
			}, err))
			return req, backend, elError(c, fiber.StatusNotFound, "voice_not_found", "Voice not found")
		}
		req.VoiceData = voiceData
		req.Voice = ""
		// Cloned voices are PocketTTS embeddings — cloning isn't supported
		// by Kokoro; the sidecar 422s if forced otherwise.
		if backend == "" {
			backend = "pocket"
		}
	}
	return req, backend, nil
}

// elevenLabsModelToBackend maps an ElevenLabs model_id to a sidecar TTS
// backend. Unknown/eleven_* IDs return "" so the sidecar default applies —
// clients hardcoding "eleven_multilingual_v2" keep working.
func elevenLabsModelToBackend(modelID string) string {
	switch strings.ToLower(strings.TrimSpace(modelID)) {
	case "kokoro", "kokoro_82m", "kokoro-82m":
		return "kokoro"
	case "pocket", "pocket_tts", "pocket-tts", "pocket-tts-1":
		return "pocket"
	default:
		return ""
	}
}

func (h *ElevenLabsHandler) logELValidation(endpoint, voiceID, reason string, c *fiber.Ctx) {
	h.lm.SendLog(h.lm.BuildLog("ELEVENLABS_TTS_VALIDATION_FAILED", "ElevenLabsTTSValidationFailed", slog.LevelWarn, map[string]interface{}{
		"endpoint":   endpoint,
		"ip":         c.IP(),
		"voice_id":   voiceID,
		"reason":     reason,
		"request_id": middleware.RequestIDFromCtx(c),
	}))
}

// ── Voices ──────────────────────────────────────────────────────────────

// ListVoicesV1 handles GET /v1/voices — legacy ElevenLabs shape
// ({"voices": [...]}), what SDK client.voices.getAll() calls.
func (h *ElevenLabsHandler) ListVoicesV1(c *fiber.Ctx) error {
	voices, err := h.collectVoices(c)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"voices": voices})
}

// ListVoicesV2 handles GET /v2/voices — the search shape with pagination
// fields. `search` filters name/description case-insensitively; page_size
// caps the page; next_page_token is the offset into the filtered list.
func (h *ElevenLabsHandler) ListVoicesV2(c *fiber.Ctx) error {
	voices, err := h.collectVoices(c)
	if err != nil {
		return err
	}

	if search := strings.ToLower(strings.TrimSpace(c.Query("search"))); search != "" {
		filtered := voices[:0]
		for _, v := range voices {
			if strings.Contains(strings.ToLower(v.Name), search) ||
				strings.Contains(strings.ToLower(v.Description), search) ||
				strings.Contains(strings.ToLower(v.VoiceID), search) {
				filtered = append(filtered, v)
			}
		}
		voices = filtered
	}

	total := len(voices)
	pageSize := c.QueryInt("page_size", 100)
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 100
	}
	offset := 0
	if tok := c.Query("next_page_token"); tok != "" {
		if n, err := strconv.Atoi(tok); err == nil && n >= 0 {
			offset = n
		}
	}

	page := []ElevenLabsVoice{}
	if offset < total {
		end := offset + pageSize
		if end > total {
			end = total
		}
		page = voices[offset:end]
	}
	hasMore := offset+pageSize < total
	var nextToken *string
	if hasMore {
		tok := strconv.Itoa(offset + pageSize)
		nextToken = &tok
	}

	return c.JSON(fiber.Map{
		"voices":          page,
		"has_more":        hasMore,
		"total_count":     total,
		"next_page_token": nextToken,
	})
}

// GetVoice handles GET /v1/voices/:voice_id.
func (h *ElevenLabsHandler) GetVoice(c *fiber.Ctx) error {
	voiceID := c.Params("voice_id")
	voices, err := h.collectVoices(c)
	if err != nil {
		return err
	}
	for _, v := range voices {
		if v.VoiceID == voiceID {
			return c.JSON(v)
		}
	}
	return elError(c, fiber.StatusNotFound, "voice_not_found", "Voice not found")
}

// AddVoice handles POST /v1/voices/add — ElevenLabs-compatible voice
// cloning. Multipart form: name (required), description, files[] (audio
// samples; the first file is used — PocketTTS clones from a single
// reference). The clone runs through the same storage core as
// /api/v1/voices/clone, so the embedding lands in the DB blob store and is
// usable from every node. Responds {"voice_id": "<uuid>"} like ElevenLabs.
func (h *ElevenLabsHandler) AddVoice(c *fiber.Ctx) error {
	userID, err := middleware.ParseUserID(c)
	if err != nil {
		return elError(c, fiber.StatusUnauthorized, "unauthorized", "Authentication required")
	}

	name := c.FormValue("name")
	if name == "" {
		return elError(c, fiber.StatusUnprocessableEntity, "invalid_request", "Voice name is required (form field: name)")
	}
	description := c.FormValue("description")

	form, err := c.MultipartForm()
	if err != nil || form == nil || len(form.File["files"]) == 0 {
		return elError(c, fiber.StatusUnprocessableEntity, "invalid_request", "Audio file is required (multipart field: files)")
	}
	file := form.File["files"][0]
	if file.Size > 10*1024*1024 {
		return elError(c, fiber.StatusUnprocessableEntity, "invalid_request", "Audio file must be 10MB or less")
	}
	f, err := file.Open()
	if err != nil {
		return elError(c, fiber.StatusInternalServerError, "file_read_error", "Failed to read uploaded file")
	}
	defer f.Close()
	audioBytes, err := io.ReadAll(f)
	if err != nil {
		return elError(c, fiber.StatusInternalServerError, "file_read_error", "Failed to read uploaded file")
	}

	voice, _, _, opErr := h.voiceHandler.cloneVoiceCore(c.UserContext(), userID, name, description, audioBytes, file.Size, "/v1/voices/add", c.IP(), middleware.RequestIDFromCtx(c))
	if opErr != nil {
		status := "clone_failed"
		switch opErr.code {
		case "VOICE_EXISTS":
			status = "voice_exists"
		case "STORAGE_ERROR", "DB_ERROR":
			status = "server_error"
		}
		return elError(c, opErr.httpStatus, status, opErr.msg)
	}

	return c.JSON(fiber.Map{"voice_id": voice.ID.String()})
}

// DeleteVoice handles DELETE /v1/voices/:voice_id — cloned voices only;
// system voices are not deletable. Responds {"status":"ok"} like ElevenLabs.
func (h *ElevenLabsHandler) DeleteVoice(c *fiber.Ctx) error {
	userID, err := middleware.ParseUserID(c)
	if err != nil {
		return elError(c, fiber.StatusUnauthorized, "unauthorized", "Authentication required")
	}

	voiceID, err := uuid.Parse(c.Params("voice_id"))
	if err != nil {
		// Not a UUID → not a cloned voice (system voices can't be deleted).
		return elError(c, fiber.StatusNotFound, "voice_not_found", "Voice not found")
	}

	if _, opErr := h.voiceHandler.deleteVoiceCore(userID, voiceID, "/v1/voices/:voice_id", c.IP(), middleware.RequestIDFromCtx(c)); opErr != nil {
		status := "server_error"
		if opErr.httpStatus == fiber.StatusNotFound {
			status = "voice_not_found"
		}
		return elError(c, opErr.httpStatus, status, opErr.msg)
	}

	return c.JSON(fiber.Map{"status": "ok"})
}

// collectVoices merges sidecar system voices (PocketTTS + Kokoro) with the
// authenticated user's cloned voices into ElevenLabs-shaped entries.
func (h *ElevenLabsHandler) collectVoices(c *fiber.Ctx) ([]ElevenLabsVoice, error) {
	voices := []ElevenLabsVoice{}

	sidecarVoices, err := h.sidecar.ListVoices()
	if err != nil {
		h.lm.SendLog(h.lm.BuildLog("VOICE_LIST_SIDECAR_FAILED", "VoiceListSidecarFailed", slog.LevelWarn, map[string]interface{}{
			"endpoint":   "/v1/voices",
			"ip":         c.IP(),
			"request_id": middleware.RequestIDFromCtx(c),
		}, err))
	} else {
		for _, sv := range sidecarVoices.Voices {
			backend := sv.Backend
			if backend == "" {
				backend = "pocket"
			}
			voices = append(voices, ElevenLabsVoice{
				VoiceID:     sv.ID,
				Name:        sv.Name,
				Category:    "premade",
				Description: sv.Description,
				Labels:      map[string]string{"backend": backend},
				Settings:    defaultELVoiceSettings,
			})
		}
	}

	// Custom cloned voices for this user (best-effort — a list without them
	// is more useful than a 500 when the DB hiccups).
	if userID, err := middleware.ParseUserID(c); err == nil && h.voiceHandler != nil {
		for _, cv := range h.voiceHandler.listCustomVoices(userID) {
			voices = append(voices, ElevenLabsVoice{
				VoiceID:     cv.ID.String(),
				Name:        cv.Name,
				Category:    "cloned",
				Description: cv.Description,
				Labels:      map[string]string{"backend": "pocket"},
				Settings:    defaultELVoiceSettings,
			})
		}
	}

	return voices, nil
}

// ── Models ──────────────────────────────────────────────────────────────

// ElevenLabsModel is one entry in the ElevenLabs GET /v1/models response.
type ElevenLabsModel struct {
	ModelID                     string               `json:"model_id"`
	Name                        string               `json:"name"`
	CanBeFinetuned              bool                 `json:"can_be_finetuned"`
	CanDoTextToSpeech           bool                 `json:"can_do_text_to_speech"`
	CanDoVoiceConversion        bool                 `json:"can_do_voice_conversion"`
	CanUseStyle                 bool                 `json:"can_use_style"`
	CanUseSpeakerBoost          bool                 `json:"can_use_speaker_boost"`
	Description                 string               `json:"description,omitempty"`
	MaximumTextLengthPerRequest int                  `json:"maximum_text_length_per_request"`
	Languages                   []ElevenLabsLanguage `json:"languages"`
}

// ElevenLabsLanguage is a supported-language entry on a model.
type ElevenLabsLanguage struct {
	LanguageID string `json:"language_id"`
	Name       string `json:"name"`
}

// Models serves the ElevenLabs-shaped GET /v1/models list. main.go routes
// the shared /v1/models path here when the request carries xi-api-key
// (ElevenLabs SDKs always send it); OpenAI clients get the OpenAI shape.
func (h *ElevenLabsHandler) Models(c *fiber.Ctx) error {
	return c.JSON([]ElevenLabsModel{
		{
			ModelID:                     "pocket_tts",
			Name:                        "PocketTTS (on-device, low latency)",
			CanDoTextToSpeech:           true,
			CanUseSpeakerBoost:          false,
			Description:                 "Kyutai PocketTTS on CoreML/ANE — low-latency streaming + voice cloning.",
			MaximumTextLengthPerRequest: 5000,
			Languages:                   []ElevenLabsLanguage{{LanguageID: "en", Name: "English"}},
		},
		{
			ModelID:                     "kokoro_82m",
			Name:                        "Kokoro 82M (on-device, multilingual)",
			CanDoTextToSpeech:           true,
			Description:                 "Kokoro 82M on CoreML/ANE — higher quality, English/Mandarin/Japanese. Batch only.",
			MaximumTextLengthPerRequest: 5000,
			Languages: []ElevenLabsLanguage{
				{LanguageID: "en", Name: "English"},
				{LanguageID: "zh", Name: "Mandarin"},
				{LanguageID: "ja", Name: "Japanese"},
			},
		},
	})
}
