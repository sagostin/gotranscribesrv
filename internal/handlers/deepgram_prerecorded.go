package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/shaunagostinho/gotranscribesrv/internal/logging"
	"github.com/shaunagostinho/gotranscribesrv/internal/middleware"
	"github.com/shaunagostinho/gotranscribesrv/internal/pii"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
)

// DeepgramPreRecordedHandler handles POST /v1/listen — Deepgram's
// pre-recorded (REST) transcription API. WS GET /v1/listen (streaming)
// is DeepgramHandler in deepgram.go; Fiber routes are method-specific,
// so both coexist on the same path.
//
// Two body modes, matching Deepgram:
//   - Content-Type: application/json  {"url": "https://…/file.wav"}
//   - any audio Content-Type          raw audio bytes in the body
//
// Honored query params: language, diarize, diarize_model (enables
// diarization), smart_format/numerals (map to ITN), itn (extension),
// utterances, detect_language. Everything else (punctuate — always on,
// keywords/keyterm, redact, search, replace, paragraphs, summarize,
// topics, intents, sentiment, multichannel, callback, …) is accepted
// and ignored, per the documented compatibility posture.
type DeepgramPreRecordedHandler struct {
	sidecar    *sidecar.Client
	redactor   *pii.Redactor
	defaultITN bool
	lm         *logging.LogManager
	fetcher    *http.Client
}

// NewDeepgramPreRecordedHandler creates a new DeepgramPreRecordedHandler.
func NewDeepgramPreRecordedHandler(sc *sidecar.Client, redactor *pii.Redactor, defaultITN bool, lm *logging.LogManager) *DeepgramPreRecordedHandler {
	return &DeepgramPreRecordedHandler{
		sidecar:    sc,
		redactor:   redactor,
		defaultITN: defaultITN,
		lm:         lm,
		fetcher:    &http.Client{Timeout: 60 * time.Second},
	}
}

const dgPreRecordedMaxBytes = 100 * 1024 * 1024 // 100MB, same as /api/v1/asr

// --- Deepgram pre-recorded response types (ListenV1Response) ---

type dgPreRecordedResponse struct {
	Metadata dgPreRecordedMetadata `json:"metadata"`
	Results  dgPreRecordedResults  `json:"results"`
}

type dgPreRecordedMetadata struct {
	TransactionKey string                       `json:"transaction_key"`
	RequestID      string                       `json:"request_id"`
	SHA256         string                       `json:"sha256"`
	Created        string                       `json:"created"`
	Duration       float64                      `json:"duration"`
	Channels       int                          `json:"channels"`
	Models         []string                     `json:"models"`
	ModelInfo      map[string]map[string]string `json:"model_info"`
}

type dgPreRecordedResults struct {
	Channels   []dgPreRecordedChannel   `json:"channels"`
	Utterances []dgPreRecordedUtterance `json:"utterances,omitempty"`
}

type dgPreRecordedChannel struct {
	Alternatives     []dgPreRecordedAlternative `json:"alternatives"`
	DetectedLanguage string                     `json:"detected_language,omitempty"`
}

type dgPreRecordedAlternative struct {
	Transcript string              `json:"transcript"`
	Confidence float64             `json:"confidence"`
	Words      []dgPreRecordedWord `json:"words"`
}

type dgPreRecordedWord struct {
	Word           string  `json:"word"`
	Start          float64 `json:"start"`
	End            float64 `json:"end"`
	Confidence     float64 `json:"confidence"`
	PunctuatedWord string  `json:"punctuated_word"`
	Speaker        *int    `json:"speaker,omitempty"`
}

type dgPreRecordedUtterance struct {
	Start      float64             `json:"start"`
	End        float64             `json:"end"`
	Confidence float64             `json:"confidence"`
	Channel    int                 `json:"channel"`
	Transcript string              `json:"transcript"`
	Words      []dgPreRecordedWord `json:"words"`
	Speaker    *int                `json:"speaker,omitempty"`
	ID         string              `json:"id"`
}

// dgError renders Deepgram's REST error shape.
func dgError(c *fiber.Ctx, status int, code, msg, requestID string) error {
	return c.Status(status).JSON(fiber.Map{
		"err_code":   code,
		"err_msg":    msg,
		"request_id": requestID,
	})
}

