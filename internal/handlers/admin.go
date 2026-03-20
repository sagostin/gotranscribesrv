package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/shaunagostinho/gotranscribesrv/internal/models"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// AdminHandler handles admin-only management routes.
type AdminHandler struct {
	db *gorm.DB
}

// NewAdminHandler creates a new AdminHandler.
func NewAdminHandler(db *gorm.DB) *AdminHandler {
	return &AdminHandler{db: db}
}

// === User Management ===

// ListUsers returns all users (admin only).
// GET /api/v1/admin/users
func (h *AdminHandler) ListUsers(c *fiber.Ctx) error {
	page := c.QueryInt("page", 1)
	limit := c.QueryInt("limit", 20)
	if limit > 100 {
		limit = 100
	}
	if page < 1 {
		page = 1
	}

	var total int64
	h.db.Model(&models.User{}).Count(&total)

	var users []models.User
	h.db.Order("created_at DESC").
		Offset((page - 1) * limit).
		Limit(limit).
		Find(&users)

	pages := int(math.Ceil(float64(total) / float64(limit)))

	return c.JSON(fiber.Map{
		"items": users,
		"total": total,
		"page":  page,
		"pages": pages,
	})
}

// CreateUser creates a new user (admin only).
// POST /api/v1/admin/users
func (h *AdminHandler) CreateUser(c *fiber.Ctx) error {
	type createUserReq struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Tier     string `json:"tier"`
		Admin    bool   `json:"admin"`
	}

	var req createUserReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{"code": "INVALID_INPUT", "message": "Invalid request body", "status": 422},
		})
	}

	if req.Email == "" || len(req.Password) < 8 {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{"code": "VALIDATION_ERROR", "message": "Email required, password min 8 chars", "status": 422},
		})
	}

	if req.Tier == "" {
		req.Tier = "free"
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": fiber.Map{"code": "INTERNAL_ERROR", "message": "Failed to hash password", "status": 500},
		})
	}

	user := models.User{
		Email:    req.Email,
		Password: string(hash),
		Tier:     req.Tier,
		Admin:    req.Admin,
	}

	if result := h.db.Create(&user); result.Error != nil {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error": fiber.Map{"code": "EMAIL_EXISTS", "message": "Email already in use", "status": 409},
		})
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"id":         user.ID,
		"email":      user.Email,
		"tier":       user.Tier,
		"created_at": user.CreatedAt,
	})
}

// GetUser returns a specific user by ID (admin only).
// GET /api/v1/admin/users/:id
func (h *AdminHandler) GetUser(c *fiber.Ctx) error {
	userID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": fiber.Map{"code": "INVALID_ID", "message": "Invalid user ID", "status": 400},
		})
	}

	var user models.User
	if result := h.db.Preload("APIKeys").First(&user, "id = ?", userID); result.Error != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": fiber.Map{"code": "NOT_FOUND", "message": "User not found", "status": 404},
		})
	}

	// Usage stats for this user
	var totalRequests int64
	h.db.Model(&models.UsageLog{}).Where("user_id = ?", userID).Count(&totalRequests)

	// Per-key usage counts
	type KeyUsageCount struct {
		APIKeyID     uuid.UUID `json:"api_key_id"`
		Label        string    `json:"label"`
		RequestCount int64     `json:"request_count"`
	}
	var keyUsage []KeyUsageCount
	h.db.Raw(`
		SELECT ak.id AS api_key_id, ak.label,
		       COUNT(ul.id) AS request_count
		FROM api_keys ak
		LEFT JOIN usage_logs ul ON ul.api_key_id = ak.id
		WHERE ak.user_id = ?
		GROUP BY ak.id, ak.label
		ORDER BY request_count DESC
	`, userID).Scan(&keyUsage)

	return c.JSON(fiber.Map{
		"user":           user,
		"api_keys":       user.APIKeys,
		"total_requests": totalRequests,
		"key_usage":      keyUsage,
	})
}

// UpdateUser updates a user's tier or email (admin only).
// PUT /api/v1/admin/users/:id
func (h *AdminHandler) UpdateUser(c *fiber.Ctx) error {
	userID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": fiber.Map{"code": "INVALID_ID", "message": "Invalid user ID", "status": 400},
		})
	}

	type updateReq struct {
		Email    string `json:"email,omitempty"`
		Tier     string `json:"tier,omitempty"`
		Password string `json:"password,omitempty"`
		Admin    *bool  `json:"admin,omitempty"`
	}

	var req updateReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{"code": "INVALID_INPUT", "message": "Invalid request body", "status": 422},
		})
	}

	updates := map[string]interface{}{}
	if req.Email != "" {
		updates["email"] = req.Email
	}
	if req.Tier != "" {
		updates["tier"] = req.Tier
	}
	if req.Password != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": fiber.Map{"code": "INTERNAL_ERROR", "message": "Failed to hash password", "status": 500},
			})
		}
		updates["password"] = string(hash)
	}
	if req.Admin != nil {
		updates["admin"] = *req.Admin
	}

	if len(updates) == 0 {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{"code": "NO_CHANGES", "message": "No fields to update", "status": 422},
		})
	}

	result := h.db.Model(&models.User{}).Where("id = ?", userID).Updates(updates)
	if result.RowsAffected == 0 {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": fiber.Map{"code": "NOT_FOUND", "message": "User not found", "status": 404},
		})
	}

	return c.JSON(fiber.Map{"message": "User updated"})
}

