package handlers

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	ws "github.com/fasthttp/websocket"
	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/shaunagostinho/gotranscribesrv/internal/logging"
	"github.com/shaunagostinho/gotranscribesrv/internal/metrics"
	"github.com/shaunagostinho/gotranscribesrv/internal/middleware"
	"github.com/shaunagostinho/gotranscribesrv/internal/pii"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
	"gorm.io/gorm"
)

// WatsonHandler handles IBM Watson Speech-to-Text compatible endpoints.
//
// NOTE: Session-end logs for the WebSocket streaming path currently
// do NOT include transcript text (only audio bytes / duration /
// process time). If transcript text is added to SESSION_ENDED in the
// future, it MUST be run through the PII redactor before being added
// to BuildLog's AdditionalData — see the Recognize() handler above
// for the pattern.
type WatsonHandler struct {
	sidecar    *sidecar.Client
	redactor   *pii.Redactor
	db         *gorm.DB
	defaultITN bool
	lm         *logging.LogManager
}

// NewWatsonHandler creates a new WatsonHandler.
func NewWatsonHandler(sc *sidecar.Client, redactor *pii.Redactor, db *gorm.DB, defaultITN bool, lm *logging.LogManager) *WatsonHandler {
	return &WatsonHandler{sidecar: sc, redactor: redactor, db: db, defaultITN: defaultITN, lm: lm}
}

// --- Watson Response Types ---

// watsonSpeechResults is the top-level Watson STT response.
type watsonSpeechResults struct {
	Results       []watsonResult       `json:"results"`
	ResultIndex   int                  `json:"result_index"`
	SpeakerLabels []watsonSpeakerLabel `json:"speaker_labels,omitempty"`
}

// watsonResult represents a single recognition result segment.
type watsonResult struct {
	Alternatives []watsonAlternative `json:"alternatives"`
	Final        bool                `json:"final"`
}

// watsonAlternative represents one transcription alternative.
type watsonAlternative struct {
	Transcript     string          `json:"transcript"`
	Confidence     float64         `json:"confidence"`
	Timestamps     [][]interface{} `json:"timestamps,omitempty"`
	WordConfidence [][]interface{} `json:"word_confidence,omitempty"`
}

// watsonSpeakerLabel represents a speaker diarization label.
type watsonSpeakerLabel struct {
	From       float64 `json:"from"`
	To         float64 `json:"to"`
	Speaker    int     `json:"speaker"`
	Confidence float64 `json:"confidence"`
	Final      bool    `json:"final"`
}

