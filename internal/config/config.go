package config

import (
	"fmt"
	"os"
	"time"
)

// Config holds all application configuration.
type Config struct {
	// Server
	Port        string
	Environment string

	// Database
	DatabaseURL string

	// JWT
	JWTSecret     string
	JWTAccessTTL  time.Duration
	JWTRefreshTTL time.Duration

	// Swift Sidecar (ASR, VAD, Diarization, TTS — CoreML/ANE)
	SwiftSidecarURL   string
	SwiftSidecarWSURL string

	// Models
	ASRModel          string
	ASRRuntime        string
	EnableDiarization bool
	EnableTTS         bool

	// ITN (Inverse Text Normalization) — spoken-form ASR -> written form
	// ("one two five O" -> "1250"). Applied in the Swift sidecar. Default on.
	EnableITN bool

	// Rate Limits
	RateLimitFree       int
	RateLimitPro        int
	RateLimitEnterprise int

	// Registration
	RegistrationEnabled bool

	// Voice Storage
	VoicesDataDir string

	// Logging
	LogLevel string

	// ServerID identifies this node in every log entry (slog attr +
	// LoggingFormat JSON payload) and as the Loki `server_id` label —
	// useful when running multiple nodes behind a load balancer.
	ServerID string

	// Metrics
	MetricsEnabled bool
	MetricsPath    string

	// Loki (optional Grafana Loki structured-log shipping).
	// When LokiEnabled is false the LogManager is created with a nil
	// LokiClient and the consumer goroutine short-circuits, so the
	// cost is one idle goroutine + a 512-buffered channel.
	LokiEnabled  bool
	LokiPushURL  string
	LokiUsername string
	LokiPassword string
	LokiJob      string

	// PII redaction in logs (Loki + stdout only — response bodies are
	// never modified). Presidio-analyzer is the analyzer; replacement
	// is performed in Go. The redactor is fail-closed: on any error
	// from the analyzer, log fields are replaced with "<REDACTED-ERROR>".
	EnablePII         bool
	PresidioURL       string
	PresidioTimeoutMs int
	PIIEntities       string // CSV; empty = use built-in default set
	PIIScoreThreshold float64
}

// Load reads configuration from environment variables with sensible defaults.
func Load() *Config {
	return &Config{
		Port:        envOrDefault("PORT", "3000"),
		Environment: envOrDefault("ENVIRONMENT", "development"),

		DatabaseURL: envOrDefault("DATABASE_URL", "postgres://transcribesrv:changeme@localhost:5432/transcribesrv?sslmode=disable"),

		JWTSecret:     envOrDefault("JWT_SECRET", "change-this-to-a-random-64-character-string"),
		JWTAccessTTL:  parseDuration("JWT_ACCESS_TTL", 15*time.Minute),
		JWTRefreshTTL: parseDuration("JWT_REFRESH_TTL", 168*time.Hour),

		SwiftSidecarURL:   envOrDefault("SWIFT_SIDECAR_URL", "http://127.0.0.1:8101"),
		SwiftSidecarWSURL: envOrDefault("SWIFT_SIDECAR_WS_URL", "ws://127.0.0.1:8101"),

		ASRModel:          envOrDefault("ASR_MODEL", "mlx-community/parakeet-tdt-0.6b-v3"),
		ASRRuntime:        envOrDefault("ASR_RUNTIME", "mlx"),
		EnableDiarization: envOrDefault("ENABLE_DIARIZATION", "true") == "true",
		EnableTTS:         envOrDefault("ENABLE_TTS", "true") == "true",
		EnableITN:         envOrDefault("ENABLE_ITN", "true") == "true",

		RateLimitFree:       envOrDefaultInt("RATE_LIMIT_FREE", 20),
		RateLimitPro:        envOrDefaultInt("RATE_LIMIT_PRO", 120),
		RateLimitEnterprise: envOrDefaultInt("RATE_LIMIT_ENTERPRISE", 0),

		RegistrationEnabled: envOrDefault("REGISTRATION_ENABLED", "false") == "true",

		VoicesDataDir: envOrDefault("VOICES_DATA_DIR", "data/voices"),

		LogLevel: envOrDefault("LOG_LEVEL", "info"),

		ServerID: envOrDefault("SERVER_ID", ""),

		MetricsEnabled: envOrDefault("METRICS_ENABLED", "true") == "true",
		MetricsPath:    envOrDefault("METRICS_PATH", "/metrics"),

		LokiEnabled:  envOrDefault("LOKI_ENABLED", "false") == "true",
		LokiPushURL:  envOrDefault("LOKI_PUSH_URL", "http://loki:3100"),
		LokiUsername: envOrDefault("LOKI_USERNAME", ""),
		LokiPassword: envOrDefault("LOKI_PASSWORD", ""),
		LokiJob:      envOrDefault("LOKI_JOB", "gotranscribesrv"),

		EnablePII:         envOrDefault("ENABLE_PII", "true") == "true",
		PresidioURL:       envOrDefault("PRESIDIO_ANALYZER_URL", "http://presidio-analyzer:3000"),
		PresidioTimeoutMs: envOrDefaultInt("PRESIDIO_TIMEOUT_MS", 3000),
		PIIEntities:       envOrDefault("PII_ENTITIES", ""),
		PIIScoreThreshold: envOrDefaultFloat("PII_SCORE_THRESHOLD", 0.6),
	}
}

func envOrDefaultFloat(key string, fallback float64) float64 {
	val := os.Getenv(key)
	if val == "" {
		return fallback
	}
	var f float64
	if _, err := fmt.Sscanf(val, "%f", &f); err != nil {
		return fallback
	}
	return f
}

// IsProd returns true if running in production mode.
func (c *Config) IsProd() bool {
	return c.Environment == "production"
}

func envOrDefault(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func envOrDefaultInt(key string, fallback int) int {
	val := os.Getenv(key)
	if val == "" {
		return fallback
	}
	var i int
	for _, c := range val {
		if c < '0' || c > '9' {
			return fallback
		}
		i = i*10 + int(c-'0')
	}
	return i
}

func parseDuration(key string, fallback time.Duration) time.Duration {
	val := os.Getenv(key)
	if val == "" {
		return fallback
	}
	d, err := time.ParseDuration(val)
	if err != nil {
		return fallback
	}
	return d
}
