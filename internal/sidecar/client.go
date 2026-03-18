package sidecar

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"time"
)

// Client communicates with the inference sidecars.
// Swift sidecar → ASR, VAD, diarization, TTS (CoreML/ANE, port 8101)
// Python sidecar → LLM processing only (MLX, port 8100)
type Client struct {
	swiftURL   string // Swift sidecar (ASR, VAD, diarization, TTS)
	swiftWSURL string
	llmURL     string // Python sidecar (LLM only)
	httpClient *http.Client
}

// NewClient creates a new dual-sidecar client.
// swiftURL/swiftWSURL = Swift sidecar (ASR, VAD, diarization, TTS — CoreML/ANE)
// llmURL = Python sidecar (LLM processing — MLX)
func NewClient(swiftURL, swiftWSURL, llmURL string) *Client {
	return &Client{
		swiftURL:   swiftURL,
		swiftWSURL: swiftWSURL,
		llmURL:     llmURL,
		httpClient: &http.Client{
			Timeout: 5 * time.Minute, // Long timeout for large audio files
		},
	}
}

// TranscribeRequest is sent to the Swift sidecar for ASR.
type TranscribeRequest struct {
	Audio    []byte `json:"-"`
	Filename string `json:"filename"`
	Language string `json:"language"`
	Diarize  bool   `json:"diarize"`
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

// SynthesizeRequest is sent to the Swift sidecar for TTS.
type SynthesizeRequest struct {
	Text     string  `json:"text"`
	Voice    string  `json:"voice"`
	VoiceRef string  `json:"voice_ref,omitempty"`
	Speed    float64 `json:"speed"`
	Format   string  `json:"format"`
}

// HealthResponse from the sidecar health check.
type HealthResponse struct {
	Status string            `json:"status"`
	Models map[string]string `json:"models"`
}

// Health checks both sidecar health endpoints and merges results.
func (c *Client) Health() (*HealthResponse, error) {
	merged := &HealthResponse{
		Status: "ok",
		Models: make(map[string]string),
	}

	// Check Swift sidecar (ASR, VAD, diarization, TTS)
	swiftResp, err := c.httpClient.Get(c.swiftURL + "/health")
	if err != nil {
		merged.Models["asr"] = "disconnected"
		merged.Models["vad"] = "disconnected"
		merged.Models["diarizer"] = "disconnected"
		merged.Models["tts"] = "disconnected"
	} else {
		defer swiftResp.Body.Close()
		var swiftHealth HealthResponse
		if err := json.NewDecoder(swiftResp.Body).Decode(&swiftHealth); err == nil {
			for k, v := range swiftHealth.Models {
				merged.Models[k] = v
			}
		}
	}

	// Check Python sidecar (LLM only)
	pyResp, err := c.httpClient.Get(c.llmURL + "/health")
	if err != nil {
		merged.Models["llm"] = "disconnected"
	} else {
		defer pyResp.Body.Close()
		var pyHealth HealthResponse
		if err := json.NewDecoder(pyResp.Body).Decode(&pyHealth); err == nil {
			for k, v := range pyHealth.Models {
				merged.Models[k] = v
			}
		}
	}

	return merged, nil
}

// Transcribe sends audio to the Swift sidecar for transcription.
// If req.Diarize is true, the Swift sidecar handles diarization inline
// (single-hop — no separate Python call needed).
func (c *Client) Transcribe(req TranscribeRequest) (*TranscribeResponse, error) {
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
	writer.Close()

	httpReq, err := http.NewRequest("POST", c.swiftURL+"/transcribe", &buf)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())

	slog.Debug("sending transcription request to Swift sidecar",
		"filename", req.Filename, "size", len(req.Audio), "diarize", req.Diarize)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Swift sidecar request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Swift sidecar returned %d: %s", resp.StatusCode, string(body))
	}

	var result TranscribeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode transcript: %w", err)
	}

	return &result, nil
}

