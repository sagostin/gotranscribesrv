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

// Client communicates with the Python inference sidecar.
type Client struct {
	baseURL    string
	wsURL      string
	httpClient *http.Client
}

// NewClient creates a new sidecar HTTP client.
func NewClient(baseURL, wsURL string) *Client {
	return &Client{
		baseURL: baseURL,
		wsURL:   wsURL,
		httpClient: &http.Client{
			Timeout: 5 * time.Minute, // Long timeout for large audio files
		},
	}
}

// TranscribeRequest is sent to the Python sidecar for file-based ASR.
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

// SynthesizeRequest is sent to the Python sidecar for TTS.
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

// Health checks the sidecar health endpoint.
func (c *Client) Health() (*HealthResponse, error) {
	resp, err := c.httpClient.Get(c.baseURL + "/health")
	if err != nil {
		return nil, fmt.Errorf("sidecar health check failed: %w", err)
	}
	defer resp.Body.Close()

	var health HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return nil, fmt.Errorf("decode health response: %w", err)
	}
	return &health, nil
}

// Transcribe sends audio to the sidecar for transcription.
func (c *Client) Transcribe(req TranscribeRequest) (*TranscribeResponse, error) {
	// Build multipart request
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

	httpReq, err := http.NewRequest("POST", c.baseURL+"/transcribe", &buf)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())

	slog.Debug("sending transcription request to sidecar", "filename", req.Filename, "size", len(req.Audio))

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sidecar request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("sidecar returned %d: %s", resp.StatusCode, string(body))
	}

	var result TranscribeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode transcript: %w", err)
	}
	return &result, nil
}

// DiarizeResponse is the JSON speaker-detection result from the sidecar.
type DiarizeResponse struct {
	Speakers      map[string][]SpeakerSegment `json:"speakers"`
	NumSpeakers   int                         `json:"num_speakers"`
	Duration      float64                     `json:"duration"`
	ProcessTimeMs int                         `json:"processing_time_ms"`
}

// SpeakerSegment is a time range where a speaker is talking.
type SpeakerSegment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// Diarize sends audio to the sidecar for standalone speaker detection.
func (c *Client) Diarize(audio []byte, filename string) (*DiarizeResponse, error) {
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

	httpReq, err := http.NewRequest("POST", c.baseURL+"/diarize", &buf)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())

	slog.Debug("sending diarization request to sidecar", "filename", filename, "size", len(audio))

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sidecar request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("sidecar returned %d: %s", resp.StatusCode, string(body))
	}

	var result DiarizeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode diarization: %w", err)
	}
	return &result, nil
}

// Synthesize sends text to the sidecar for TTS and returns raw audio bytes.
func (c *Client) Synthesize(req SynthesizeRequest) ([]byte, string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, "", fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", c.baseURL+"/synthesize", bytes.NewReader(body))
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

// StreamURL returns the WebSocket URL for streaming ASR.
func (c *Client) StreamURL() string {
	return c.wsURL + "/stream"
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

// ListVoices fetches available TTS voice presets from the sidecar.
func (c *Client) ListVoices() (*VoicesResponse, error) {
	resp, err := c.httpClient.Get(c.baseURL + "/voices")
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