// Recognize handles the Watson-compatible HTTP batch transcription endpoint.
// POST /v1/recognize
func (h *WatsonHandler) Recognize(c *fiber.Ctx) error {
	// Watson sends raw audio in the request body (not multipart)
	audioBytes := c.Body()
	if len(audioBytes) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error":            "no audio data",
			"code":             400,
			"code_description": "Bad Request",
		})
	}

	// 100MB limit
	if len(audioBytes) > 100*1024*1024 {
		return c.Status(fiber.StatusRequestEntityTooLarge).JSON(fiber.Map{
			"error":            "audio data exceeds maximum size of 100MB",
			"code":             413,
			"code_description": "Request Entity Too Large",
		})
	}

	// Parse query parameters
	timestamps := c.Query("timestamps", "false") == "true"
	speakerLabels := c.Query("speaker_labels", "false") == "true"
	wordConfidence := c.Query("word_confidence", "false") == "true"

	// Determine filename from content type for sidecar
	rawContentType := c.Get("Content-Type", "application/octet-stream")
	// Strip parameters (e.g. "audio/mulaw;rate=8000" → "audio/mulaw")
	contentType := rawContentType
	if idx := strings.IndexByte(contentType, ';'); idx != -1 {
		contentType = strings.TrimSpace(contentType[:idx])
	}
	filename := filenameFromContentType(contentType)

	// For mulaw/alaw raw audio, wrap in a WAV header so the sidecar/CoreAudio can decode it
	isMulaw := contentType == "audio/mulaw" || contentType == "audio/basic"
	isAlaw := contentType == "audio/alaw"
	if isMulaw || isAlaw {
		sampleRate := parseSampleRate(rawContentType, 8000) // default 8kHz for telephony
		var fmtCode uint16 = 7                              // mulaw
		if isAlaw {
			fmtCode = 6 // alaw
		}
		audioBytes = wrapRawInWAV(audioBytes, fmtCode, 1, uint32(sampleRate), 8)
		filename = "audio.wav"
		slog.InfoContext(c.UserContext(), "[Watson] Wrapped raw audio in WAV header",
			"format", contentType, "sample_rate", sampleRate, "wrapped_size", len(audioBytes))
	} else {
		// Sniff actual audio format from magic bytes and override if Content-Type is wrong
		detectedFormat := sniffAudioFormat(audioBytes)
		if detectedFormat != "" && detectedFormat != filename {
			slog.InfoContext(c.UserContext(), "[Watson] Content-Type/magic mismatch, using detected format",
				"content_type", rawContentType,
				"content_type_filename", filename,
				"detected_filename", detectedFormat)
			filename = detectedFormat
		}
	}

	slog.InfoContext(c.UserContext(), "[Watson] POST /v1/recognize",
		"content_type", rawContentType,
		"body_size", len(audioBytes),
		"resolved_filename", filename,
		"timestamps", timestamps,
		"speaker_labels", speakerLabels,
		"word_confidence", wordConfidence)

	// model, max_alternatives, profanity_filter, smart_formatting, inactivity_timeout
	// are accepted for compatibility but ignored

	// ITN: respect the per-request "itn" query param if present, otherwise
	// fall back to the server-wide default (cfg.EnableITN).
	itnVal := h.defaultITN
	if v := c.Query("itn"); v != "" {
		itnVal = v != "false"
	}

	h.lm.SendLog(h.lm.BuildLog("WATSON_RECOGNIZE_RECEIVED", "WatsonRecognizeReceived", slog.LevelInfo, map[string]interface{}{
		"endpoint":        "/v1/recognize",
		"content_type":    rawContentType,
		"body_size":       len(audioBytes),
		"resolved_file":   filename,
		"timestamps":      timestamps,
		"speaker_labels":  speakerLabels,
		"word_confidence": wordConfidence,
		"language":        c.Query("language", "en"),
		"itn":             itnVal,
		"ip":              c.IP(),
		"request_id":      middleware.RequestIDFromCtx(c),
	}))

	result, err := h.sidecar.Transcribe(c.UserContext(), sidecar.TranscribeRequest{
		Audio:    audioBytes,
		Filename: filename,
		Language: c.Query("language", "en"),
		Diarize:  speakerLabels,
		ITN:      &itnVal,
	})
	if err != nil {
		slog.ErrorContext(c.UserContext(), "Watson transcription failed", "error", err, "content_type", rawContentType)
		h.lm.SendLog(h.lm.BuildLog("WATSON_RECOGNIZE_FAILED", "WatsonRecognizeFailed", slog.LevelError, map[string]interface{}{
			"endpoint":     "/v1/recognize",
			"content_type": rawContentType,
			"body_size":    len(audioBytes),
			"request_id":   middleware.RequestIDFromCtx(c),
		}, err))
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error":            "transcription service unavailable",
			"code":             503,
			"code_description": "Service Unavailable",
		})
	}

	// Redact transcript text for log emission. Response body is left untouched.
	redactedText, piiItems, piiErr := h.redactor.RedactText(c.UserContext(), result.Text)
	if piiErr != nil {
		h.lm.SendLog(h.lm.BuildLog("PII_REDACTOR_ERROR", "PIIRedactorError", slog.LevelWarn, map[string]interface{}{
			"endpoint":   "/v1/recognize",
			"text_len":   len(result.Text),
			"request_id": middleware.RequestIDFromCtx(c),
		}, piiErr))
	}
	completedFields := map[string]interface{}{
		"endpoint":      "/v1/recognize",
		"content_type":  rawContentType,
		"audio_ms":      int(result.Duration * 1000),
		"asr_ms":        result.ProcessTimeMs,
		"word_count":    len(result.Words),
		"segment_count": len(result.Segments),
		"diarized":      result.Diarized,
		"num_speakers":  result.NumSpeakers,
		"itn_applied":   result.ITNApplied,
		"transcript":    redactedText,
		"pii_redacted":  len(piiItems),
		"request_id":    middleware.RequestIDFromCtx(c),
	}
	if len(piiItems) > 0 {
		completedFields["pii_entity_types"] = piiEntityTypes(piiItems)
	}
	h.lm.SendLog(h.lm.BuildLog("WATSON_RECOGNIZE_COMPLETED", "WatsonRecognizeCompleted", slog.LevelInfo, completedFields))

	// Store metadata for usage tracking
	c.Locals("audio_duration_ms", int(result.Duration*1000))
	c.Locals("diarized", result.Diarized)
	c.Locals("usage_meta", map[string]interface{}{
		"file_size_bytes": len(audioBytes),
		"content_type":    rawContentType,
		"word_count":      len(result.Words),
		"segment_count":   len(result.Segments),
		"language":        c.Query("language", "en"),
	})

	resp := buildWatsonHTTPResponse(result, timestamps, wordConfidence, speakerLabels)
	return c.JSON(resp)
}

