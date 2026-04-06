package metrics

import (
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"
)

// enabled gates all recording functions. When false, every Record*/Track* call
// is a no-op and no collectors are registered with Prometheus.
var enabled bool

// ──────────────────────────────────────────────────────────────
// HTTP request metrics
// ──────────────────────────────────────────────────────────────

var (
	httpRequestsTotal     *prometheus.CounterVec
	httpRequestDuration   *prometheus.HistogramVec
	httpRequestsPerMinute *prometheus.GaugeVec
)

// ──────────────────────────────────────────────────────────────
// ASR metrics
// ──────────────────────────────────────────────────────────────

var (
	asrRequestsTotal      *prometheus.CounterVec
	asrAudioDurationTotal prometheus.Counter
	asrAudioMinutesTotal  prometheus.Counter
	asrProcessingDuration *prometheus.HistogramVec
	asrRealtimeFactor     *prometheus.HistogramVec
)

// ──────────────────────────────────────────────────────────────
// WebSocket metrics
// ──────────────────────────────────────────────────────────────

// ActiveWebSocketConnections tracks live WS connections by protocol.
// Exported so handlers can call .Inc() / .Dec() directly.
var ActiveWebSocketConnections *prometheus.GaugeVec

// ──────────────────────────────────────────────────────────────
// TTS metrics
// ──────────────────────────────────────────────────────────────

var (
	ttsRequestsTotal *prometheus.CounterVec
	ttsDuration      *prometheus.HistogramVec
)

// ──────────────────────────────────────────────────────────────
// LLM metrics
// ──────────────────────────────────────────────────────────────

var (
	llmRequestsTotal *prometheus.CounterVec
	llmTokensTotal   *prometheus.CounterVec
	llmDuration      *prometheus.HistogramVec
)

// ──────────────────────────────────────────────────────────────
// Sidecar metrics
// ──────────────────────────────────────────────────────────────

var (
	sidecarRequestDuration *prometheus.HistogramVec
	sidecarErrorsTotal     *prometheus.CounterVec
)

// ──────────────────────────────────────────────────────────────
// Auth / rate-limit metrics
// ──────────────────────────────────────────────────────────────

var (
	authAttemptsTotal        *prometheus.CounterVec
	rateLimitRejectionsTotal *prometheus.CounterVec
)

// ──────────────────────────────────────────────────────────────
// Operational gauges
// ──────────────────────────────────────────────────────────────

var (
	activeUsersGauge prometheus.Gauge
	buildInfo        *prometheus.GaugeVec
)

// per-minute ticker state — uses atomic counter so the ticker goroutine
// can compute request-per-minute deltas without reading Prometheus internals.
var (
	httpRequestCounter uint64 // atomic; incremented on every request
	prevHTTPCount      uint64 // previous tick snapshot
	activeUsersMu      sync.Mutex
	activeUserSet      = make(map[string]time.Time)
)

// ──────────────────────────────────────────────────────────────
// Init registers all Prometheus collectors when enabled is true.
// Must be called once at startup before any Record* calls.
// ──────────────────────────────────────────────────────────────

