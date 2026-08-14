package middleware

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	jwtware "github.com/gofiber/contrib/jwt"
	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/shaunagostinho/gotranscribesrv/internal/logging"
	"github.com/shaunagostinho/gotranscribesrv/internal/metrics"
	"github.com/shaunagostinho/gotranscribesrv/internal/models"
	"gorm.io/gorm"
)

// AuthConfig holds JWT configuration.
type AuthConfig struct {
	Secret     string
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	DB         *gorm.DB
	// LogManager is optional. When non-nil, every failed auth attempt
	// emits an AUTH_FAILED event to Loki/stdout. The raw token, API
	// key, or password is NEVER included in the log payload.
	LogManager *logging.LogManager
}

// NewAuthMiddleware creates the JWT authentication middleware.
// It supports Bearer token, X-API-Key header, and query param authentication.
// Query param auth (via ?token=...) is required for WebSocket connections where
// custom headers cannot be set.
func NewAuthMiddleware(cfg AuthConfig) fiber.Handler {
	return func(c *fiber.Ctx) error {
		// 1. Check for API key headers:
		//    - X-API-Key   → native clients
		//    - xi-api-key  → ElevenLabs-compatible clients (their SDKs send
		//      this header on every request)
		if apiKey := c.Get("X-API-Key"); apiKey != "" {
			return authenticateAPIKey(c, cfg.DB, cfg.LogManager, apiKey)
		}
		if xiKey := c.Get("xi-api-key"); xiKey != "" {
			return authenticateAPIKey(c, cfg.DB, cfg.LogManager, xiKey)
		}

		// 2. Check Authorization header:
		//    - "Token <key>"  → Deepgram-compatible (always API key lookup)
		//    - "Bearer <key>" → Try API key first, fall through to JWT
		authHeader := c.Get("Authorization")
		if strings.HasPrefix(authHeader, "Token ") {
			// Deepgram format — always authenticate as API key
			tokenValue := strings.TrimPrefix(authHeader, "Token ")
			return authenticateAPIKey(c, cfg.DB, cfg.LogManager, tokenValue)
		}
		if strings.HasPrefix(authHeader, "Basic ") {
			// Watson format — "Basic base64(apikey:THE_KEY)"
			encoded := strings.TrimPrefix(authHeader, "Basic ")
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err == nil {
				parts := strings.SplitN(string(decoded), ":", 2)
				if len(parts) == 2 {
					// Use the password portion as the API key
					return authenticateAPIKey(c, cfg.DB, cfg.LogManager, parts[1])
				}
			}
			logAuthFailure(cfg.LogManager, c, "basic", "malformed_header")
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"error": fiber.Map{
					"code":    "UNAUTHORIZED",
					"message": "Invalid Basic auth credentials",
					"status":  401,
				},
			})
		}
		if strings.HasPrefix(authHeader, "Bearer ") {
			bearerValue := strings.TrimPrefix(authHeader, "Bearer ")
			// Try as API key first (any key format, not just gtx_*)
			if tryAuthenticateAPIKey(c, cfg.DB, cfg.LogManager, bearerValue) {
				return c.Next()
			}
			// Fall through — will be tried as JWT below
		}

		// 3. Check query param ?token=... (required for WebSocket, where headers
		//    cannot be set from browser clients)
		if qToken := c.Query("token"); qToken != "" {
			// Try as API key first (any format)
			if tryAuthenticateAPIKey(c, cfg.DB, cfg.LogManager, qToken) {
				return c.Next()
			}
			// Otherwise treat as JWT
			claims, err := ParseToken(qToken, cfg.Secret, cfg.DB)
			if err != nil {
				logAuthFailure(cfg.LogManager, c, "jwt_query", classifyTokenError(err))
				return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
					"error": fiber.Map{
						"code":    "UNAUTHORIZED",
						"message": "Invalid or expired token",
						"status":  401,
					},
				})
			}
			c.Locals("user_id", claims["sub"])
			c.Locals("email", claims["email"])
			c.Locals("tier", claims["tier"])
			return c.Next()
		}

		// 4. Check Sec-WebSocket-Protocol header (Deepgram browser auth pattern:
		//    client sets Sec-WebSocket-Protocol: token, <api_key>)
		if swp := c.Get("Sec-WebSocket-Protocol"); swp != "" {
			// Format: "token, <api_key>" — extract the key part
			parts := strings.SplitN(swp, ",", 2)
			if len(parts) == 2 {
				apiKeyVal := strings.TrimSpace(parts[1])
				if apiKeyVal != "" {
					return authenticateAPIKey(c, cfg.DB, cfg.LogManager, apiKeyVal)
				}
			}
			// Single value — try as API key directly
			return authenticateAPIKey(c, cfg.DB, cfg.LogManager, strings.TrimSpace(swp))
		}

		// 4. Fall through to JWT from Authorization header
		return jwtware.New(jwtware.Config{
			SigningKey: jwtware.SigningKey{Key: []byte(cfg.Secret)},
			ErrorHandler: func(c *fiber.Ctx, err error) error {
				metrics.RecordAuthAttempt("jwt", "failure")
				logAuthFailure(cfg.LogManager, c, "jwt", classifyJWTMiddlewareError(err))
				return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
					"error": fiber.Map{
						"code":    "UNAUTHORIZED",
						"message": "Invalid or missing authentication",
						"status":  401,
					},
				})
			},
			SuccessHandler: func(c *fiber.Ctx) error {
				token := c.Locals("user").(*jwt.Token)
				claims := token.Claims.(jwt.MapClaims)

				// Check token blacklist
				if tokenID, ok := claims["token_id"].(string); ok && tokenID != "" {
					var count int64
					cfg.DB.Model(&models.TokenBlacklist{}).Where("token_id = ?", tokenID).Count(&count)
					if count > 0 {
						metrics.RecordAuthAttempt("jwt", "failure")
						logAuthFailure(cfg.LogManager, c, "jwt", "blacklisted")
						return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
							"error": fiber.Map{
								"code":    "TOKEN_REVOKED",
								"message": "Token has been revoked",
								"status":  401,
							},
						})
					}
				}

				metrics.RecordAuthAttempt("jwt", "success")
				c.Locals("user_id", claims["sub"])
				c.Locals("email", claims["email"])
				c.Locals("tier", claims["tier"])
				return c.Next()
			},
		})(c)
	}
}