// Upgrade returns the Fiber middleware that upgrades HTTP to WebSocket for Watson streaming.
func (h *WatsonHandler) Upgrade() fiber.Handler {
	return websocket.New(h.handleStream)
}

// handleStream proxies WebSocket frames between a Watson-compatible client and
// the Swift sidecar's /stream endpoint, translating the event protocol.
func (h *WatsonHandler) handleStream(c *websocket.Conn) {
	defer c.Close()

	// Limit incoming message size to 1MB to prevent memory exhaustion
	c.SetReadLimit(1 * 1024 * 1024)

	// Reuse the id minted by the HTTP RequestID middleware (if present)
	// so the access/upgrade logs correlate with this session.
	requestID, _ := c.Locals(middleware.RequestIDLocalKey).(string)
	if requestID == "" {
		requestID = uuid.New().String()
		c.Locals(middleware.RequestIDLocalKey, requestID)
	}
	timestamps := c.Query("timestamps", "false") == "true"
	wordConfidence := c.Query("word_confidence", "false") == "true"
	speakerLabels := c.Query("speaker_labels", "false") == "true"
	interimResults := c.Query("interim_results", "true") == "true"

	// Connect to Swift sidecar /stream WebSocket
	sidecarURL := h.sidecar.StreamURL()
	u, err := url.Parse(sidecarURL)
	if err != nil {
		slog.Error("[Watson] invalid sidecar stream URL", "error", err, "request_id", requestID)
		_ = c.WriteJSON(fiber.Map{"error": "internal configuration error"})
		return
	}

	// Forward relevant query params to sidecar
	q := u.Query()
	for _, param := range []string{"language", "diarize", "encoding", "sample_rate", "itn"} {
		if v := c.Query(param); v != "" {
			q.Set(param, v)
		}
	}
	// ITN: if the client didn't pass ?itn= and the server-wide default is
	// off, inject it so ENABLE_ITN=false in .env actually disables ITN
	// for Watson-compat clients.
	if c.Query("itn") == "" && !h.defaultITN {
		q.Set("itn", "false")
	}
	if speakerLabels {
		q.Set("diarize", "true")
	}
	u.RawQuery = q.Encode()

	slog.Info("[Watson] Connecting to sidecar", "request_id", requestID, "url", u.String())

	sidecarConn, _, err := ws.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		slog.Error("[Watson] failed to connect to sidecar /stream WebSocket", "error", err, "request_id", requestID)
		h.lm.SendLog(h.lm.BuildLog("WATSON_CONNECT_FAILED", "WatsonConnectFailed", slog.LevelError, map[string]interface{}{
			"endpoint":   "/v1/recognize",
			"request_id": requestID,
		}, err))
		_ = c.WriteJSON(fiber.Map{"error": "transcription service unavailable"})
		return
	}
	defer sidecarConn.Close()

	slog.Info("[Watson] Watson-compat streaming session started", "request_id", requestID,
		"interim_results", interimResults, "timestamps", timestamps)
	metrics.ActiveWebSocketConnections.WithLabelValues("watson").Inc()
	defer metrics.ActiveWebSocketConnections.WithLabelValues("watson").Dec()

	h.lm.SendLog(h.lm.BuildLog("WATSON_SESSION_STARTED", "WatsonSessionStarted", slog.LevelInfo, map[string]interface{}{
		"endpoint":        "/v1/recognize",
		"request_id":      requestID,
		"interim_results": interimResults,
		"timestamps":      timestamps,
		"speaker_labels":  speakerLabels,
		"word_confidence": wordConfidence,
	}))

	// Send Watson "listening" state
	_ = c.WriteJSON(fiber.Map{"state": "listening"})

	var totalAudioBytes int
	var firstAudioAt time.Time
	var lastResultAt time.Time
	errCh := make(chan error, 2)

	// Client → Sidecar: forward binary audio and translate Watson control messages
	go func() {
		var frameCount int
		for {
			msgType, msg, err := c.ReadMessage()
			if err != nil {
				slog.Info("[Watson] Client read error", "error", err, "request_id", requestID)
				errCh <- err
				return
			}

			// Text frames may be Watson control messages
			if msgType == websocket.TextMessage {
				slog.Info("[Watson] Received text from client", "text", string(msg), "request_id", requestID)
				var ctrl map[string]interface{}
				if json.Unmarshal(msg, &ctrl) == nil {
					if action, ok := ctrl["action"].(string); ok {
						switch action {
						case "start":
							// Watson "start" message — extract params if provided
							slog.Info("[Watson] Start message from client", "request_id", requestID)
							// Check for inline parameters
							if ts, ok := ctrl["timestamps"].(bool); ok {
								timestamps = ts
							}
							if wc, ok := ctrl["word_confidence"].(bool); ok {
								wordConfidence = wc
							}
							if ir, ok := ctrl["interim_results"].(bool); ok {
								interimResults = ir
							}
							continue
						case "stop":
							slog.Info("[Watson] Stop message from client, forwarding to sidecar",
								"request_id", requestID, "total_frames", frameCount, "total_bytes", totalAudioBytes)
							_ = sidecarConn.WriteMessage(ws.TextMessage,
								[]byte(`{"type":"CloseStream"}`))
							continue
						}
					}
				}
				// Forward other text messages
				if err := sidecarConn.WriteMessage(msgType, msg); err != nil {
					errCh <- err
					return
				}
				continue
			}

			// Binary audio frame
			frameCount++
			totalAudioBytes += len(msg)
			if frameCount == 1 {
				firstAudioAt = time.Now()
			}
			if frameCount%50 == 1 {
				slog.Info("[Watson] Forwarding audio to sidecar", "frame", frameCount,
					"bytes", len(msg), "total_bytes", totalAudioBytes, "request_id", requestID)
			}

			if err := sidecarConn.WriteMessage(msgType, msg); err != nil {
				slog.Error("[Watson] Failed to forward audio to sidecar", "error", err, "request_id", requestID)
				h.lm.SendLog(h.lm.BuildLog("WATSON_FORWARD_FAILED", "WatsonForwardFailed", slog.LevelError, map[string]interface{}{
					"endpoint":   "/v1/recognize",
					"request_id": requestID,
				}, err))
				errCh <- err
				return
			}
		}
	}()

	// Sidecar → Client: translate internal events to Watson format
	go func() {
		for {
			_, msg, err := sidecarConn.ReadMessage()
			if err != nil {
				slog.Info("[Watson] Sidecar read error", "error", err, "request_id", requestID)
				errCh <- err
				return
			}

			// Debug-level only — the raw message body contains transcript text
			// (PII). At info level we surface only the parsed event type.
			slog.Debug("[Watson] Received from sidecar", "msg", string(msg[:min(len(msg), 500)]),
				"request_id", requestID)

			var evt sidecarStreamEvent
			if json.Unmarshal(msg, &evt) != nil {
				slog.Warn("[Watson] Non-JSON from sidecar, forwarding raw", "request_id", requestID)
				if writeErr := c.WriteMessage(websocket.TextMessage, msg); writeErr != nil {
					errCh <- writeErr
					return
				}
				continue
			}

			switch evt.Type {
			case "partial":
				if !interimResults {
					continue
				}
				watsonEvt := buildWatsonStreamResult(evt, false, timestamps, wordConfidence)
				if err := c.WriteJSON(watsonEvt); err != nil {
					errCh <- err
					return
				}
				slog.Info("[Watson] Sent interim result to client", "request_id", requestID)

			case "final":
				watsonEvt := buildWatsonStreamResult(evt, true, timestamps, wordConfidence)
				if err := c.WriteJSON(watsonEvt); err != nil {
					errCh <- err
					return
				}
				lastResultAt = time.Now()
				slog.Info("[Watson] Sent final result to client", "request_id", requestID)

			case "ready":
				slog.Info("[Watson] Sidecar ready", "request_id", requestID)
				continue

			case "error":
				slog.Error("[Watson] Sidecar error", "message", evt.Message, "request_id", requestID)
				h.lm.SendLog(h.lm.BuildLog("WATSON_SIDECAR_ERROR", "WatsonSidecarError", slog.LevelError, map[string]interface{}{
					"endpoint":   "/v1/recognize",
					"request_id": requestID,
				}, evt.Message))
				_ = c.WriteJSON(fiber.Map{"error": evt.Message})
				errCh <- nil
				return

			case "done":
				slog.Info("[Watson] Sidecar done, ending session", "request_id", requestID)
				// Watson sends a "listening" state to indicate end
				_ = c.WriteJSON(fiber.Map{"state": "listening"})
				errCh <- nil
				return

			default:
				slog.Info("[Watson] Unknown sidecar event, forwarding", "type", evt.Type, "request_id", requestID)
				if writeErr := c.WriteMessage(websocket.TextMessage, msg); writeErr != nil {
					errCh <- writeErr
					return
				}
			}
		}
	}()

	<-errCh

	// Log usage — processing time = first audio frame → last final result
	audioDurationMs := 0
	if totalAudioBytes > 0 {
		audioDurationMs = totalAudioBytes / 32 // PCM 16-bit 16kHz mono = 32 bytes/ms
	}
	processTimeMs := 0
	if !firstAudioAt.IsZero() && !lastResultAt.IsZero() {
		processTimeMs = int(lastResultAt.Sub(firstAudioAt).Milliseconds())
	}
	userID, _ := c.Locals("user_id").(string)
	apiKeyID, _ := c.Locals("api_key_id").(string)
	middleware.LogWebSocketUsage(h.db, userID, apiKeyID, "asr_watson_stream",
		audioDurationMs, processTimeMs, speakerLabels)

	slog.Info("[Watson] Watson-compat session ended", "request_id", requestID,
		"audio_bytes", totalAudioBytes, "audio_duration_ms", audioDurationMs,
		"process_ms", processTimeMs)

	h.lm.SendLog(h.lm.BuildLog("WATSON_SESSION_ENDED", "WatsonSessionEnded", slog.LevelInfo, map[string]interface{}{
		"endpoint":          "/v1/recognize",
		"request_id":        requestID,
		"audio_bytes":       totalAudioBytes,
		"audio_duration_ms": audioDurationMs,
		"process_ms":        processTimeMs,
		"speaker_labels":    speakerLabels,
		"realtime_x":        realtimeFactor(audioDurationMs, processTimeMs),
	}))
}