func Init(metricsEnabled bool) {
	enabled = metricsEnabled
	if !enabled {
		return
	}

	// ── HTTP ──────────────────────────────────────────────
	httpRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gotranscribesrv_http_requests_total",
		Help: "Total number of HTTP requests by method, path, and status code.",
	}, []string{"method", "path", "status"})

	httpRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gotranscribesrv_http_request_duration_seconds",
		Help:    "HTTP request duration in seconds.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
	}, []string{"method", "path"})

	httpRequestsPerMinute = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gotranscribesrv_http_requests_per_minute",
		Help: "HTTP requests in the last 60-second window.",
	}, []string{"method"})

	// ── ASR ───────────────────────────────────────────────
	asrRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gotranscribesrv_asr_requests_total",
		Help: "Total ASR requests by endpoint and whether diarization was used.",
	}, []string{"endpoint", "diarized"})

	asrAudioDurationTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gotranscribesrv_asr_audio_duration_seconds_total",
		Help: "Total audio duration processed in seconds.",
	})

	asrAudioMinutesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gotranscribesrv_asr_audio_minutes_total",
		Help: "Total audio duration processed in minutes.",
	})

	asrProcessingDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gotranscribesrv_asr_processing_duration_seconds",
		Help:    "ASR processing duration in seconds.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
	}, []string{"endpoint"})

	asrRealtimeFactor = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gotranscribesrv_asr_realtime_factor",
		Help:    "Ratio of audio duration to processing time (>1 = faster than real-time).",
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 20, 50, 100},
	}, []string{"endpoint"})

	// ── WebSocket ─────────────────────────────────────────
	ActiveWebSocketConnections = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gotranscribesrv_active_websocket_connections",
		Help: "Number of active WebSocket connections by protocol.",
	}, []string{"protocol"})

	// ── TTS ───────────────────────────────────────────────
	ttsRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gotranscribesrv_tts_requests_total",
		Help: "Total TTS synthesis requests by voice.",
	}, []string{"voice"})

	ttsDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gotranscribesrv_tts_duration_seconds",
		Help:    "TTS synthesis duration in seconds.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{})

	// ── LLM ───────────────────────────────────────────────
	llmRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gotranscribesrv_llm_requests_total",
		Help: "Total LLM processing requests by task type.",
	}, []string{"task"})

	llmTokensTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gotranscribesrv_llm_tokens_generated_total",
		Help: "Total tokens generated by LLM by task type.",
	}, []string{"task"})

	llmDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gotranscribesrv_llm_duration_seconds",
		Help:    "LLM processing duration in seconds.",
		Buckets: []float64{0.5, 1, 2.5, 5, 10, 30, 60, 120},
	}, []string{"task"})

	// ── Sidecar ───────────────────────────────────────────
	sidecarRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gotranscribesrv_sidecar_request_duration_seconds",
		Help:    "Round-trip latency to inference sidecars.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
	}, []string{"sidecar", "operation"})

	sidecarErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gotranscribesrv_sidecar_errors_total",
		Help: "Total sidecar request errors by sidecar and operation.",
	}, []string{"sidecar", "operation"})

	// ── Auth / rate-limit ─────────────────────────────────
	authAttemptsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gotranscribesrv_auth_attempts_total",
		Help: "Authentication attempts by method and result.",
	}, []string{"method", "result"})

	rateLimitRejectionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gotranscribesrv_rate_limit_rejections_total",
		Help: "Rate limit rejections (429s) by user tier.",
	}, []string{"tier"})

	// ── Operational ───────────────────────────────────────
	activeUsersGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gotranscribesrv_active_users",
		Help: "Distinct users seen in the last 60 seconds.",
	})

	buildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gotranscribesrv_build_info",
		Help: "Static build metadata.",
	}, []string{"version", "go_version"})

	// ── Register all ──────────────────────────────────────
	prometheus.MustRegister(
		httpRequestsTotal,
		httpRequestDuration,
		httpRequestsPerMinute,
		asrRequestsTotal,
		asrAudioDurationTotal,
		asrAudioMinutesTotal,
		asrProcessingDuration,
		asrRealtimeFactor,
		ActiveWebSocketConnections,
		ttsRequestsTotal,
		ttsDuration,
		llmRequestsTotal,
		llmTokensTotal,
		llmDuration,
		sidecarRequestDuration,
		sidecarErrorsTotal,
		authAttemptsTotal,
		rateLimitRejectionsTotal,
		activeUsersGauge,
		buildInfo,
	)

	// Set build info (constant gauge = 1)
	buildInfo.WithLabelValues("dev", runtime.Version()).Set(1)

	// Start per-minute ticker for computed gauges
	go perMinuteTicker()
}

// ──────────────────────────────────────────────────────────────
// Per-minute ticker — updates request-rate gauge and active-user count
// ──────────────────────────────────────────────────────────────

func perMinuteTicker() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		updateRequestsPerMinute()
		updateActiveUsers()
	}
}

func updateRequestsPerMinute() {
	current := atomic.LoadUint64(&httpRequestCounter)
	delta := current - prevHTTPCount
	prevHTTPCount = current
	httpRequestsPerMinute.WithLabelValues("all").Set(float64(delta))
}

func updateActiveUsers() {
	activeUsersMu.Lock()
	cutoff := time.Now().Add(-60 * time.Second)
	count := 0
	for uid, ts := range activeUserSet {
		if ts.After(cutoff) {
			count++
		} else {
			delete(activeUserSet, uid)
		}
	}
	activeUsersMu.Unlock()

	activeUsersGauge.Set(float64(count))
}

