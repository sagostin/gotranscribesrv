// Package pii wraps the Microsoft Presidio analyzer with a Go-side
// replacement operator and an in-process LRU cache, returning redacted
// text suitable for emission to the structured log pipeline.
//
// Design constraints:
//   - Logs only. Response bodies (raw transcripts) are NEVER modified.
//   - Fail-closed: when the analyzer is unreachable, errors, or returns
//     an invalid response, the returned string is the literal sentinel
//     "<REDACTED-ERROR>" so log entries never leak unredacted PII.
//   - Cache hits short-circuit the analyzer entirely. Within a single
//     request, repeated segments (same text) only pay the analyze cost
//     once. The cache is bounded and lock-protected.
//
// Replacement is performed right-to-left so that successive span
// offsets stay valid against the original string as it's mutated.
package pii

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/shaunagostinho/gotranscribesrv/internal/metrics"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
)

// RedactedErrorSentinel is the literal string substituted into log
// fields when the PII analyzer fails. Operators searching for this
// string in Loki can immediately surface every request whose log was
// scrubbed due to a redactor fault.
const RedactedErrorSentinel = "<REDACTED-ERROR>"

// Default entities passed to Presidio when PII_ENTITIES is unset.
// This is Presidio's "Global" set minus the lower-yield types.
// See https://microsoft.github.io/presidio/supported_entities/
var defaultEntities = []string{
	"PERSON",
	"EMAIL_ADDRESS",
	"PHONE_NUMBER",
	"CREDIT_CARD",
	"US_SSN",
	"IP_ADDRESS",
	"IBAN_CODE",
	"URL",
	"DATE_TIME",
	"LOCATION",
}

// cacheEntry is one LRU slot — a previously-redacted text plus the
// entities that produced it (for log enrichment).
type cacheEntry struct {
	redacted string
	items    []sidecar.PresidioEntity
}

// Redactor is the package's primary type. Construct via NewRedactor
// and share one instance across handlers — it is safe for concurrent
// use. The zero value is NOT usable.
type Redactor struct {
	enabled        bool
	client         *sidecar.PresidioClient
	entities       []string
	scoreThreshold float64
	cache          map[string]cacheEntry
	cacheOrder     []string // FIFO eviction; oldest first
	cacheMu        sync.Mutex
	cacheSize      int
	logger         *slog.Logger
}

// NewRedactor constructs a Redactor. enabled=false short-circuits all
// calls (the input string is returned verbatim with zero overhead).
// scoreThreshold filters low-confidence entities before replacement;
// pass 0 to disable filtering at the Go layer (Presidio itself accepts
// its own threshold in the request body — scoreThreshold here is a
// final safety net).
func NewRedactor(client *sidecar.PresidioClient, enabled bool, entities []string, scoreThreshold float64) *Redactor {
	if len(entities) == 0 {
		entities = defaultEntities
	}
	if scoreThreshold <= 0 {
		scoreThreshold = 0.6
	}
	return &Redactor{
		enabled:        enabled,
		client:         client,
		entities:       entities,
		scoreThreshold: scoreThreshold,
		cache:          make(map[string]cacheEntry, 64),
		cacheOrder:     make([]string, 0, 64),
		cacheSize:      64,
		logger:         slog.Default().With("component", "pii"),
	}
}

// Enabled reports whether redaction is active. Useful for handlers that
// want to skip wiring work entirely.
func (r *Redactor) Enabled() bool { return r != nil && r.enabled }

// Entities returns the configured entity list (for diagnostics / logs).
func (r *Redactor) Entities() []string {
	if r == nil {
		return nil
	}
	out := make([]string, len(r.entities))
	copy(out, r.entities)
	return out
}