// --- Helper Functions ---

// buildWatsonHTTPResponse converts a sidecar TranscribeResponse to Watson format.
func buildWatsonHTTPResponse(result *sidecar.TranscribeResponse, timestamps, wordConfidence, speakerLabels bool) watsonSpeechResults {
	// Build alternatives per segment or as a single result
	var results []watsonResult

	if len(result.Segments) > 0 {
		for _, seg := range result.Segments {
			alt := watsonAlternative{
				Transcript: seg.Text,
				Confidence: 0.99,
			}

			if timestamps {
				alt.Timestamps = buildTimestampsForSegment(result.Words, seg.Start, seg.End)
			}
			if wordConfidence {
				alt.WordConfidence = buildWordConfidenceForSegment(result.Words, seg.Start, seg.End)
			}

			results = append(results, watsonResult{
				Alternatives: []watsonAlternative{alt},
				Final:        true,
			})
		}
	} else {
		// No segments — single result with full text
		alt := watsonAlternative{
			Transcript: result.Text,
			Confidence: 0.99,
		}

		if timestamps && len(result.Words) > 0 {
			alt.Timestamps = buildTimestamps(result.Words)
		}
		if wordConfidence && len(result.Words) > 0 {
			alt.WordConfidence = buildWordConfidences(result.Words)
		}

		results = []watsonResult{{
			Alternatives: []watsonAlternative{alt},
			Final:        true,
		}}
	}

	resp := watsonSpeechResults{
		Results:     results,
		ResultIndex: 0,
	}

	// Append speaker labels if diarization was requested and data is available
	if speakerLabels && result.Diarized && len(result.Words) > 0 {
		resp.SpeakerLabels = buildSpeakerLabels(result.Words)
	}

	return resp
}