// authenticateAPIKey validates an API key against the database.
func authenticateAPIKey(c *fiber.Ctx, db *gorm.DB, lm *logging.LogManager, rawKey string) error {
	hash := sha256.Sum256([]byte(rawKey))
	keyHash := hex.EncodeToString(hash[:])

	var apiKey models.APIKey
	result := db.Where("key_hash = ? AND active = true", keyHash).
		Preload("User").
		First(&apiKey)

	if result.Error != nil {
		metrics.RecordAuthAttempt("api_key", "failure")
		logAuthFailure(lm, c, "api_key", "unknown_or_revoked")
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "INVALID_API_KEY",
				"message": "Invalid or revoked API key",
				"status":  401,
			},
		})
	}

	metrics.RecordAuthAttempt("api_key", "success")
	c.Locals("user_id", apiKey.UserID.String())
	c.Locals("api_key_id", apiKey.ID.String())
	c.Locals("email", apiKey.User.Email)
	c.Locals("tier", apiKey.User.Tier)
	c.Locals("admin", apiKey.User.Admin)
	return c.Next()
}

// tryAuthenticateAPIKey checks if the raw key is a valid API key and sets
// the user context if found. Returns true on success, false if not found.
// Unlike authenticateAPIKey, this does NOT write a 401 response on failure,
// allowing callers to fall through to other auth methods.
func tryAuthenticateAPIKey(c *fiber.Ctx, db *gorm.DB, lm *logging.LogManager, rawKey string) bool {
	hash := sha256.Sum256([]byte(rawKey))
	keyHash := hex.EncodeToString(hash[:])

	var apiKey models.APIKey
	result := db.Where("key_hash = ? AND active = true", keyHash).
		Preload("User").
		First(&apiKey)

	if result.Error != nil {
		// Don't emit an AUTH_FAILED here — this is a probe. The
		// caller will either continue with JWT (and emit its own
		// failure if that also fails) or 401 directly. Emitting
		// here would double-count every Bearer-as-API-key probe.
		return false
	}

	c.Locals("user_id", apiKey.UserID.String())
	c.Locals("api_key_id", apiKey.ID.String())
	c.Locals("email", apiKey.User.Email)
	c.Locals("tier", apiKey.User.Tier)
	c.Locals("admin", apiKey.User.Admin)
	return true
}

// logAuthFailure emits an AUTH_FAILED structured log event.
// SECURITY: the raw token, API key, or password is NEVER included
// — only the auth method, failure reason, IP, user agent, and
// request id. Operators correlate failed-auth patterns in Loki
// via {type="AUTH_FAILED"} and group by reason/method/path.
func logAuthFailure(lm *logging.LogManager, c *fiber.Ctx, method, reason string) {
	if lm == nil {
		return
	}
	fields := map[string]interface{}{
		"endpoint":    c.Path(),
		"method":      c.Method(),
		"auth_method": method,
		"reason":      reason,
		"ip":          c.IP(),
		"user_agent":  c.Get("User-Agent"),
		"request_id":  RequestIDFromCtx(c),
	}
	lm.SendLog(lm.BuildLog("AUTH_FAILED", "AuthFailed", slog.LevelWarn, fields, method, reason))
}

