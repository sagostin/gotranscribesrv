package metrics

import (
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"
)

var (
	// HTTP request metrics
	httpRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gotranscribesrv_http_requests_total",
		Help: "Total number of HTTP requests by method, path, and status code.",
	}, []string{"method", "path", "status"})

	httpRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gotranscribesrv_http_request_duration_seconds",
		Help:    "HTTP request duration in seconds.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
	}, []string{"method", "path"})

	// ASR-specific metrics
	asrRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gotranscribesrv_asr_requests_total",
		Help: "Total ASR requests by endpoint and whether diarization was used.",
	}, []string{"endpoint", "diarized"})

	asrAudioDurationTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gotranscribesrv_asr_audio_duration_seconds_total",
		Help: "Total audio duration processed in seconds.",
	})

	asrProcessingDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gotranscribesrv_asr_processing_duration_seconds",
		Help:    "ASR processing duration in seconds.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
	}, []string{"endpoint"})

	// WebSocket connections gauge
	ActiveWebSocketConnections = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gotranscribesrv_active_websocket_connections",
		Help: "Number of active WebSocket connections by protocol.",
	}, []string{"protocol"})
)

// PrometheusMiddleware returns a Fiber handler that instruments HTTP requests
// with Prometheus counters and histograms.
func PrometheusMiddleware() fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()

		err := c.Next()

		duration := time.Since(start).Seconds()
		status := strconv.Itoa(c.Response().StatusCode())
		method := c.Method()
		path := normalizePath(c.Route().Path, c.Path())

		httpRequestsTotal.WithLabelValues(method, path, status).Inc()
		httpRequestDuration.WithLabelValues(method, path).Observe(duration)

		return err
	}
}

// RecordASRUsage records ASR-specific metrics (called from the usage tracker flush path).
func RecordASRUsage(endpoint string, audioDurationMs, processTimeMs int, diarized bool) {
	diarizedStr := "false"
	if diarized {
		diarizedStr = "true"
	}

	asrRequestsTotal.WithLabelValues(endpoint, diarizedStr).Inc()
	asrAudioDurationTotal.Add(float64(audioDurationMs) / 1000.0)
	asrProcessingDuration.WithLabelValues(endpoint).Observe(float64(processTimeMs) / 1000.0)
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