// buildWatsonStreamResult converts a sidecar stream event to a Watson result.
func buildWatsonStreamResult(evt sidecarStreamEvent, isFinal, timestamps, wordConfidence bool) watsonSpeechResults {
	alt := watsonAlternative{
		Transcript: evt.Text,
		Confidence: 0.99,
	}

	if timestamps && len(evt.Words) > 0 {
		alt.Timestamps = buildTimestamps(wordsFromSidecar(evt.Words))
	}
	if wordConfidence && len(evt.Words) > 0 {
		alt.WordConfidence = buildWordConfidences(wordsFromSidecar(evt.Words))
	}

	// On partial results, don't include confidence
	if !isFinal {
		alt.Confidence = 0
	}

	return watsonSpeechResults{
		Results: []watsonResult{{
			Alternatives: []watsonAlternative{alt},
			Final:        isFinal,
		}},
		ResultIndex: 0,
	}
}

// buildTimestamps creates Watson-format timestamp triples [word, start, end].
func buildTimestamps(words []sidecar.Word) [][]interface{} {
	ts := make([][]interface{}, 0, len(words))
	for _, w := range words {
		ts = append(ts, []interface{}{w.Word, w.Start, w.End})
	}
	return ts
}

// buildTimestampsForSegment filters words by segment time range and builds timestamps.
func buildTimestampsForSegment(words []sidecar.Word, segStart, segEnd float64) [][]interface{} {
	var filtered []sidecar.Word
	for _, w := range words {
		if w.Start >= segStart && w.End <= segEnd {
			filtered = append(filtered, w)
		}
	}
	return buildTimestamps(filtered)
}