// ──────────────────────────────────────────────────────────────
// Recording functions (all no-ops when disabled)
// ──────────────────────────────────────────────────────────────

// PrometheusMiddleware returns a Fiber handler that instruments HTTP requests
// with Prometheus counters and histograms.
func PrometheusMiddleware() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if !enabled {
			return c.Next()
		}

		start := time.Now()

		err := c.Next()

		duration := time.Since(start).Seconds()
		status := strconv.Itoa(c.Response().StatusCode())
		method := c.Method()
		path := normalizePath(c.Route().Path, c.Path())

		httpRequestsTotal.WithLabelValues(method, path, status).Inc()
		httpRequestDuration.WithLabelValues(method, path).Observe(duration)
		atomic.AddUint64(&httpRequestCounter, 1)

		return err
	}
}

// RecordASRUsage records ASR-specific metrics (called from the usage tracker flush path).
func RecordASRUsage(endpoint string, audioDurationMs, processTimeMs int, diarized bool) {
	if !enabled {
		return
	}

	diarizedStr := "false"
	if diarized {
		diarizedStr = "true"
	}

	asrRequestsTotal.WithLabelValues(endpoint, diarizedStr).Inc()

	audioSec := float64(audioDurationMs) / 1000.0
	asrAudioDurationTotal.Add(audioSec)
	asrAudioMinutesTotal.Add(audioSec / 60.0)
	asrProcessingDuration.WithLabelValues(endpoint).Observe(float64(processTimeMs) / 1000.0)

	// Real-time factor: audio_duration / process_time (higher = faster than real-time)
	if processTimeMs > 0 {
		rtf := float64(audioDurationMs) / float64(processTimeMs)
		asrRealtimeFactor.WithLabelValues(endpoint).Observe(rtf)
	}
}

// RecordTTSUsage records TTS synthesis metrics.
func RecordTTSUsage(voice string, processTimeMs int) {
	if !enabled {
		return
	}
	ttsRequestsTotal.WithLabelValues(voice).Inc()
	ttsDuration.WithLabelValues().Observe(float64(processTimeMs) / 1000.0)
}

// RecordLLMUsage records LLM processing metrics.
func RecordLLMUsage(task string, tokensGenerated, processTimeMs int) {
	if !enabled {
		return
	}
	llmRequestsTotal.WithLabelValues(task).Inc()
	llmTokensTotal.WithLabelValues(task).Add(float64(tokensGenerated))
	llmDuration.WithLabelValues(task).Observe(float64(processTimeMs) / 1000.0)
}

// RecordSidecarLatency records sidecar round-trip latency and errors.
// sidecar should be "swift" or "python". operation should be the endpoint name.
func RecordSidecarLatency(sidecar, operation string, durationMs int, err error) {
	if !enabled {
		return
	}
	sidecarRequestDuration.WithLabelValues(sidecar, operation).Observe(float64(durationMs) / 1000.0)
	if err != nil {
		sidecarErrorsTotal.WithLabelValues(sidecar, operation).Inc()
	}
}

// RecordAuthAttempt records an authentication attempt.
// method: "jwt", "api_key", "login", "register"
// result: "success", "failure"
func RecordAuthAttempt(method, result string) {
	if !enabled {
		return
	}
	authAttemptsTotal.WithLabelValues(method, result).Inc()
}

// RecordRateLimitRejection records a rate limit 429 rejection by tier.
func RecordRateLimitRejection(tier string) {
	if !enabled {
		return
	}
	rateLimitRejectionsTotal.WithLabelValues(tier).Inc()
}

// TrackUser marks a user as active for the active-users gauge.
func TrackUser(userID string) {
	if !enabled || userID == "" {
		return
	}
	activeUsersMu.Lock()
	activeUserSet[userID] = time.Now()
	activeUsersMu.Unlock()
}

// Handler returns a Fiber handler that serves the Prometheus metrics endpoint.
func Handler() fiber.Handler {
	handler := promhttp.Handler()
	return func(c *fiber.Ctx) error {
		fasthttpadaptor.NewFastHTTPHandler(handler)(c.Context())
		return nil
	}
}

// normalizePath returns the route pattern if available, otherwise the raw path.
// This prevents high-cardinality label explosion from path parameters.
func normalizePath(routePattern, rawPath string) string {
	if routePattern != "" {
		return routePattern
	}
	return rawPath
}