// RedactText returns text with detected PII replaced by "<TYPE>" placeholders.
// On any error from the analyzer, returns (RedactedErrorSentinel, nil, err)
// — callers should log the error via PII_REDACTOR_ERROR and continue
// without blocking the request path.
//
// Empty input is returned unchanged (no analyzer call, no metric).
func (r *Redactor) RedactText(ctx context.Context, text string) (string, []sidecar.PresidioEntity, error) {
	if !r.Enabled() {
		return text, nil, nil
	}
	if text == "" {
		return text, nil, nil
	}

	// Cache lookup keyed by sha256(text). Repeated segments within a
	// single request (e.g. multiple ASR segments with the same PII)
	// bypass the analyzer entirely.
	key := cacheKey(text)
	if cached, ok := r.cacheGet(key); ok {
		return cached.redacted, cached.items, nil
	}

	start := time.Now()
	entities, err := r.client.Analyze(ctx, text, r.entities)
	metrics.RecordPIILatency(time.Since(start), err == nil)
	if err != nil {
		metrics.RecordPIIError("analyzer_error")
		r.logger.Warn("presidio analyzer failed; emitting sentinel",
			"error", err, "text_len", len(text))
		return RedactedErrorSentinel, nil, err
	}

	// Apply the local score threshold as a safety net (the analyzer
	// also accepts one, but defense-in-depth is cheap).
	filtered := make([]sidecar.PresidioEntity, 0, len(entities))
	for _, e := range entities {
		if e.Score >= r.scoreThreshold {
			filtered = append(filtered, e)
		}
	}

	redacted := r.replace(text, filtered)

	// Track per-entity-type redactions in Prometheus.
	for _, e := range filtered {
		metrics.RecordPIIRedaction(e.EntityType)
	}

	r.cachePut(key, cacheEntry{redacted: redacted, items: filtered})
	return redacted, filtered, nil
}

// replace walks entities right-to-left and replaces each span with a
// "<TYPE>" placeholder. Right-to-left is essential: replacing earlier
// spans first would invalidate the offsets of later spans.
//
// Entities with invalid offsets (Start < 0, End > len(text), End <=
// Start, Start >= len(text)) are skipped silently — they would otherwise
// produce corrupted output and they're typically Presidio bugs or
// out-of-range edge cases.
func (r *Redactor) replace(text string, entities []sidecar.PresidioEntity) string {
	if len(entities) == 0 {
		return text
	}

	// Filter and sort a copy so we don't mutate the caller's slice.
	clean := make([]sidecar.PresidioEntity, 0, len(entities))
	for _, e := range entities {
		if e.Start < 0 || e.End > len(text) || e.End <= e.Start {
			continue
		}
		clean = append(clean, e)
	}
	if len(clean) == 0 {
		return text
	}
	sort.Slice(clean, func(i, j int) bool {
		return clean[i].Start > clean[j].Start // descending: rightmost first
	})

	// Operate on a []byte to avoid allocating a new string per
	// replacement; strings.Builder would also work but []byte is
	// straightforward and the typical transcript is <10 KB.
	buf := []byte(text)
	for _, e := range clean {
		placeholder := "<" + e.EntityType + ">"
		// buf = buf[:e.Start] + placeholder + buf[e.End:]
		newBuf := make([]byte, 0, len(buf)-(e.End-e.Start)+len(placeholder))
		newBuf = append(newBuf, buf[:e.Start]...)
		newBuf = append(newBuf, placeholder...)
		newBuf = append(newBuf, buf[e.End:]...)
		buf = newBuf
	}
	return string(buf)
}

// ── LRU cache ───────────────────────────────────────────────────

func cacheKey(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func (r *Redactor) cacheGet(key string) (cacheEntry, bool) {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	e, ok := r.cache[key]
	return e, ok
}

func (r *Redactor) cachePut(key string, e cacheEntry) {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	if _, exists := r.cache[key]; exists {
		// Update existing entry in-place; no order change needed.
		r.cache[key] = e
		return
	}
	if len(r.cacheOrder) >= r.cacheSize {
		// Evict oldest.
		oldest := r.cacheOrder[0]
		r.cacheOrder = r.cacheOrder[1:]
		delete(r.cache, oldest)
	}
	r.cache[key] = e
	r.cacheOrder = append(r.cacheOrder, key)
}
