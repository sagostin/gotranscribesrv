package handlers

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
)

// WhisperHandler handles the OpenAI Whisper-compatible endpoint.
type WhisperHandler struct {
	sidecar *sidecar.Client
}

// NewWhisperHandler creates a new WhisperHandler.
func NewWhisperHandler(sc *sidecar.Client) *WhisperHandler {
	return &WhisperHandler{sidecar: sc}
}

// Transcriptions handles the Whisper-compatible endpoint.
// POST /v1/audio/transcriptions
//
// Supports three response object types per the OpenAI spec:
//   - Transcription (json)          → {text, usage}
//   - TranscriptionVerbose          → {duration, language, text, segments, words, usage}
//   - TranscriptionDiarized         → {task, duration, text, segments (with speaker), usage}
func (h *WhisperHandler) Transcriptions(c *fiber.Ctx) error {
	file, err := c.FormFile("file")
	if err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "MISSING_FILE",
				"message": "Audio file is required (field: 'file')",
				"status":  422,
			},
		})
	}

	// 25MB limit per Whisper spec
	if file.Size > 25*1024*1024 {
		return c.Status(fiber.StatusRequestEntityTooLarge).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "FILE_TOO_LARGE",
				"message": "Audio file must be less than 25MB",
				"status":  413,
			},
		})
	}

	f, err := file.Open()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "FILE_READ_ERROR",
				"message": "Failed to read audio file",
				"status":  500,
			},
		})
	}
	defer f.Close()

	audioBytes, err := io.ReadAll(f)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "FILE_READ_ERROR",
				"message": "Failed to read audio file",
				"status":  500,
			},
		})
	}

	// ── Parse all Whisper form fields ─────────────────────────────────
	language := c.FormValue("language", "en")
	stream := c.FormValue("stream") == "true"
	responseFormat := c.FormValue("response_format", "json")
	model := c.FormValue("model")
	temperature := c.FormValue("temperature")
	prompt := c.FormValue("prompt")

	// timestamp_granularities[] — multi-value form field
	var timestampGranularities []string
	if form, err := c.MultipartForm(); err == nil {
		if tg, ok := form.Value["timestamp_granularities[]"]; ok {
			timestampGranularities = tg
		} else if tg, ok := form.Value["timestamp_granularities"]; ok {
			timestampGranularities = tg
		}
	}

	// Determine what to include based on timestamp_granularities
	includeWords := true
	includeSegments := true
	if len(timestampGranularities) > 0 {
		hasWord := false
		hasSegment := false
		for _, g := range timestampGranularities {
			switch strings.TrimSpace(strings.ToLower(g)) {
			case "word":
				hasWord = true
			case "segment":
				hasSegment = true
			}
		}
		if hasWord || hasSegment {
			includeWords = hasWord
			includeSegments = hasSegment
		}
	}

	// Determine if diarization is requested:
	//   - response_format=diarized_json (explicit)
	//   - model contains "diarize" (e.g. "gpt-4o-transcribe-diarize")
	wantDiarize := responseFormat == "diarized_json" ||
		strings.Contains(strings.ToLower(model), "diarize")

	// ── Verbose request logging ──────────────────────────────────────
	slog.Info("[whisper] incoming request",
		"file", fmt.Sprintf("%s (%d bytes)", file.Filename, file.Size),
		"model", model,
		"lang", language,
		"format", responseFormat,
		"stream", stream,
		"granularities", strings.Join(timestampGranularities, ","),
		"diarize", wantDiarize,
		"prompt", prompt,
		"temperature", temperature,
		"ip", c.IP(),
	)

	// ── Send transcription + optional VAD in parallel ────────────────
	var (
		result    *sidecar.TranscribeResponse
		vadResult *sidecar.VadResponse
		asrErr    error
		vadErr    error
	)

	wantVAD := responseFormat == "verbose_json"

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		result, asrErr = h.sidecar.Transcribe(sidecar.TranscribeRequest{
			Audio:    audioBytes,
			Filename: file.Filename,
			Language: language,
			Diarize:  wantDiarize,
		})
	}()

	if wantVAD {
		wg.Add(1)
		go func() {
			defer wg.Done()
			vadResult, vadErr = h.sidecar.VAD(audioBytes, file.Filename)
			if vadErr != nil {
				slog.Warn("[whisper] VAD failed, continuing without",
					"error", vadErr)
			}
		}()
	}

	wg.Wait()

	if asrErr != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "SIDECAR_ERROR",
				"message": "Transcription service unavailable",
				"status":  502,
			},
		})
	}

	// Store metadata for usage tracking
	c.Locals("audio_duration_ms", int(result.Duration*1000))
	c.Locals("diarized", result.Diarized)
	c.Locals("usage_meta", map[string]interface{}{
		"file_size_bytes": file.Size,
		"filename":        file.Filename,
		"word_count":      len(result.Words),
		"segment_count":   len(result.Segments),
		"language":        language,
		"model":           model,
		"response_format": responseFormat,
		"num_speakers":    result.NumSpeakers,
	})

	// If VAD detected multiple speech regions but sidecar returned just 1-2 segments,
	// split the transcript using VAD boundaries + word timestamps.
	if vadResult != nil && len(vadResult.SpeechSegments) > 1 && len(result.Segments) <= 2 && len(result.Words) > 0 {
		split := splitSegmentsByVAD(result.Words, vadResult.SpeechSegments)
		if len(split) > len(result.Segments) {
			slog.Info("[whisper] split segments using VAD boundaries",
				"original_segments", len(result.Segments),
				"vad_segments", len(vadResult.SpeechSegments),
				"new_segments", len(split),
			)
			result.Segments = split
		}
	}

	// Concise completion summary
	slog.Info("[whisper] transcription complete",
		"duration_s", fmt.Sprintf("%.2f", result.Duration),
		"segments", len(result.Segments),
		"words", len(result.Words),
		"diarized", result.Diarized,
		"speakers", result.NumSpeakers,
		"asr_ms", result.ProcessTimeMs,
	)

	// SSE streaming mode (OpenAI-compatible)
	if stream {
		return streamWhisperResponse(c, result)
	}

	// Format response based on response_format
	return formatWhisperResponse(c, result, responseFormat, language,
		includeWords, includeSegments, vadResult)
}

