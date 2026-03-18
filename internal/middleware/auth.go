package middleware

import (
	"crypto/sha256"
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
// It supports both Bearer token and X-API-Key header authentication.
func NewAuthMiddleware(cfg AuthConfig) fiber.Handler {
	return func(c *fiber.Ctx) error {
		// Check for API key first
		apiKey := c.Get("X-API-Key")
		if apiKey != "" {
			return authenticateAPIKey(c, cfg.DB, apiKey)
		}

		// Fall through to JWT
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
	c.Locals("email", apiKey.User.Email)
	c.Locals("tier", apiKey.User.Tier)
	return c.Next()
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
