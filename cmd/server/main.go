package main

import (
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	fiberlogger "github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/joho/godotenv"
	"github.com/shaunagostinho/gotranscribesrv/internal/config"
	"github.com/shaunagostinho/gotranscribesrv/internal/database"
	"github.com/shaunagostinho/gotranscribesrv/internal/handlers"
	"github.com/shaunagostinho/gotranscribesrv/internal/logging"
	"github.com/shaunagostinho/gotranscribesrv/internal/metrics"
	"github.com/shaunagostinho/gotranscribesrv/internal/middleware"
	"github.com/shaunagostinho/gotranscribesrv/internal/models"
	"github.com/shaunagostinho/gotranscribesrv/internal/pii"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
)

func main() {
	migrateOnly := flag.Bool("migrate", false, "Run database migrations and exit")
	flag.Parse()

	// Load .env file if present
	_ = godotenv.Load()

	// Load configuration
	cfg := config.Load()

	// Setup structured logging. The base handler prints to stdout in
	// text format; the ContextHandler wrapper additionally pulls
	// request_id off context.Context and attaches it as a slog attr
	// to every record — so slog.InfoContext(c.UserContext(), ...)
	// calls in handlers automatically get correlated. When SERVER_ID
	// is set, it is baked in as a slog attr on EVERY record so stdout
	// lines can be attributed to a node when running multiple minis.
	logLevel := slog.LevelInfo
	if cfg.Environment == "development" {
		logLevel = slog.LevelDebug
	}
	baseHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})
	ctxHandler := logging.NewContextHandler(baseHandler)
	if cfg.ServerID != "" {
		ctxHandler = logging.NewContextHandler(baseHandler.WithAttrs([]slog.Attr{slog.String("server_id", cfg.ServerID)}))
	}
	slog.SetDefault(slog.New(ctxHandler))

	slog.Info("starting GoTranscribeSrv",
		"environment", cfg.Environment,
		"port", cfg.Port,
		"itn", cfg.EnableITN,
	)

	// Build the LogManager. When LOKI_ENABLED=false, a nil LokiClient
	// is passed and the consumer goroutine short-circuits — only the
	// local slog emit in SendLog runs. CloseLogManager drains the
	// channel on shutdown regardless.
	var lokiClient *logging.LokiClient
	if cfg.LokiEnabled {
		lokiClient = logging.NewLokiClient(cfg.LokiPushURL, cfg.LokiUsername, cfg.LokiPassword)
		slog.Info("loki logging enabled", "url", cfg.LokiPushURL, "job", cfg.LokiJob)
	}
	logManager := logging.NewLogManager(lokiClient, cfg.LokiEnabled, cfg.ServerID)
	defer logManager.CloseLogManager()

	// Initialize Prometheus metrics (no-op when METRICS_ENABLED=false)
	metrics.Init(cfg.MetricsEnabled)

	// Connect to database
	db, err := database.Connect(cfg.DatabaseURL, cfg.IsProd())
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// Run migrations
	if err := db.Migrate(); err != nil {
		slog.Error("failed to run migrations", "error", err)
		os.Exit(1)
	}
	slog.Info("database migrations complete")

	// Seed default admin user + API key on first run
	database.SeedAdmin(db.DB)

	if *migrateOnly {
		slog.Info("migrations complete, exiting")
		return
	}

	// Start periodic cleanup of expired blacklisted tokens
	go middleware.CleanupBlacklist(db.DB)

	// Create sidecar client:
	// Swift sidecar (ASR, VAD, diarization, TTS — CoreML/ANE)
	sc := sidecar.NewClient(
		cfg.SwiftSidecarURL, cfg.SwiftSidecarWSURL,
	)

	// PII redactor — wraps the Presidio analyzer service and applies
	// <TYPE> placeholders to log fields. Always constructed (even when
	// disabled) so handlers can call RedactText unconditionally.
	var entities []string
	if cfg.PIIEntities != "" {
		entities = strings.Split(cfg.PIIEntities, ",")
		for i := range entities {
			entities[i] = strings.TrimSpace(entities[i])
		}
	}
	presidio := sidecar.NewPresidioClient(cfg.PresidioURL, time.Duration(cfg.PresidioTimeoutMs)*time.Millisecond)
	redactor := pii.NewRedactor(presidio, cfg.EnablePII, entities, cfg.PIIScoreThreshold)
	if cfg.EnablePII {
		slog.Info("PII redaction enabled", "presidio_url", cfg.PresidioURL, "entities", redactor.Entities())
	} else {
		slog.Info("PII redaction DISABLED — logs will contain raw transcript text")
	}

	// Create Fiber app
	app := fiber.New(fiber.Config{
		AppName:     "GoTranscribeSrv",
		BodyLimit:   100 * 1024 * 1024, // 100MB
		ProxyHeader: "X-Forwarded-For", // Trust proxy headers from Caddy/nginx
		// Extract the first valid IP from the XFF chain (e.g. "client, proxy"
		// as appended by Caddy) instead of logging the whole chain string.
		EnableIPValidation: true,
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return c.Status(code).JSON(fiber.Map{
				"error": fiber.Map{
					"code":    "INTERNAL_ERROR",
					"message": err.Error(),
					"status":  code,
				},
			})
		},
	})

	// Global middleware
	app.Use(middleware.RequestID())
	app.Use(recover.New())
	// Access log carries server_id (baked in, static per process) and
	// request_id (from the RequestID middleware's Locals) so raw HTTP
	// lines correlate with the structured slog/Loki events.
	accessLogFormat := "${time} | ${status} | ${latency} | ${method} ${path}"
	if cfg.ServerID != "" {
		accessLogFormat += " | server=" + cfg.ServerID
	}
	accessLogFormat += " | req=${locals:request_id}\n"
	app.Use(fiberlogger.New(fiberlogger.Config{
		Format: accessLogFormat,
	}))
	app.Use(cors.New(cors.Config{
		AllowOrigins: "*",
		AllowMethods: "GET,POST,PUT,DELETE,OPTIONS",
		AllowHeaders: "Authorization,Content-Type,X-API-Key",
	}))

	// Prometheus metrics middleware (before auth so all requests are instrumented)
	if cfg.MetricsEnabled {
		app.Use(metrics.PrometheusMiddleware())
		app.Get(cfg.MetricsPath, metrics.Handler())
		slog.Info("prometheus metrics enabled", "path", cfg.MetricsPath)
	}

	// Auth config
	authCfg := middleware.AuthConfig{
		Secret:     cfg.JWTSecret,
		AccessTTL:  cfg.JWTAccessTTL,
		RefreshTTL: cfg.JWTRefreshTTL,
		DB:         db.DB,
		LogManager: logManager,
	}

	// Create middleware
	rateLimiter := middleware.NewRateLimiter(cfg.RateLimitFree, cfg.RateLimitPro, cfg.RateLimitEnterprise)
	usageTracker := middleware.NewUsageTracker(db.DB, 1000, logManager)

	// Create handlers
	authHandler := handlers.NewAuthHandler(db.DB, authCfg, cfg.RegistrationEnabled)
	asrHandler := handlers.NewASRHandler(sc, redactor, cfg.EnableITN, logManager)
	whisperHandler := handlers.NewWhisperHandler(sc, redactor, cfg.EnableITN, logManager)
	voiceHandler := handlers.NewVoiceHandler(db.DB, sc, cfg.VoicesDataDir, logManager)
	ttsHandler := handlers.NewTTSHandler(sc, voiceHandler, logManager)
	openaiTTSHandler := handlers.NewOpenAITTSHandler(sc, logManager, cfg.TTSDefaultBackend)
	usageHandler := handlers.NewUsageHandler(db.DB)
	keysHandler := handlers.NewKeysHandler(db.DB)
	watsonHandler := handlers.NewWatsonHandler(sc, redactor, db.DB, cfg.EnableITN, logManager)
	openaiRealtimeHandler := handlers.NewOpenAIRealtimeHandler(sc, logManager)
	deepgramRealtimeHandler := handlers.NewDeepgramRealtimeHandler(sc, logManager)

	// === Health ===
	app.Get("/health", func(c *fiber.Ctx) error {
		health, err := sc.Health()
		sidecarStatus := "connected"
		models := fiber.Map{}
		if err != nil {
			sidecarStatus = "disconnected"
		} else if health != nil {
			for k, v := range health.Models {
				models[k] = v
			}
		}
		return c.JSON(fiber.Map{
			"status":  "ok",
			"sidecar": sidecarStatus,
			"models":  models,
		})
	})

	// === Public Auth Routes ===
	auth := app.Group("/api/v1/auth")
	auth.Post("/register", authHandler.Register)
	auth.Post("/login", authHandler.Login)
	auth.Post("/refresh", authHandler.Refresh)

	// === WebSocket Routes (must be registered BEFORE the authed group,
	// which has an empty prefix and would intercept upgrade requests) ===

	// WebSocket ASR (streaming — native protocol)
	wsHandler := handlers.NewWSHandler(sc, db.DB, cfg.EnableITN, logManager)
	app.Use("/ws/asr", func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			slog.InfoContext(c.UserContext(), "WebSocket upgrade request", "path", "/ws/asr", "remote", c.IP())
			return c.Next()
		}
		return c.Status(fiber.StatusUpgradeRequired).JSON(fiber.Map{
			"error": fiber.Map{"code": "UPGRADE_REQUIRED", "message": "WebSocket upgrade required", "status": 426},
		})
	}, middleware.NewAuthMiddleware(authCfg))
	app.Get("/ws/asr", wsHandler.Upgrade())

	// Deepgram-compatible streaming
	dgHandler := handlers.NewDeepgramHandler(sc, redactor, db.DB, cfg.EnableITN, logManager)
	app.Use("/v1/listen", func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			slog.InfoContext(c.UserContext(), "Deepgram WebSocket upgrade request", "path", "/v1/listen", "remote", c.IP())
			return c.Next()
		}
		return c.Status(fiber.StatusUpgradeRequired).JSON(fiber.Map{
			"error": fiber.Map{"code": "UPGRADE_REQUIRED", "message": "WebSocket upgrade required", "status": 426},
		})
	}, middleware.NewAuthMiddleware(authCfg))
	app.Get("/v1/listen", dgHandler.Upgrade())

	// Deepgram-compatible REAL-TIME streaming (true streaming ASR).
	// Legacy /v1/listen → buffered /stream route is untouched above.
	app.Use("/v2/listen", func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			slog.InfoContext(c.UserContext(), "Deepgram-realtime WebSocket upgrade request", "path", "/v2/listen", "remote", c.IP())
			return middleware.NewAuthMiddleware(authCfg)(c)
		}
		return c.Status(fiber.StatusUpgradeRequired).JSON(fiber.Map{
			"error": fiber.Map{"code": "UPGRADE_REQUIRED", "message": "WebSocket upgrade required", "status": 426},
		})
	})
	app.Get("/v2/listen", deepgramRealtimeHandler.Upgrade())

	// OpenAI Realtime-style streaming (true streaming ASR).
	app.Use("/v1/realtime", func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			slog.InfoContext(c.UserContext(), "OpenAI Realtime WebSocket upgrade request", "path", "/v1/realtime", "remote", c.IP())
			return middleware.NewAuthMiddleware(authCfg)(c)
		}
		return c.Status(fiber.StatusUpgradeRequired).JSON(fiber.Map{
			"error": fiber.Map{"code": "UPGRADE_REQUIRED", "message": "WebSocket upgrade required", "status": 426},
		})
	})
	app.Get("/v1/realtime", openaiRealtimeHandler.Upgrade())

	// Watson-compatible streaming (WebSocket only — POST /v1/recognize is in the authed group below)
	app.Use("/v1/recognize", func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			slog.InfoContext(c.UserContext(), "Watson WebSocket upgrade request", "path", "/v1/recognize", "remote", c.IP())
			return c.Next()
		}
		// For non-WebSocket requests (POST), skip this middleware chain entirely
		// so the request reaches the authed group's POST handler.
		if c.Method() != fiber.MethodGet {
			return c.Next()
		}
		return c.Status(fiber.StatusUpgradeRequired).JSON(fiber.Map{
			"error": fiber.Map{"code": "UPGRADE_REQUIRED", "message": "WebSocket upgrade required", "status": 426},
		})
	})
	app.Use("/v1/recognize", func(c *fiber.Ctx) error {
		// Only apply auth middleware to WebSocket upgrades
		if websocket.IsWebSocketUpgrade(c) {
			return middleware.NewAuthMiddleware(authCfg)(c)
		}
		return c.Next()
	})
	app.Get("/v1/recognize", watsonHandler.Upgrade())

	// === Authenticated Routes ===
	authed := app.Group("",
		middleware.NewAuthMiddleware(authCfg),
		rateLimiter.Middleware(),
		usageTracker.Middleware(),
	)

	// Auth (authenticated)
	authed.Post("/api/v1/auth/logout", authHandler.Logout)

	// ASR
	authed.Post("/api/v1/asr", asrHandler.TranscribeFile)

	// Whisper-compatible
	authed.Get("/v1/models", handlers.ListModels)
	authed.Post("/v1/audio/transcriptions", whisperHandler.Transcriptions)

	// Watson-compatible
	authed.Post("/v1/recognize", watsonHandler.Recognize)

	// TTS
	authed.Post("/api/v1/tts", ttsHandler.Synthesize)

	// OpenAI-compatible TTS (POST /v1/audio/speech)
	authed.Post("/v1/audio/speech", openaiTTSHandler.Speech)

	// Voice Management
	authed.Post("/api/v1/voices/clone", voiceHandler.Clone)
	authed.Get("/api/v1/voices", voiceHandler.List)
	authed.Get("/api/v1/voices/:id", voiceHandler.Get)
	authed.Delete("/api/v1/voices/:id", voiceHandler.Delete)

	// Usage
	authed.Get("/api/v1/usage/summary", usageHandler.Summary)
	authed.Get("/api/v1/usage/history", usageHandler.History)
	authed.Get("/api/v1/usage/keys/:id", usageHandler.KeySummary)
	authed.Get("/api/v1/usage/me", usageHandler.MyUsage)

	// API Keys
	authed.Post("/api/v1/keys", keysHandler.Create)
	authed.Get("/api/v1/keys", keysHandler.List)
	authed.Delete("/api/v1/keys/:id", keysHandler.Revoke)

	// === Admin Routes (enterprise tier only) ===
	adminHandler := handlers.NewAdminHandler(db.DB)
	admin := app.Group("/api/v1/admin",
		middleware.NewAuthMiddleware(authCfg),
		func(c *fiber.Ctx) error {
			// Real-time DB check — admin flag is authoritative, not JWT claims
			userID, err := middleware.ParseUserID(c)
			if err != nil {
				return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
					"error": fiber.Map{
						"code":    "UNAUTHORIZED",
						"message": "Authentication required",
						"status":  401,
					},
				})
			}
			var user models.User
			if result := db.DB.Select("admin").First(&user, "id = ?", userID); result.Error != nil {
				return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
					"error": fiber.Map{
						"code":    "UNAUTHORIZED",
						"message": "User not found",
						"status":  401,
					},
				})
			}
			if !user.Admin {
				return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
					"error": fiber.Map{
						"code":    "FORBIDDEN",
						"message": "Admin access required",
						"status":  403,
					},
				})
			}
			return c.Next()
		},
	)

	// Users
	admin.Get("/users", adminHandler.ListUsers)
	admin.Post("/users", adminHandler.CreateUser)
	admin.Get("/users/:id", adminHandler.GetUser)
	admin.Put("/users/:id", adminHandler.UpdateUser)
	admin.Delete("/users/:id", adminHandler.DeleteUser)

	// User API keys (managed by admin)
	admin.Post("/users/:id/keys", adminHandler.CreateUserKey)
	admin.Get("/users/:id/keys", adminHandler.ListUserKeys)
	admin.Delete("/users/:id/keys/:keyId", adminHandler.RevokeUserKey)

	// Global usage
	admin.Get("/usage", adminHandler.GlobalUsageSummary)

	// === Graceful Shutdown ===
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if err := app.Listen(":" + cfg.Port); err != nil {
			slog.Error("server error", "error", err)
		}
	}()

	<-quit
	slog.Info("shutting down server")
	_ = app.Shutdown()
}