// ── Usage helpers ────────────────────────────────────────────────────

// whisperDurationUsage returns the OpenAI-compatible "usage" object
// for models billed by audio duration.
func whisperDurationUsage(durationSec float64) fiber.Map {
	return fiber.Map{
		"type":    "duration",
		"seconds": int(math.Ceil(durationSec)),
	}
}

// ── SSE Streaming ────────────────────────────────────────────────────

// sseEvent represents a single Server-Sent Event payload.
type sseEvent struct {
	Type     string         `json:"type"`
	Delta    string         `json:"delta,omitempty"`
	Text     string         `json:"text,omitempty"`
	Duration float64        `json:"duration,omitempty"`
	Words    []sidecar.Word `json:"words,omitempty"`
	Logprobs interface{}    `json:"logprobs"`
}

// streamWhisperResponse writes the transcript as OpenAI-compatible SSE events.
func streamWhisperResponse(c *fiber.Ctx, result *sidecar.TranscribeResponse) error {
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		// Emit delta events — one per segment for incremental delivery
		if len(result.Segments) > 0 {
			for _, seg := range result.Segments {
				delta := sseEvent{
					Type:     "transcript.text.delta",
					Delta:    seg.Text,
					Logprobs: nil,
				}
				writeSSE(w, "transcript.text.delta", delta)
				w.Flush()
				time.Sleep(5 * time.Millisecond)
			}
		} else if result.Text != "" {
			delta := sseEvent{
				Type:     "transcript.text.delta",
				Delta:    result.Text,
				Logprobs: nil,
			}
			writeSSE(w, "transcript.text.delta", delta)
			w.Flush()
		}

		// Emit done event with full transcript + usage
		donePayload := fiber.Map{
			"type":     "transcript.text.done",
			"text":     result.Text,
			"logprobs": nil,
			"usage":    whisperDurationUsage(result.Duration),
		}
		doneJSON, _ := json.Marshal(donePayload)
		fmt.Fprintf(w, "event: transcript.text.done\ndata: %s\n\n", string(doneJSON))
		w.Flush()

		// Terminal sentinel (OpenAI convention)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		w.Flush()
	})

	return nil
}

// writeSSE writes a single SSE event to the writer.
func writeSSE(w *bufio.Writer, event string, data interface{}) {
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonBytes))
}

// ── Response Types ───────────────────────────────────────────────────

