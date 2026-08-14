package sidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shaunagostinho/gotranscribesrv/internal/metrics"
)

// Client communicates with the audio inference sidecar.
// Audio sidecar → ASR, VAD, diarization, TTS (CoreML/ANE, port 8101)
// LLM sidecar   → chat, completions, embeddings, images (CoreML/ANE, port 8080)
type Client struct {
	audioURL   string // Audio sidecar (ASR, VAD, diarization, TTS)
	audioWSURL string
	llmURL     string // LLM sidecar (chat, completions, embeddings, images)
	httpClient *http.Client
	// streamClient has no global timeout — used for SSE streaming
	// responses (LLM chat/messages). Cancellation comes from the
	// request context instead.
	streamClient *http.Client
}

// NewClient creates a new sidecar client.
// audioURL/audioWSURL = Audio sidecar (ASR, VAD, diarization, TTS — CoreML/ANE)
// llmURL              = LLM sidecar (chat, completions, embeddings, images — CoreML/ANE)
func NewClient(audioURL, audioWSURL, llmURL string) *Client {
	return &Client{
		audioURL:   audioURL,
		audioWSURL: audioWSURL,
		llmURL:     llmURL,
		httpClient: &http.Client{
			Timeout: 5 * time.Minute, // Long timeout for large audio files
		},
		streamClient: &http.Client{}, // No timeout — streaming responses
	}
}

// TranscribeRequest is sent to the audio sidecar for ASR.
type TranscribeRequest struct {
	Audio    []byte `json:"-"`
	Filename string `json:"filename"`
	Language string `json:"language"`
	Diarize  bool   `json:"diarize"`
	// ITN enables inverse text normalization in the audio sidecar.
	// nil = use server default (currently on), true = force on, false = force off.
	ITN *bool `json:"-"`
}

// TranscribeResponse is the JSON transcript from the sidecar.
type TranscribeResponse struct {
	Text          string                    `json:"text"`
	Segments      []Segment                 `json:"segments,omitempty"`
	Words         []Word                    `json:"words,omitempty"`
	Duration      float64                   `json:"duration"`
	ProcessTimeMs int                       `json:"processing_time_ms"`
	Model         string                    `json:"model"`
	Diarized      bool                      `json:"diarized"`
	NumSpeakers   int                       `json:"num_speakers,omitempty"`
	Speakers      map[string]SpeakerSummary `json:"speakers,omitempty"`
	ITNApplied    bool                      `json:"itn_applied"`
}

// SpeakerSummary holds per-speaker statistics from diarization.
type SpeakerSummary struct {
	SegmentCount  int     `json:"segment_count"`
	WordCount     int     `json:"word_count"`
	TotalDuration float64 `json:"total_duration_s"`
}

// Segment is a transcript segment with optional speaker label.
type Segment struct {
	Speaker string  `json:"speaker,omitempty"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Text    string  `json:"text"`
}

// Word is a word-level timestamp with optional speaker label.
type Word struct {
	Word    string  `json:"word"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Speaker string  `json:"speaker,omitempty"`
}

// SynthesizeRequest is sent to the audio sidecar for TTS.
type SynthesizeRequest struct {
	Text      string  `json:"text"`
	Voice     string  `json:"voice"`
	VoiceRef  string  `json:"voice_ref,omitempty"`  // Base64 raw audio for one-shot cloning
	VoiceData string  `json:"voice_data,omitempty"` // Base64 pre-extracted embedding for stored voices
	Speed     float64 `json:"speed"`
	Format    string  `json:"format"`
}

// VadResponse is the JSON result from the audio sidecar VAD endpoint.
type VadResponse struct {
	SpeechSegments []VadSegment `json:"speech_segments"`
	Duration       float64      `json:"duration"`
	ProcessTimeMs  int          `json:"processing_time_ms"`
}

// VadSegment represents a detected speech region with start/end times in seconds.
type VadSegment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// HealthResponse from the sidecar health check.
type HealthResponse struct {
	Status string            `json:"status"`
	Models map[string]string `json:"models"`
}

// Health checks the audio sidecar health endpoint.
func (c *Client) Health() (*HealthResponse, error) {
	merged := &HealthResponse{
		Status: "ok",
		Models: make(map[string]string),
	}

	// Check audio sidecar (ASR, VAD, diarization, TTS)
	audioResp, err := c.httpClient.Get(c.audioURL + "/health")
	if err != nil {
		merged.Models["asr"] = "disconnected"
		merged.Models["vad"] = "disconnected"
		merged.Models["diarizer"] = "disconnected"
		merged.Models["tts"] = "disconnected"
	} else {
		defer audioResp.Body.Close()
		var audioHealth HealthResponse
		if err := json.NewDecoder(audioResp.Body).Decode(&audioHealth); err == nil {
			for k, v := range audioHealth.Models {
				merged.Models[k] = v
			}
		}
	}

	// Check LLM sidecar (chat, completions, embeddings, images)
	if c.llmURL != "" {
		llmResp, err := c.httpClient.Get(c.llmURL + "/health")
		if err != nil {
			merged.Models["llm"] = "disconnected"
		} else {
			defer llmResp.Body.Close()
			if llmResp.StatusCode == http.StatusOK {
				merged.Models["llm"] = "ok"
			} else {
				merged.Models["llm"] = "error"
			}
		}
	}

	return merged, nil
}