// buildWordConfidences creates Watson-format word confidence pairs [word, confidence].
func buildWordConfidences(words []sidecar.Word) [][]interface{} {
	wc := make([][]interface{}, 0, len(words))
	for _, w := range words {
		wc = append(wc, []interface{}{w.Word, 0.99})
	}
	return wc
}

// buildWordConfidenceForSegment filters words by segment time range and builds confidences.
func buildWordConfidenceForSegment(words []sidecar.Word, segStart, segEnd float64) [][]interface{} {
	var filtered []sidecar.Word
	for _, w := range words {
		if w.Start >= segStart && w.End <= segEnd {
			filtered = append(filtered, w)
		}
	}
	return buildWordConfidences(filtered)
}

// buildSpeakerLabels converts diarized words into Watson speaker_labels format.
func buildSpeakerLabels(words []sidecar.Word) []watsonSpeakerLabel {
	speakerMap := make(map[string]int) // map speaker name → speaker index
	nextSpeaker := 0

	labels := make([]watsonSpeakerLabel, 0, len(words))
	for _, w := range words {
		if w.Speaker == "" {
			continue
		}

		speakerIdx, ok := speakerMap[w.Speaker]
		if !ok {
			speakerIdx = nextSpeaker
			speakerMap[w.Speaker] = speakerIdx
			nextSpeaker++
		}

		labels = append(labels, watsonSpeakerLabel{
			From:       w.Start,
			To:         w.End,
			Speaker:    speakerIdx,
			Confidence: 0.99,
			Final:      true,
		})
	}
	return labels
}

// wordsFromSidecar converts sidecar.Word slice to the same type (identity for stream events
// which use a different word type).
func wordsFromSidecar(words []sidecar.Word) []sidecar.Word {
	return words
}

