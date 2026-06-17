package main

import (
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

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
	// calls in handlers automatically get correlated.
	logLevel := slog.LevelInfo
	if cfg.Environment == "development" {
		logLevel = slog.LevelDebug
	}
	baseHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})
	slog.SetDefault(slog.New(logging.NewContextHandler(baseHandler)))

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
	logManager := logging.NewLogManager(lokiClient, cfg.LokiEnabled)
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

	// Create dual-sidecar client:
	// Swift sidecar (ASR, VAD, diarization, TTS — CoreML/ANE) + Python sidecar (LLM — MLX)
	sc := sidecar.NewClient(
		cfg.SwiftSidecarURL, cfg.SwiftSidecarWSURL,
		cfg.LLMSidecarURL,
	)

	// Create Fiber app
	app := fiber.New(fiber.Config{
		AppName:     "GoTranscribeSrv",
		BodyLimit:   100 * 1024 * 1024, // 100MB
		ProxyHeader: "X-Forwarded-For", // Trust proxy headers from Caddy/nginx
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
	app.Use(fiberlogger.New(fiberlogger.Config{
		Format: "${time} | ${status} | ${latency} | ${method} ${path}\n",
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
	}

	// Create middleware
	rateLimiter := middleware.NewRateLimiter(cfg.RateLimitFree, cfg.RateLimitPro, cfg.RateLimitEnterprise)
	usageTracker := middleware.NewUsageTracker(db.DB, 1000, logManager)

	// Create handlers
	authHandler := handlers.NewAuthHandler(db.DB, authCfg, cfg.RegistrationEnabled)
	asrHandler := handlers.NewASRHandler(sc, cfg.EnableITN, logManager)
	whisperHandler := handlers.NewWhisperHandler(sc, cfg.EnableITN, logManager)
	voiceHandler := handlers.NewVoiceHandler(db.DB, sc, cfg.VoicesDataDir, logManager)
	ttsHandler := handlers.NewTTSHandler(sc, voiceHandler, logManager)
	usageHandler := handlers.NewUsageHandler(db.DB)
	keysHandler := handlers.NewKeysHandler(db.DB)
	processHandler := handlers.NewProcessHandler(sc, logManager)
	watsonHandler := handlers.NewWatsonHandler(sc, db.DB, cfg.EnableITN, logManager)

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
			slog.Info("WebSocket upgrade request", "path", "/ws/asr", "remote", c.IP())
			return c.Next()
		}
		return c.Status(fiber.StatusUpgradeRequired).JSON(fiber.Map{
			"error": fiber.Map{"code": "UPGRADE_REQUIRED", "message": "WebSocket upgrade required", "status": 426},
		})
	}, middleware.NewAuthMiddleware(authCfg))
	app.Get("/ws/asr", wsHandler.Upgrade())

	// Deepgram-compatible streaming
	dgHandler := handlers.NewDeepgramHandler(sc, db.DB, cfg.EnableITN, logManager)
	app.Use("/v1/listen", func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			slog.Info("Deepgram WebSocket upgrade request", "path", "/v1/listen", "remote", c.IP())
			return c.Next()
		}
		return c.Status(fiber.StatusUpgradeRequired).JSON(fiber.Map{
			"error": fiber.Map{"code": "UPGRADE_REQUIRED", "message": "WebSocket upgrade required", "status": 426},
		})
	}, middleware.NewAuthMiddleware(authCfg))
	app.Get("/v1/listen", dgHandler.Upgrade())

	// Watson-compatible streaming (WebSocket only — POST /v1/recognize is in the authed group below)
	app.Use("/v1/recognize", func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			slog.Info("Watson WebSocket upgrade request", "path", "/v1/recognize", "remote", c.IP())
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
	authed.Post("/v1/audio/transcriptions", whisperHandler.Transcriptions)

	// Watson-compatible
	authed.Post("/v1/recognize", watsonHandler.Recognize)

	// TTS
	authed.Post("/api/v1/tts", ttsHandler.Synthesize)

	// Voice Management
	authed.Post("/api/v1/voices/clone", voiceHandler.Clone)
	authed.Get("/api/v1/voices", voiceHandler.List)
	authed.Get("/api/v1/voices/:id", voiceHandler.Get)
	authed.Delete("/api/v1/voices/:id", voiceHandler.Delete)

	// LLM Transcript Processing
	authed.Post("/api/v1/process", processHandler.Process)
	authed.Get("/api/v1/process/tasks", processHandler.ListTasks)
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
