package config

import (
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

	// Python Sidecar (LLM only — MLX)
	LLMSidecarURL string

	// Models
	ASRModel          string
	ASRRuntime        string
	EnableDiarization bool
	EnableTTS         bool
	EnableLLM         bool

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

	// Metrics
	MetricsEnabled bool
	MetricsPath    string
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

		LLMSidecarURL: envOrDefault("LLM_SIDECAR_URL", "http://localhost:8100"),

		ASRModel:          envOrDefault("ASR_MODEL", "mlx-community/parakeet-tdt-0.6b-v3"),
		ASRRuntime:        envOrDefault("ASR_RUNTIME", "mlx"),
		EnableDiarization: envOrDefault("ENABLE_DIARIZATION", "true") == "true",
		EnableTTS:         envOrDefault("ENABLE_TTS", "true") == "true",
		EnableLLM:         envOrDefault("ENABLE_LLM", "false") == "true",

		RateLimitFree:       envOrDefaultInt("RATE_LIMIT_FREE", 20),
		RateLimitPro:        envOrDefaultInt("RATE_LIMIT_PRO", 120),
		RateLimitEnterprise: envOrDefaultInt("RATE_LIMIT_ENTERPRISE", 0),

		RegistrationEnabled: envOrDefault("REGISTRATION_ENABLED", "false") == "true",

		VoicesDataDir: envOrDefault("VOICES_DATA_DIR", "data/voices"),

		LogLevel: envOrDefault("LOG_LEVEL", "info"),

		MetricsEnabled: envOrDefault("METRICS_ENABLED", "true") == "true",
		MetricsPath:    envOrDefault("METRICS_PATH", "/metrics"),
	}
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