// filenameFromContentType maps an audio Content-Type to a reasonable filename
// for the sidecar.
func filenameFromContentType(ct string) string {
	switch ct {
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "audio.wav"
	case "audio/flac":
		return "audio.flac"
	case "audio/mp3", "audio/mpeg":
		return "audio.mp3"
	case "audio/ogg", "audio/ogg;codecs=opus":
		return "audio.ogg"
	case "audio/webm", "audio/webm;codecs=opus":
		return "audio.webm"
	case "audio/mulaw", "audio/basic":
		return "audio.wav"
	case "audio/l16":
		return "audio.pcm"
	default:
		return "audio.wav"
	}
}

// sniffAudioFormat detects audio format from magic bytes at the start of the data.
// Returns the appropriate filename (e.g. "audio.mp3") or empty string if unknown.
// NOTE: This should NOT be called for known raw formats (mulaw, alaw) since raw
// audio has no magic bytes and will false-positive as MP3.
func sniffAudioFormat(data []byte) string {
	if len(data) < 12 {
		return ""
	}

	// WAV: "RIFF....WAVE" — check first (most reliable signature)
	if data[0] == 'R' && data[1] == 'I' && data[2] == 'F' && data[3] == 'F' &&
		data[8] == 'W' && data[9] == 'A' && data[10] == 'V' && data[11] == 'E' {
		return "audio.wav"
	}

	// FLAC: "fLaC"
	if data[0] == 'f' && data[1] == 'L' && data[2] == 'a' && data[3] == 'C' {
		return "audio.flac"
	}

	// OGG: "OggS"
	if data[0] == 'O' && data[1] == 'g' && data[2] == 'g' && data[3] == 'S' {
		return "audio.ogg"
	}

	// WebM: starts with EBML header (0x1A 0x45 0xDF 0xA3)
	if data[0] == 0x1A && data[1] == 0x45 && data[2] == 0xDF && data[3] == 0xA3 {
		return "audio.webm"
	}

	// MP3: ID3 tag (most reliable MP3 signature)
	if data[0] == 'I' && data[1] == 'D' && data[2] == '3' {
		return "audio.mp3"
	}

	// MP3: MPEG sync word — require stricter check to avoid false positives
	// on raw audio formats. Frame sync = 0xFFE0, plus valid MPEG version/layer bits.
	if data[0] == 0xFF && (data[1]&0xE6) == 0xE2 {
		return "audio.mp3"
	}

	return ""
}

// wrapRawInWAV wraps raw audio data in a minimal WAV header.
// fmtCode: 1=PCM, 6=A-law, 7=μ-law
func wrapRawInWAV(raw []byte, fmtCode, channels uint16, sampleRate uint32, bitsPerSample uint16) []byte {
	dataSize := uint32(len(raw))
	blockAlign := channels * (bitsPerSample / 8)
	byteRate := sampleRate * uint32(blockAlign)

	var buf bytes.Buffer
	buf.Grow(44 + len(raw))

	// RIFF header
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+dataSize)) // file size - 8
	buf.WriteString("WAVE")

	// fmt sub-chunk
	buf.WriteString("fmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))    // sub-chunk size
	binary.Write(&buf, binary.LittleEndian, fmtCode)       // audio format
	binary.Write(&buf, binary.LittleEndian, channels)      // num channels
	binary.Write(&buf, binary.LittleEndian, sampleRate)    // sample rate
	binary.Write(&buf, binary.LittleEndian, byteRate)      // byte rate
	binary.Write(&buf, binary.LittleEndian, blockAlign)    // block align
	binary.Write(&buf, binary.LittleEndian, bitsPerSample) // bits per sample

	// data sub-chunk
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, dataSize)
	buf.Write(raw)

	return buf.Bytes()
}

// parseSampleRate extracts the rate parameter from a Content-Type string.
// e.g. "audio/mulaw;rate=8000" → 8000. Returns defaultRate if not found.
func parseSampleRate(ct string, defaultRate int) int {
	for _, part := range strings.Split(ct, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "rate=") {
			var rate int
			if _, err := fmt.Sscanf(part, "rate=%d", &rate); err == nil && rate > 0 {
				return rate
			}
		}
	}
	return defaultRate
}