// Transcribe sends audio to the audio sidecar for transcription.
// If req.Diarize is true, the audio sidecar handles diarization inline
// (single-hop — no separate Python call needed). ctx is used for log
// correlation (request_id injection); pass the handler's request ctx.
func (c *Client) Transcribe(ctx context.Context, req TranscribeRequest) (*TranscribeResponse, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	part, err := writer.CreateFormFile("audio", req.Filename)
	if err != nil {
		return nil, fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(req.Audio); err != nil {
		return nil, fmt.Errorf("write audio data: %w", err)
	}

	_ = writer.WriteField("language", req.Language)
	if req.Diarize {
		_ = writer.WriteField("diarize", "true")
	}
	if req.ITN != nil {
		if *req.ITN {
			_ = writer.WriteField("itn", "true")
		} else {
			_ = writer.WriteField("itn", "false")
		}
	}
	writer.Close()

	httpReq, err := http.NewRequest("POST", c.audioURL+"/transcribe", &buf)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())

	slog.DebugContext(ctx, "sending transcription request to audio sidecar",
		"filename", req.Filename, "size", len(req.Audio), "diarize", req.Diarize)

	start := time.Now()
	resp, err := c.httpClient.Do(httpReq)
	durationMs := int(time.Since(start).Milliseconds())
	metrics.RecordSidecarLatency("swift", "transcribe", durationMs, err)
	if err != nil {
		return nil, fmt.Errorf("audio sidecar request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("audio sidecar returned %d: %s", resp.StatusCode, string(body))
	}

	var result TranscribeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode transcript: %w", err)
	}

	return &result, nil
}

// VAD sends audio to the audio sidecar for voice activity detection.
// Returns speech segment boundaries (start/end times in seconds).
// ctx is used for log correlation (request_id injection).
func (c *Client) VAD(ctx context.Context, audio []byte, filename string) (*VadResponse, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	part, err := writer.CreateFormFile("audio", filename)
	if err != nil {
		return nil, fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(audio); err != nil {
		return nil, fmt.Errorf("write audio data: %w", err)
	}
	writer.Close()

	httpReq, err := http.NewRequest("POST", c.audioURL+"/vad", &buf)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())

	slog.DebugContext(ctx, "sending VAD request to audio sidecar",
		"filename", filename, "size", len(audio))

	start := time.Now()
	resp, err := c.httpClient.Do(httpReq)
	durationMs := int(time.Since(start).Milliseconds())
	metrics.RecordSidecarLatency("swift", "vad", durationMs, err)
	if err != nil {
		return nil, fmt.Errorf("audio sidecar VAD request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("audio sidecar VAD returned %d: %s", resp.StatusCode, string(body))
	}

	var result VadResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode VAD response: %w", err)
	}

	return &result, nil
}

// Synthesize sends text to the audio sidecar for TTS and returns raw audio bytes.
// ctx is used for log correlation (request_id injection).
func (c *Client) Synthesize(ctx context.Context, req SynthesizeRequest) ([]byte, string, error) {
	audio, ct, _, err := c.SynthesizeWithBackend(ctx, req, "")
	return audio, ct, err
}

// SidecarError is a non-200 HTTP response from a sidecar. Callers can use
// errors.As to distinguish client-fixable rejections (4xx, e.g. unknown
// voice) from genuine sidecar outages (5xx / transport errors).
type SidecarError struct {
	StatusCode int
	Reason     string // parsed from Vapor's {"error":true,"reason":"..."} body when possible
}

func (e *SidecarError) Error() string {
	return fmt.Sprintf("sidecar returned %d: %s", e.StatusCode, e.Reason)
}

