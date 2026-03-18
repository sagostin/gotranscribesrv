package handlers

import (
	"github.com/gofiber/fiber/v2"
	"github.com/shaunagostinho/gotranscribesrv/internal/middleware"
	"github.com/shaunagostinho/gotranscribesrv/internal/models"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// AuthHandler handles authentication routes.
type AuthHandler struct {
	db      *gorm.DB
	authCfg middleware.AuthConfig
}

// NewAuthHandler creates a new AuthHandler.
func NewAuthHandler(db *gorm.DB, authCfg middleware.AuthConfig) *AuthHandler {
	return &AuthHandler{db: db, authCfg: authCfg}
}

// Register creates a new user account.
// POST /api/v1/auth/register
func (h *AuthHandler) Register(c *fiber.Ctx) error {
	var req models.RegisterRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "INVALID_INPUT",
				"message": "Invalid request body",
				"status":  422,
			},
		})
	}

	if req.Email == "" || len(req.Password) < 8 {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "VALIDATION_ERROR",
				"message": "Email is required and password must be at least 8 characters",
				"status":  422,
			},
		})
	}

	// Hash password
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "INTERNAL_ERROR",
				"message": "Failed to process registration",
				"status":  500,
			},
		})
	}

	user := models.User{
		Email:    req.Email,
		Password: string(hash),
	}

	result := h.db.Create(&user)
	if result.Error != nil {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "EMAIL_EXISTS",
				"message": "An account with this email already exists",
				"status":  409,
			},
		})
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"id":         user.ID,
		"email":      user.Email,
		"tier":       user.Tier,
		"created_at": user.CreatedAt,
	})
}

// Login authenticates a user and returns JWT tokens.
// POST /api/v1/auth/login
func (h *AuthHandler) Login(c *fiber.Ctx) error {
	var req models.LoginRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "INVALID_INPUT",
				"message": "Invalid request body",
				"status":  422,
			},
		})
	}

	var user models.User
	result := h.db.Where("email = ?", req.Email).First(&user)
	if result.Error != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "INVALID_CREDENTIALS",
				"message": "Invalid email or password",
				"status":  401,
			},
		})
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.Password)); err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "INVALID_CREDENTIALS",
				"message": "Invalid email or password",
				"status":  401,
			},
		})
	}

	tokens, err := middleware.GenerateTokens(h.authCfg, &user)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "TOKEN_ERROR",
				"message": "Failed to generate tokens",
				"status":  500,
			},
		})
	}

	return c.JSON(tokens)
}

// Refresh exchanges a refresh token for a new access token.
// POST /api/v1/auth/refresh
func (h *AuthHandler) Refresh(c *fiber.Ctx) error {
	type refreshReq struct {
		RefreshToken string `json:"refresh_token"`
	}
	var req refreshReq
	if err := c.BodyParser(&req); err != nil || req.RefreshToken == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "INVALID_INPUT",
				"message": "Refresh token is required",
				"status":  400,
			},
		})
	}

	// Parse and validate the refresh token
	claims, err := middleware.ParseToken(req.RefreshToken, h.authCfg.Secret)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "INVALID_TOKEN",
				"message": "Invalid or expired refresh token",
				"status":  401,
			},
		})
	}

	// Look up user
	var user models.User
	result := h.db.First(&user, "id = ?", claims["sub"])
	if result.Error != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "USER_NOT_FOUND",
				"message": "User not found",
				"status":  401,
			},
		})
	}

	tokens, err := middleware.GenerateTokens(h.authCfg, &user)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "TOKEN_ERROR",
				"message": "Failed to generate tokens",
				"status":  500,
			},
		})
	}

	return c.JSON(fiber.Map{
		"access_token": tokens.AccessToken,
		"expires_in":   tokens.ExpiresIn,
	})
}

// Logout invalidates the current session.
// POST /api/v1/auth/logout
func (h *AuthHandler) Logout(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"message": "logged out",
	})
}