// DeleteUser soft-deletes a user (admin only).
// DELETE /api/v1/admin/users/:id
func (h *AdminHandler) DeleteUser(c *fiber.Ctx) error {
	userID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": fiber.Map{"code": "INVALID_ID", "message": "Invalid user ID", "status": 400},
		})
	}

	result := h.db.Delete(&models.User{}, "id = ?", userID)
	if result.RowsAffected == 0 {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": fiber.Map{"code": "NOT_FOUND", "message": "User not found", "status": 404},
		})
	}

	return c.JSON(fiber.Map{"message": "User deleted"})
}

// === API Key Management (for any user) ===

// CreateUserKey creates an API key for a specific user (admin only).
// POST /api/v1/admin/users/:id/keys
func (h *AdminHandler) CreateUserKey(c *fiber.Ctx) error {
	userID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": fiber.Map{"code": "INVALID_ID", "message": "Invalid user ID", "status": 400},
		})
	}

	// Verify user exists
	var user models.User
	if result := h.db.First(&user, "id = ?", userID); result.Error != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": fiber.Map{"code": "NOT_FOUND", "message": "User not found", "status": 404},
		})
	}

	type createKeyReq struct {
		Label  string   `json:"label"`
		Scopes []string `json:"scopes"`
	}

	var req createKeyReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{"code": "INVALID_INPUT", "message": "Invalid request body", "status": 422},
		})
	}

	// Generate API key
	rawKey := generateAdminAPIKey()
	hash := sha256.Sum256([]byte(rawKey))

	apiKey := models.APIKey{
		UserID:  userID,
		KeyHash: hex.EncodeToString(hash[:]),
		Label:   req.Label,
		Scopes:  req.Scopes,
		Active:  true,
	}

	if result := h.db.Create(&apiKey); result.Error != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": fiber.Map{"code": "CREATE_ERROR", "message": "Failed to create API key", "status": 500},
		})
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"id":         apiKey.ID,
		"key":        rawKey,
		"label":      apiKey.Label,
		"scopes":     req.Scopes,
		"user_id":    userID,
		"user_email": user.Email,
		"created_at": apiKey.CreatedAt,
	})
}

// ListUserKeys lists all API keys for a specific user (admin only).
// GET /api/v1/admin/users/:id/keys
func (h *AdminHandler) ListUserKeys(c *fiber.Ctx) error {
	userID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": fiber.Map{"code": "INVALID_ID", "message": "Invalid user ID", "status": 400},
		})
	}

	var keys []models.APIKey
	h.db.Where("user_id = ?", userID).Find(&keys)
	return c.JSON(keys)
}

// RevokeUserKey revokes a specific API key (admin only).
// DELETE /api/v1/admin/users/:id/keys/:keyId
func (h *AdminHandler) RevokeUserKey(c *fiber.Ctx) error {
	keyID, err := uuid.Parse(c.Params("keyId"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": fiber.Map{"code": "INVALID_ID", "message": "Invalid key ID", "status": 400},
		})
	}

	now := time.Now()
	result := h.db.Model(&models.APIKey{}).
		Where("id = ?", keyID).
		Updates(map[string]interface{}{
			"active":     false,
			"revoked_at": &now,
		})

	if result.RowsAffected == 0 {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": fiber.Map{"code": "NOT_FOUND", "message": "API key not found", "status": 404},
		})
	}

	return c.JSON(fiber.Map{"message": "API key revoked"})
}

// === Admin Usage Overview ===

// GlobalUsageSummary returns aggregated usage across all users (admin only).
// GET /api/v1/admin/usage
func (h *AdminHandler) GlobalUsageSummary(c *fiber.Ctx) error {
	p := parsePeriod(c)

	var totalRequests int64
	query := h.db.Model(&models.UsageLog{})
	if !p.IsAll {
		query = query.Where("created_at >= ? AND created_at <= ?", p.Since, p.Until)
	}
	query.Count(&totalRequests)

	type UserStat struct {
		UserID       uuid.UUID `json:"user_id"`
		Email        string    `json:"email"`
		RequestCount int64     `json:"request_count"`
		AudioHours   float64   `json:"audio_hours"`
	}

	// Top users by request count
	var topUsers []UserStat
	if p.IsAll {
		h.db.Raw(`
			SELECT u.id AS user_id, u.email,
			       COUNT(ul.id) AS request_count,
			       COALESCE(SUM(ul.audio_duration), 0) / 3600000.0 AS audio_hours
			FROM users u
			LEFT JOIN usage_logs ul ON ul.user_id = u.id
			GROUP BY u.id, u.email
			ORDER BY request_count DESC
			LIMIT 20
		`).Scan(&topUsers)
	} else {
		h.db.Raw(`
			SELECT u.id AS user_id, u.email,
			       COUNT(ul.id) AS request_count,
			       COALESCE(SUM(ul.audio_duration), 0) / 3600000.0 AS audio_hours
			FROM users u
			LEFT JOIN usage_logs ul ON ul.user_id = u.id AND ul.created_at >= ? AND ul.created_at <= ?
			GROUP BY u.id, u.email
			ORDER BY request_count DESC
			LIMIT 20
		`, p.Since, p.Until).Scan(&topUsers)
	}

	// Total users
	var totalUsers int64
	h.db.Model(&models.User{}).Count(&totalUsers)

	return c.JSON(fiber.Map{
		"period":         p.Period,
		"from":           p.Since,
		"to":             p.Until,
		"total_requests": totalRequests,
		"total_users":    totalUsers,
		"top_users":      topUsers,
	})
}

func generateAdminAPIKey() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return "gtx_live_" + hex.EncodeToString(b)
}