// Diarize sends audio and a transcript to the Swift sidecar for speaker diarization.
func (c *Client) Diarize(audio []byte, filename string, transcript *TranscribeResponse) (*TranscribeResponse, error) {
	transcriptJSON, err := json.Marshal(transcript)
	if err != nil {
		return nil, fmt.Errorf("marshal transcript: %w", err)
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	part, err := writer.CreateFormFile("audio", filename)
	if err != nil {
		return nil, fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(audio); err != nil {
		return nil, fmt.Errorf("write audio data: %w", err)
	}

	_ = writer.WriteField("transcript", string(transcriptJSON))
	writer.Close()

	httpReq, err := http.NewRequest("POST", c.swiftURL+"/diarize", &buf)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())

	slog.Debug("sending diarization request to Swift sidecar")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("diarization sidecar request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("diarization sidecar returned %d: %s", resp.StatusCode, string(body))
	}

	var result TranscribeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode diarized transcript: %w", err)
	}
	return &result, nil
}

// Synthesize sends text to the Swift sidecar for TTS and returns raw audio bytes.
func (c *Client) Synthesize(req SynthesizeRequest) ([]byte, string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, "", fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", c.swiftURL+"/synthesize", bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, "", fmt.Errorf("sidecar request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("sidecar returned %d: %s", resp.StatusCode, string(errBody))
	}

	audio, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read audio response: %w", err)
	}

	contentType := resp.Header.Get("Content-Type")
	return audio, contentType, nil
}

// StreamURL returns the WebSocket URL for streaming ASR on the Swift sidecar.
func (c *Client) StreamURL() string {
	return c.swiftWSURL + "/stream"
}

// VoiceInfo describes a single TTS voice preset.
type VoiceInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// VoicesResponse is the JSON list of available TTS voices.
type VoicesResponse struct {
	Voices []VoiceInfo `json:"voices"`
}

// ListVoices fetches available TTS voice presets from the Swift sidecar.
func (c *Client) ListVoices() (*VoicesResponse, error) {
	resp, err := c.httpClient.Get(c.swiftURL + "/voices")
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

// ProcessRequest is sent to the Python sidecar for LLM transcript processing.
type ProcessRequest struct {
	TranscriptText string  `json:"transcript_text"`
	Task           string  `json:"task"`
	Language       string  `json:"language,omitempty"`
	Prompt         string  `json:"prompt,omitempty"`
	MaxTokens      int     `json:"max_tokens"`
	Temperature    float64 `json:"temperature"`
}

// ProcessResponse is the JSON result from LLM processing.
type ProcessResponse struct {
	Result          string `json:"result"`
	Task            string `json:"task"`
	Model           string `json:"model"`
	ProcessTimeMs   int    `json:"processing_time_ms"`
	TokensGenerated int    `json:"tokens_generated"`
}

// TasksResponse lists available LLM processing tasks.
type TasksResponse struct {
	Tasks        []string          `json:"tasks"`
	Descriptions map[string]string `json:"descriptions"`
}

// Process sends transcript text to the Python sidecar for LLM processing.
func (c *Client) Process(req ProcessRequest) (*ProcessResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", c.llmURL+"/process", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	slog.Debug("sending LLM process request to Python sidecar", "task", req.Task, "text_len", len(req.TranscriptText))

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sidecar request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("sidecar returned %d: %s", resp.StatusCode, string(errBody))
	}

	var result ProcessResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode process response: %w", err)
	}
	return &result, nil
}

// ListTasks fetches available LLM processing tasks from the Python sidecar.
func (c *Client) ListTasks() (*TasksResponse, error) {
	resp, err := c.httpClient.Get(c.llmURL + "/process/tasks")
	if err != nil {
		return nil, fmt.Errorf("sidecar tasks request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("sidecar returned %d: %s", resp.StatusCode, string(body))
	}

	var result TasksResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode tasks response: %w", err)
	}
	return &result, nil
}