// verboseSegment matches the OpenAI TranscriptionSegment schema.
type verboseSegment struct {
	ID               int     `json:"id"`
	Seek             int     `json:"seek"`
	Start            float64 `json:"start"`
	End              float64 `json:"end"`
	Text             string  `json:"text"`
	Tokens           []int   `json:"tokens"`
	Temperature      float64 `json:"temperature"`
	AvgLogprob       float64 `json:"avg_logprob"`
	CompressionRatio float64 `json:"compression_ratio"`
	NoSpeechProb     float64 `json:"no_speech_prob"`
}

// diarizedSegment matches the OpenAI TranscriptionDiarizedSegment schema.
type diarizedSegment struct {
	Type    string  `json:"type"`
	ID      string  `json:"id"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Text    string  `json:"text"`
	Speaker string  `json:"speaker"`
}

// formatWhisperResponse formats the transcript in the requested Whisper format.
func formatWhisperResponse(
	c *fiber.Ctx,
	result *sidecar.TranscribeResponse,
	format string,
	language string,
	includeWords bool,
	includeSegments bool,
	vadResult *sidecar.VadResponse,
) error {
	usage := whisperDurationUsage(result.Duration)

	switch format {
	case "text":
		c.Set("Content-Type", "text/plain")
		return c.SendString(result.Text)

	case "srt":
		c.Set("Content-Type", "text/plain")
		return c.SendString(toSRT(result))

	case "vtt":
		c.Set("Content-Type", "text/vtt")
		return c.SendString(toVTT(result))

	case "diarized_json":
		// OpenAI TranscriptionDiarized schema
		resp := fiber.Map{
			"task":     "transcribe",
			"duration": result.Duration,
			"text":     result.Text,
			"usage":    usage,
		}

		// Build diarized segments with speaker labels
		segments := make([]diarizedSegment, 0, len(result.Segments))
		for i, seg := range result.Segments {
			speaker := seg.Speaker
			if speaker == "" {
				// Default labeling: A, B, C, ...
				speaker = string(rune('A' + i%26))
			}
			segments = append(segments, diarizedSegment{
				Type:    "transcript.text.segment",
				ID:      fmt.Sprintf("seg_%03d", i+1),
				Start:   seg.Start,
				End:     seg.End,
				Text:    seg.Text,
				Speaker: speaker,
			})
		}
		resp["segments"] = segments

		slog.Info("[whisper] diarized response",
			"segments", len(segments),
			"speakers", result.NumSpeakers,
			"duration_s", fmt.Sprintf("%.1f", result.Duration),
		)

		return c.JSON(resp)

	case "verbose_json":
		// OpenAI TranscriptionVerbose schema
		resp := fiber.Map{
			"task":     "transcribe",
			"language": language,
			"duration": result.Duration,
			"text":     result.Text,
			"usage":    usage,
		}

		// Build enriched segments with Whisper-spec fields
		if includeSegments && len(result.Segments) > 0 {
			enriched := make([]verboseSegment, len(result.Segments))
			for i, seg := range result.Segments {
				noSpeechProb := 0.0
				if vadResult != nil {
					noSpeechProb = estimateNoSpeechProb(seg.Start, seg.End, vadResult.SpeechSegments)
				}
				enriched[i] = verboseSegment{
					ID:               i,
					Seek:             int(seg.Start * 100),
					Start:            seg.Start,
					End:              seg.End,
					Text:             seg.Text,
					Tokens:           []int{}, // Parakeet TDT doesn't emit token IDs
					Temperature:      0.0,
					AvgLogprob:       -0.15,
					CompressionRatio: 1.2,
					NoSpeechProb:     noSpeechProb,
				}
			}
			resp["segments"] = enriched
		}

		if includeWords {
			resp["words"] = result.Words
		}

		// Include VAD speech segments when available
		if vadResult != nil && len(vadResult.SpeechSegments) > 0 {
			resp["vad_segments"] = vadResult.SpeechSegments
			resp["vad_processing_time_ms"] = vadResult.ProcessTimeMs
		}

		return c.JSON(resp)

	default: // "json"
		// OpenAI Transcription schema (minimal)
		return c.JSON(fiber.Map{
			"text":  result.Text,
			"usage": usage,
		})
	}
}

// splitSegmentsByVAD breaks a single large transcript into multiple segments
// using VAD speech boundaries. Each word is assigned to the VAD segment whose
// time range it falls within (by word midpoint), then consecutive words in the
// same VAD segment are grouped into transcript segments.
func splitSegmentsByVAD(words []sidecar.Word, vadSegs []sidecar.VadSegment) []sidecar.Segment {
	if len(words) == 0 || len(vadSegs) == 0 {
		return nil
	}

	// Assign each word to a VAD segment index (-1 = silence gap)
	type assignedWord struct {
		Word   sidecar.Word
		VadIdx int
	}

	assigned := make([]assignedWord, len(words))
	for i, w := range words {
		wordMid := (w.Start + w.End) / 2
		bestIdx := -1
		for j, vs := range vadSegs {
			if wordMid >= vs.Start && wordMid <= vs.End {
				bestIdx = j
				break
			}
		}
		// If word doesn't fall in any VAD segment, assign to nearest
		if bestIdx == -1 {
			bestDist := float64(999999)
			for j, vs := range vadSegs {
				segMid := (vs.Start + vs.End) / 2
				dist := wordMid - segMid
				if dist < 0 {
					dist = -dist
				}
				if dist < bestDist {
					bestDist = dist
					bestIdx = j
				}
			}
		}
		assigned[i] = assignedWord{Word: w, VadIdx: bestIdx}
	}

	// Group consecutive words with the same VAD index into segments
	var segments []sidecar.Segment
	var currentWords []sidecar.Word
	currentIdx := assigned[0].VadIdx

	for _, aw := range assigned {
		if aw.VadIdx != currentIdx && len(currentWords) > 0 {
			// Flush segment
			text := ""
			for k, cw := range currentWords {
				if k > 0 {
					text += " "
				}
				text += cw.Word
			}
			segments = append(segments, sidecar.Segment{
				Start: currentWords[0].Start,
				End:   currentWords[len(currentWords)-1].End,
				Text:  text,
			})
			currentWords = nil
		}
		currentIdx = aw.VadIdx
		currentWords = append(currentWords, aw.Word)
	}

	// Flush final segment
	if len(currentWords) > 0 {
		text := ""
		for k, cw := range currentWords {
			if k > 0 {
				text += " "
			}
			text += cw.Word
		}
		segments = append(segments, sidecar.Segment{
			Start: currentWords[0].Start,
			End:   currentWords[len(currentWords)-1].End,
			Text:  text,
		})
	}

	return segments
}

// estimateNoSpeechProb calculates how much of a transcript segment overlaps
// with detected silence (non-speech). Returns 0.0 for fully-speech segments
// and approaches 1.0 for segments that fall entirely in silence.
func estimateNoSpeechProb(segStart, segEnd float64, speechSegments []sidecar.VadSegment) float64 {
	segDuration := segEnd - segStart
	if segDuration <= 0 {
		return 0.0
	}

	var speechOverlap float64
	for _, vs := range speechSegments {
		overlapStart := max(segStart, vs.Start)
		overlapEnd := min(segEnd, vs.End)
		if overlapEnd > overlapStart {
			speechOverlap += overlapEnd - overlapStart
		}
	}

	speechRatio := speechOverlap / segDuration
	if speechRatio > 1.0 {
		speechRatio = 1.0
	}
	return 1.0 - speechRatio
}

// ── Subtitle formatters ─────────────────────────────────────────────

func toSRT(result *sidecar.TranscribeResponse) string {
	var srt string
	for i, seg := range result.Segments {
		srt += fmt.Sprintf("%d\n%s --> %s\n%s\n\n",
			i+1,
			formatTimeSRT(seg.Start),
			formatTimeSRT(seg.End),
			seg.Text,
		)
	}
	return srt
}

func toVTT(result *sidecar.TranscribeResponse) string {
	vtt := "WEBVTT\n\n"
	for _, seg := range result.Segments {
		vtt += fmt.Sprintf("%s --> %s\n%s\n\n",
			formatTimeVTT(seg.Start),
			formatTimeVTT(seg.End),
			seg.Text,
		)
	}
	return vtt
}

func formatTimeSRT(seconds float64) string {
	h := int(seconds) / 3600
	m := (int(seconds) % 3600) / 60
	s := int(seconds) % 60
	ms := int((seconds - float64(int(seconds))) * 1000)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}

func formatTimeVTT(seconds float64) string {
	h := int(seconds) / 3600
	m := (int(seconds) % 3600) / 60
	s := int(seconds) % 60
	ms := int((seconds - float64(int(seconds))) * 1000)
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
}
