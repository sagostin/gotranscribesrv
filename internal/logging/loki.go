package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// LogEntry is a single log line for Loki, paired with its timestamp.
type LogEntry struct {
	Timestamp time.Time
	Line      string
}

// LokiPushData is the top-level payload accepted by Loki's /loki/api/v1/push.
type LokiPushData struct {
	Streams []LokiStream `json:"streams"`
}

// LokiStream is one labeled stream; the values array is an array of
// [unix_nano, line] tuples.
type LokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

// LokiClient handles interactions with the Loki push API. It is
// safe for concurrent use (the underlying *http.Client is).
type LokiClient struct {
	PushURL  string
	Username string
	Password string
	client   *http.Client
}

// NewLokiClient initializes a new Loki client. pushURL should be the
// base Loki URL (e.g. http://loki:3100); the /loki/api/v1/push path
// is appended automatically. username/password are optional — if both
// are non-empty they are sent as HTTP Basic auth.
func NewLokiClient(pushURL, username, password string) *LokiClient {
	return &LokiClient{
		PushURL:  pushURL,
		Username: username,
		Password: password,
		client:   &http.Client{Timeout: 3 * time.Second},
	}
}

// PushLog sends a single log entry to Loki. Mirrors the gomsggw
// implementation: one entry, one HTTP call. Used by the consumer
// goroutine in LogManager. Errors are returned to the caller (which
// logs them locally) and the entry is dropped — we never block the
// channel waiting for Loki.
func (c *LokiClient) PushLog(labels map[string]string, entry LogEntry) error {
	payload := LokiPushData{
		Streams: []LokiStream{
			{
				Stream: labels,
				Values: [][2]string{
					{strconv.FormatInt(entry.Timestamp.UnixNano(), 10), entry.Line},
				},
			},
		},
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal JSON payload: %w", err)
	}

	req, err := http.NewRequest("POST", c.PushURL+"/loki/api/v1/push", bytes.NewBuffer(jsonPayload))
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Username != "" && c.Password != "" {
		req.SetBasicAuth(c.Username, c.Password)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request to Loki: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected response from Loki: %d", resp.StatusCode)
	}

	return nil
}
