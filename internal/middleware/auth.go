package middleware

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	jwtware "github.com/gofiber/contrib/jwt"
	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/shaunagostinho/gotranscribesrv/internal/models"
	"gorm.io/gorm"
)

// AuthConfig holds JWT configuration.
type AuthConfig struct {
	Secret     string
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	DB         *gorm.DB
}

// NewAuthMiddleware creates the JWT authentication middleware.
// It supports Bearer token, X-API-Key header, and query param authentication.
// Query param auth (via ?token=...) is required for WebSocket connections where
// custom headers cannot be set.
func NewAuthMiddleware(cfg AuthConfig) fiber.Handler {
	return func(c *fiber.Ctx) error {
		// 1. Check for API key header (X-API-Key)
		apiKey := c.Get("X-API-Key")
		if apiKey != "" {
			return authenticateAPIKey(c, cfg.DB, apiKey)
		}

		// 2. Check Authorization header:
		//    - "Token <key>"  → Deepgram-compatible (always API key lookup)
		//    - "Bearer <key>" → Try API key first, fall through to JWT
		authHeader := c.Get("Authorization")
		if strings.HasPrefix(authHeader, "Token ") {
			// Deepgram format — always authenticate as API key
			tokenValue := strings.TrimPrefix(authHeader, "Token ")
			return authenticateAPIKey(c, cfg.DB, tokenValue)
		}
		if strings.HasPrefix(authHeader, "Basic ") {
			// Watson format — "Basic base64(apikey:THE_KEY)"
			encoded := strings.TrimPrefix(authHeader, "Basic ")
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err == nil {
				parts := strings.SplitN(string(decoded), ":", 2)
				if len(parts) == 2 {
					// Use the password portion as the API key
					return authenticateAPIKey(c, cfg.DB, parts[1])
				}
			}
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
			if tryAuthenticateAPIKey(c, cfg.DB, bearerValue) {
				return c.Next()
			}
			// Fall through — will be tried as JWT below
		}

		// 3. Check query param ?token=... (required for WebSocket, where headers
		//    cannot be set from browser clients)
		if qToken := c.Query("token"); qToken != "" {
			// Try as API key first (any format)
			if tryAuthenticateAPIKey(c, cfg.DB, qToken) {
				return c.Next()
			}
			// Otherwise treat as JWT
			claims, err := ParseToken(qToken, cfg.Secret)
			if err != nil {
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
					return authenticateAPIKey(c, cfg.DB, apiKeyVal)
				}
			}
			// Single value — try as API key directly
			return authenticateAPIKey(c, cfg.DB, strings.TrimSpace(swp))
		}

		// 4. Fall through to JWT from Authorization header
		return jwtware.New(jwtware.Config{
			SigningKey: jwtware.SigningKey{Key: []byte(cfg.Secret)},
			ErrorHandler: func(c *fiber.Ctx, err error) error {
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

				c.Locals("user_id", claims["sub"])
				c.Locals("email", claims["email"])
				c.Locals("tier", claims["tier"])
				return c.Next()
			},
		})(c)
	}
}

// authenticateAPIKey validates an API key against the database.
func authenticateAPIKey(c *fiber.Ctx, db *gorm.DB, rawKey string) error {
	hash := sha256.Sum256([]byte(rawKey))
	keyHash := hex.EncodeToString(hash[:])

	var apiKey models.APIKey
	result := db.Where("key_hash = ? AND active = true", keyHash).
		Preload("User").
		First(&apiKey)

	if result.Error != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "INVALID_API_KEY",
				"message": "Invalid or revoked API key",
				"status":  401,
			},
		})
	}

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
func tryAuthenticateAPIKey(c *fiber.Ctx, db *gorm.DB, rawKey string) bool {
	hash := sha256.Sum256([]byte(rawKey))
	keyHash := hex.EncodeToString(hash[:])

	var apiKey models.APIKey
	result := db.Where("key_hash = ? AND active = true", keyHash).
		Preload("User").
		First(&apiKey)

	if result.Error != nil {
		return false
	}

	c.Locals("user_id", apiKey.UserID.String())
	c.Locals("api_key_id", apiKey.ID.String())
	c.Locals("email", apiKey.User.Email)
	c.Locals("tier", apiKey.User.Tier)
	c.Locals("admin", apiKey.User.Admin)
	return true
}

// GenerateTokens creates a new JWT access/refresh token pair.
func GenerateTokens(cfg AuthConfig, user *models.User) (*models.AuthResponse, error) {
	now := time.Now()

	// Access token
	accessClaims := jwt.MapClaims{
		"sub":   user.ID.String(),
		"email": user.Email,
		"tier":  user.Tier,
		"exp":   now.Add(cfg.AccessTTL).Unix(),
		"iat":   now.Unix(),
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
func ParseToken(tokenString, secret string) (jwt.MapClaims, error) {
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
	return claims, nil
}