// Listen handles POST /v1/listen (pre-recorded).
func (h *DeepgramPreRecordedHandler) Listen(c *fiber.Ctx) error {
	requestID, _ := c.Locals(middleware.RequestIDLocalKey).(string)
	if requestID == "" {
		requestID = uuid.New().String()
		c.Locals(middleware.RequestIDLocalKey, requestID)
	}

	// --- Acquire audio bytes: JSON {"url": …} or raw body ---
	var audio []byte
	var sourceName string
	ct := c.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		var body struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(c.Body(), &body); err != nil || body.URL == "" {
			return dgError(c, fiber.StatusBadRequest, "INVALID_REQUEST",
				"JSON body must contain a 'url' field", requestID)
		}
		var err error
		audio, err = h.fetchURL(c.UserContext(), body.URL)
		if err != nil {
			slog.WarnContext(c.UserContext(), "deepgram prerecorded url fetch failed",
				"error", err, "url_host", urlHost(body.URL), "request_id", requestID)
			return dgError(c, fiber.StatusBadRequest, "INVALID_REQUEST",
				fmt.Sprintf("failed to fetch audio from url: %v", err), requestID)
		}
		sourceName = body.URL
	} else {
		audio = c.Body()
		sourceName = filenameForContentType(ct)
	}

	if len(audio) == 0 {
		return dgError(c, fiber.StatusBadRequest, "INVALID_REQUEST",
			"no audio data supplied (raw body or JSON {\"url\": …} required)", requestID)
	}
	if len(audio) > dgPreRecordedMaxBytes {
		return dgError(c, fiber.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE",
			"audio must be less than 100MB", requestID)
	}

	// --- Query params ---
	language := c.Query("language", "en")
	// diarize=true (deprecated upstream) or diarize_model=<v> both enable it.
	diarize := c.Query("diarize") == "true" || c.Query("diarize_model") != ""
	wantUtterances := c.Query("utterances") == "true"
	detectLanguage := c.Query("detect_language") != ""

	// ITN mapping: smart_format / numerals are the Deepgram switches whose
	// behavior our ITN covers; explicit ?itn= (extension) wins, then those,
	// then the server default.
	itnVal := h.defaultITN
	if c.Query("smart_format") == "true" || c.Query("numerals") == "true" {
		itnVal = true
	}
	if v := c.Query("itn"); v != "" {
		itnVal = v != "false"
	}

	h.lm.SendLog(h.lm.BuildLog("DG_PRERECORDED_RECEIVED", "DeepgramPreRecordedReceived", slog.LevelInfo, map[string]interface{}{
		"endpoint":    "/v1/listen",
		"ip":          c.IP(),
		"request_id":  requestID,
		"user_agent":  c.Get("User-Agent"),
		"audio_bytes": len(audio),
		"source":      sourceKind(ct),
		"language":    language,
		"diarize":     diarize,
		"itn":         itnVal,
		"utterances":  wantUtterances,
	}))

	start := time.Now()
	result, err := h.sidecar.Transcribe(c.UserContext(), sidecar.TranscribeRequest{
		Audio:    audio,
		Filename: sourceName,
		Language: language,
		Diarize:  diarize,
		ITN:      &itnVal,
	})
	sidecarMs := int(time.Since(start).Milliseconds())
	if err != nil {
		slog.ErrorContext(c.UserContext(), "deepgram prerecorded transcription failed",
			"error", err, "request_id", requestID)
		h.lm.SendLog(h.lm.BuildLog("DG_PRERECORDED_FAILED", "DeepgramPreRecordedFailed", slog.LevelError, map[string]interface{}{
			"endpoint":    "/v1/listen",
			"ip":          c.IP(),
			"request_id":  requestID,
			"audio_bytes": len(audio),
			"sidecar_ms":  sidecarMs,
		}, err))
		return dgError(c, fiber.StatusBadGateway, "ASR_ERROR",
			"transcription service unavailable", requestID)
	}

	// PII-redact the transcript before it touches Loki (same pattern as
	// the streaming handlers — the client gets the raw text).
	redactedText, piiItems, piiErr := h.redactor.RedactText(c.UserContext(), result.Text)
	if piiErr != nil {
		h.lm.SendLog(h.lm.BuildLog("PII_REDACTOR_ERROR", "PIIRedactorError", slog.LevelWarn, map[string]interface{}{
			"endpoint":   "/v1/listen",
			"ip":         c.IP(),
			"text_len":   len(result.Text),
			"request_id": requestID,
		}, piiErr))
	}
	completedFields := map[string]interface{}{
		"endpoint":     "/v1/listen",
		"ip":           c.IP(),
		"request_id":   requestID,
		"audio_bytes":  len(audio),
		"audio_ms":     int(result.Duration * 1000),
		"sidecar_ms":   sidecarMs,
		"asr_ms":       result.ProcessTimeMs,
		"model":        result.Model,
		"language":     language,
		"diarized":     result.Diarized,
		"num_speakers": result.NumSpeakers,
		"word_count":   len(result.Words),
		"transcript":   redactedText,
		"pii_redacted": len(piiItems),
	}
	if len(piiItems) > 0 {
		completedFields["pii_entity_types"] = piiEntityTypes(piiItems)
	}
	h.lm.SendLog(h.lm.BuildLog("DG_PRERECORDED_COMPLETED", "DeepgramPreRecordedCompleted", slog.LevelInfo, completedFields))

	// Usage-tracking middleware picks these up.
	c.Locals("audio_duration_ms", int(result.Duration*1000))
	c.Locals("diarized", result.Diarized)
	c.Locals("usage_meta", map[string]interface{}{
		"audio_bytes": len(audio),
		"word_count":  len(result.Words),
		"language":    language,
		"model":       result.Model,
		"source":      sourceKind(ct),
	})

	return c.JSON(buildDGPreRecordedResponse(result, requestID, language, diarize, wantUtterances, detectLanguage))
}

