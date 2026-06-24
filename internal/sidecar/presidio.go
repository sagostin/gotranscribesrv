package sidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/shaunagostinho/gotranscribesrv/internal/metrics"
)

// PresidioEntity is a single PII entity span returned by the
// Microsoft Presidio analyzer. Indices are character offsets into
// the original UTF-8 text, with End exclusive.
type PresidioEntity struct {
	Start      int     `json:"start"`
	End        int     `json:"end"`
	Score      float64 `json:"score"`
	EntityType string  `json:"entity_type"`
}

// PresidioClient is a thin HTTP wrapper around the Microsoft Presidio
// analyzer REST API (mcr.microsoft.com/presidio-analyzer). It exposes a
// single operation, Analyze, which returns detected PII spans for a
// given text. Replacement is performed in Go (see internal/pii.Redactor),
// not by the analyzer service.
type PresidioClient struct {
	baseURL string
	client  *http.Client
}

// NewPresidioClient creates a new client. baseURL is the analyzer's
// root URL with no trailing slash (e.g. "http://presidio-analyzer:3000").
// timeout caps each request; 3s is a sensible default for a local
// container — Presidio's spaCy analyzer adds ~50–200 ms of latency
// for a typical transcript segment.
func NewPresidioClient(baseURL string, timeout time.Duration) *PresidioClient {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &PresidioClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client: &http.Client{
			Timeout: timeout,
		},
	}
}

// Analyze requests PII entity spans for the given text. entities is an
// optional allowlist of entity types (e.g. ["PERSON", "PHONE_NUMBER"]);
// when empty, the analyzer uses its default global entity set.
//
// Returns the raw spans as Presidio reports them. The caller is
// responsible for filtering by score, validating offsets, and
// performing replacement.
func (p *PresidioClient) Analyze(ctx context.Context, text string, entities []string) ([]PresidioEntity, error) {
	if text == "" {
		return nil, nil
	}

	body := map[string]any{
		"text":     text,
		"language": "en",
	}
	if len(entities) > 0 {
		body["entities"] = entities
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal presidio request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/analyze", bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("create presidio request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("presidio request failed: %w", err)
	}
	defer resp.Body.Close()

	metrics.RecordSidecarLatency("presidio", "analyze", int(time.Since(start).Milliseconds()), err)

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("presidio returned %d: %s", resp.StatusCode, string(errBody))
	}

	var entitiesOut []PresidioEntity
	if err := json.NewDecoder(resp.Body).Decode(&entitiesOut); err != nil {
		return nil, fmt.Errorf("decode presidio response: %w", err)
	}

	return entitiesOut, nil
}