// newSidecarError builds a SidecarError from a non-200 response body,
// extracting Vapor's "reason" field when present.
func newSidecarError(statusCode int, body []byte) *SidecarError {
	reason := strings.TrimSpace(string(body))
	var vaporErr struct {
		Error  bool   `json:"error"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal(body, &vaporErr) == nil && vaporErr.Reason != "" {
		reason = vaporErr.Reason
	}
	return &SidecarError{StatusCode: statusCode, Reason: reason}
}

// SynthesizeWithBackend is like Synthesize but routes to a specific TTS
// backend on the sidecar via ?backend= query param. Pass backend="" to use
// the sidecar default (pocket). Known values: "pocket", "kokoro".
func (c *Client) SynthesizeWithBackend(ctx context.Context, req SynthesizeRequest, backend string) ([]byte, string, string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, "", "", fmt.Errorf("marshal request: %w", err)
	}

	endpoint := c.audioURL + "/synthesize"
	if backend != "" {
		endpoint += "?backend=" + url.QueryEscape(backend)
	}

	httpReq, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", "", fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := c.httpClient.Do(httpReq)
	durationMs := int(time.Since(start).Milliseconds())
	metrics.RecordSidecarLatency("swift", "synthesize", durationMs, err)
	if err != nil {
		return nil, "", "", fmt.Errorf("sidecar request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, "", "", newSidecarError(resp.StatusCode, errBody)
	}

	audio, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", "", fmt.Errorf("read audio response: %w", err)
	}

	contentType := resp.Header.Get("Content-Type")
	backendUsed := resp.Header.Get("X-TTS-Backend")
	return audio, contentType, backendUsed, nil
}

// SynthesizeStream posts text to the audio sidecar's POST /synthesize/stream
// endpoint (PocketTTS only) and returns the live response body: raw Int16
// L16 24 kHz mono frames delivered via chunked transfer encoding as they're
// generated (~80 ms per frame). Uses streamClient (no global timeout) —
// cancel ctx to abort mid-stream (barge-in). Caller must Close the body.
// Used by the realtime speech-to-speech orchestrator (docs/realtime.md).
// requestID is sent as X-Request-ID so sidecar logs correlate with the
// orchestrator's turn (empty = no header).
func (c *Client) SynthesizeStream(ctx context.Context, text, voice, requestID string) (io.ReadCloser, error) {
	body, err := json.Marshal(map[string]string{"text": text, "voice": voice})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.audioURL+"/synthesize/stream", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if requestID != "" {
		httpReq.Header.Set("X-Request-ID", requestID)
	}

	start := time.Now()
	resp, err := c.streamClient.Do(httpReq)
	durationMs := int(time.Since(start).Milliseconds())
	metrics.RecordSidecarLatency("swift", "synthesize_stream", durationMs, err)
	if err != nil {
		return nil, fmt.Errorf("sidecar streaming TTS request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, newSidecarError(resp.StatusCode, errBody)
	}
	return resp.Body, nil
}

// CloneVoice sends audio to the audio sidecar to extract a voice embedding.
// Returns the raw embedding bytes and the audio duration in milliseconds.
// ctx is used for log correlation (request_id injection).
func (c *Client) CloneVoice(ctx context.Context, audio []byte, filename string) ([]byte, int, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	part, err := writer.CreateFormFile("audio", filename)
	if err != nil {
		return nil, 0, fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(audio); err != nil {
		return nil, 0, fmt.Errorf("write audio data: %w", err)
	}
	writer.Close()

	httpReq, err := http.NewRequest("POST", c.audioURL+"/clone-voice", &buf)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())

	slog.DebugContext(ctx, "sending clone-voice request to audio sidecar",
		"filename", filename, "size", len(audio))

	start := time.Now()
	resp, err := c.httpClient.Do(httpReq)
	durationMs := int(time.Since(start).Milliseconds())
	metrics.RecordSidecarLatency("swift", "clone_voice", durationMs, err)
	if err != nil {
		return nil, 0, fmt.Errorf("sidecar clone-voice request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, 0, fmt.Errorf("sidecar clone-voice returned %d: %s", resp.StatusCode, string(errBody))
	}

	embedding, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("read embedding response: %w", err)
	}

	// Read actual audio duration from sidecar response header
	audioDurationMs := 0
	if durStr := resp.Header.Get("X-Audio-Duration-Ms"); durStr != "" {
		fmt.Sscanf(durStr, "%d", &audioDurationMs)
	}

	return embedding, audioDurationMs, nil
}

// StreamURL returns the WebSocket URL for streaming ASR on the audio sidecar.
func (c *Client) StreamURL() string {
	return c.audioWSURL + "/stream"
}

// RealtimeStreamURL returns the WebSocket URL for true real-time streaming
// ASR on the audio sidecar (uses Parakeet EOU / Nemotron / Parakeet Unified
// cache-aware streaming + streaming Silero VAD).
func (c *Client) RealtimeStreamURL(engine string) string {
	u := c.audioWSURL + "/stream/realtime"
	if engine != "" {
		u += "?engine=" + url.QueryEscape(engine)
	}
	return u
}

// DeepgramRealtimeURL returns the WebSocket URL for Deepgram-compatible
// realtime streaming ASR on the audio sidecar.
func (c *Client) DeepgramRealtimeURL(engine string) string {
	u := c.audioWSURL + "/stream/realtime"
	if engine != "" {
		u += "?engine=" + url.QueryEscape(engine)
	}
	return u
}

// VoiceInfo describes a single TTS voice.
type VoiceInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"` // "system" or "custom"
}

// VoicesResponse is the JSON list of available TTS voices.
type VoicesResponse struct {
	Voices []VoiceInfo `json:"voices"`
}

// ListVoices fetches available TTS voice presets from the audio sidecar.
func (c *Client) ListVoices() (*VoicesResponse, error) {
	resp, err := c.httpClient.Get(c.audioURL + "/voices")
	if err != nil {
		return nil, fmt.Errorf("sidecar voices request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("sidecar returned %d: %s", resp.StatusCode, string(body))
	}

	var result VoicesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode voices response: %w", err)
	}
	return &result, nil
}