// fetchURL downloads audio from a remote URL (Deepgram's {"url": …} mode).
// Scheme-restricted to http/https with a hard size cap.
func (h *DeepgramPreRecordedHandler) fetchURL(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported url scheme %q (http/https only)", u.Scheme)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.fetcher.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("url returned HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, dgPreRecordedMaxBytes+1))
}

// buildDGPreRecordedResponse converts a sidecar transcript into Deepgram's
// ListenV1Response shape.
func buildDGPreRecordedResponse(result *sidecar.TranscribeResponse, requestID, language string, diarized, wantUtterances, detectLanguage bool) dgPreRecordedResponse {
	modelUUID := uuid.New().String()
	modelName := result.Model
	if modelName == "" {
		modelName = "parakeet-tdt-v3-coreml"
	}

	words := make([]dgPreRecordedWord, 0, len(result.Words))
	for _, w := range result.Words {
		words = append(words, dgPreRecordedWord{
			Word:           w.Word,
			Start:          w.Start,
			End:            w.End,
			Confidence:     0.99,
			PunctuatedWord: w.Word,
			Speaker:        dgSpeakerID(w.Speaker),
		})
	}

	channel := dgPreRecordedChannel{
		Alternatives: []dgPreRecordedAlternative{
			{
				Transcript: result.Text,
				Confidence: 0.99,
				Words:      words,
			},
		},
	}
	if detectLanguage {
		// Parakeet TDT v3 is multilingual but the sidecar does not run
		// language ID — echo the requested hint (Deepgram clients treat
		// detected_language as informational).
		channel.DetectedLanguage = language
	}

	resp := dgPreRecordedResponse{
		Metadata: dgPreRecordedMetadata{
			TransactionKey: "deprecated",
			RequestID:      requestID,
			SHA256:         fmt.Sprintf("%x", sha256.Sum256([]byte(requestID))),
			Created:        time.Now().UTC().Format(time.RFC3339),
			Duration:       result.Duration,
			Channels:       1,
			Models:         []string{modelUUID},
			ModelInfo: map[string]map[string]string{
				modelUUID: {
					"name":    modelName,
					"version": "2026-03-01",
					"arch":    "parakeet-tdt",
				},
			},
		},
		Results: dgPreRecordedResults{
			Channels: []dgPreRecordedChannel{channel},
		},
	}

	// Utterances: built from the sidecar's pause-segmented transcript
	// segments; words are those whose midpoint falls in the segment.
	if wantUtterances {
		for i, seg := range result.Segments {
			utt := dgPreRecordedUtterance{
				Start:      seg.Start,
				End:        seg.End,
				Confidence: 0.99,
				Channel:    0,
				Transcript: strings.TrimSpace(seg.Text),
				ID:         fmt.Sprintf("utt-%03d", i+1),
				Speaker:    dgSpeakerID(seg.Speaker),
			}
			for _, w := range words {
				mid := (w.Start + w.End) / 2
				if mid >= seg.Start && mid < seg.End {
					utt.Words = append(utt.Words, w)
				}
			}
			if utt.Words == nil {
				utt.Words = []dgPreRecordedWord{}
			}
			resp.Results.Utterances = append(resp.Results.Utterances, utt)
		}
		if resp.Results.Utterances == nil {
			resp.Results.Utterances = []dgPreRecordedUtterance{}
		}
	}

	return resp
}

// dgSpeakerID converts the sidecar's string speaker label into Deepgram's
// integer speaker ID. The batch route emits "SPEAKER_00"-style labels
// (Sortformer), the streaming route emits bare indices — handle both,
// any case, with or without padding. nil when there is no label
// (diarization off).
func dgSpeakerID(label string) *int {
	if label == "" {
		return nil
	}
	s := label
	if i := strings.LastIndexByte(s, '_'); i >= 0 {
		s = s[i+1:]
	}
	id := 0
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		id = n
	}
	return &id
}

// filenameForContentType picks a cosmetic filename for the sidecar
// multipart upload (the sidecar sniffs the container format itself).
func filenameForContentType(ct string) string {
	switch {
	case strings.Contains(ct, "wav"):
		return "audio.wav"
	case strings.Contains(ct, "mpeg") || strings.Contains(ct, "mp3"):
		return "audio.mp3"
	case strings.Contains(ct, "flac"):
		return "audio.flac"
	case strings.Contains(ct, "ogg"):
		return "audio.ogg"
	case strings.Contains(ct, "webm"):
		return "audio.webm"
	case strings.Contains(ct, "mp4") || strings.Contains(ct, "m4a"):
		return "audio.m4a"
	default:
		return "audio.bin"
	}
}

func sourceKind(ct string) string {
	if strings.HasPrefix(ct, "application/json") {
		return "url"
	}
	return "raw"
}

func urlHost(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return u.Host
	}
	return ""
}