// classifyJWTMiddlewareError maps the fiber-jwt middleware's error
// to a short, stable reason string suitable for log labels.
func classifyJWTMiddlewareError(err error) string {
	if err == nil {
		return "unknown"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "missing") || strings.Contains(msg, "no token"):
		return "missing_token"
	case strings.Contains(msg, "expired"):
		return "expired"
	case strings.Contains(msg, "signature"):
		return "bad_signature"
	case strings.Contains(msg, "malformed"):
		return "malformed"
	default:
		return "invalid"
	}
}

// classifyTokenError maps a ParseToken error to a stable reason
// string. Mirrors classifyJWTMiddlewareError but operates on the
// errors returned by our ParseToken helper.
func classifyTokenError(err error) string {
	if err == nil {
		return "unknown"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "revoked"):
		return "blacklisted"
	case strings.Contains(msg, "expired"):
		return "expired"
	case strings.Contains(msg, "signature") || strings.Contains(msg, "signing"):
		return "bad_signature"
	case strings.Contains(msg, "unexpected signing method"):
		return "wrong_algorithm"
	default:
		return "invalid"
	}
}

// GenerateTokens creates a new JWT access/refresh token pair.
func GenerateTokens(cfg AuthConfig, user *models.User) (*models.AuthResponse, error) {
	now := time.Now()

	// Access token
	accessTokenID := uuid.New().String()
	accessClaims := jwt.MapClaims{
		"sub":      user.ID.String(),
		"email":    user.Email,
		"tier":     user.Tier,
		"token_id": accessTokenID,
		"exp":      now.Add(cfg.AccessTTL).Unix(),
		"iat":      now.Unix(),
	}
	accessToken := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims)
	accessStr, err := accessToken.SignedString([]byte(cfg.Secret))
	if err != nil {
		return nil, err
	}

	// Refresh token
	refreshClaims := jwt.MapClaims{
		"sub":      user.ID.String(),
		"token_id": uuid.New().String(),
		"exp":      now.Add(cfg.RefreshTTL).Unix(),
		"iat":      now.Unix(),
	}
	refreshToken := jwt.NewWithClaims(jwt.SigningMethodHS256, refreshClaims)
	refreshStr, err := refreshToken.SignedString([]byte(cfg.Secret))
	if err != nil {
		return nil, err
	}

	return &models.AuthResponse{
		AccessToken:  accessStr,
		RefreshToken: refreshStr,
		ExpiresIn:    int(cfg.AccessTTL.Seconds()),
		TokenType:    "Bearer",
	}, nil
}

// ParseUserID extracts the user UUID from the fiber context.
func ParseUserID(c *fiber.Ctx) (uuid.UUID, error) {
	raw, ok := c.Locals("user_id").(string)
	if !ok {
		return uuid.Nil, fiber.ErrUnauthorized
	}
	// Strip any extra whitespace
	raw = strings.TrimSpace(raw)
	return uuid.Parse(raw)
}

// ParseToken parses and validates a JWT token string.
// If a DB is provided, it also checks the token blacklist.
func ParseToken(tokenString, secret string, db ...*gorm.DB) (jwt.MapClaims, error) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token")
	}

	// Check token blacklist if DB is available
	if len(db) > 0 && db[0] != nil {
		if tokenID, ok := claims["token_id"].(string); ok && tokenID != "" {
			var count int64
			db[0].Model(&models.TokenBlacklist{}).Where("token_id = ?", tokenID).Count(&count)
			if count > 0 {
				return nil, fmt.Errorf("token has been revoked")
			}
		}
	}

	return claims, nil
}

// BlacklistToken adds a token ID to the blacklist.
func BlacklistToken(db *gorm.DB, tokenID string, userID uuid.UUID, expiresAt time.Time) error {
	entry := models.TokenBlacklist{
		TokenID:   tokenID,
		UserID:    userID,
		ExpiresAt: expiresAt,
	}
	return db.Create(&entry).Error
}

// CleanupBlacklist periodically removes expired entries from the token blacklist.
func CleanupBlacklist(db *gorm.DB) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		result := db.Where("expires_at < ?", time.Now()).Delete(&models.TokenBlacklist{})
		if result.RowsAffected > 0 {
			slog.Info("cleaned up expired blacklist entries", "count", result.RowsAffected)
		}
	}
}
