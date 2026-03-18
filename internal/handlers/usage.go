package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/shaunagostinho/gotranscribesrv/internal/middleware"
	"github.com/shaunagostinho/gotranscribesrv/internal/models"
	"gorm.io/gorm"
)

// UsageHandler handles usage tracking routes.
type UsageHandler struct {
	db *gorm.DB
}

// NewUsageHandler creates a new UsageHandler.
func NewUsageHandler(db *gorm.DB) *UsageHandler {
	return &UsageHandler{db: db}
}

// Summary returns aggregated usage stats for the authenticated user.
// GET /api/v1/usage/summary
func (h *UsageHandler) Summary(c *fiber.Ctx) error {
	userID, err := middleware.ParseUserID(c)
	if err != nil {
		return fiber.ErrUnauthorized
	}

	period := c.Query("period", "month")
	var since time.Time
	switch period {
	case "day":
		since = time.Now().AddDate(0, 0, -1)
	case "week":
		since = time.Now().AddDate(0, 0, -7)
	default:
		period = "month"
		since = time.Now().AddDate(0, -1, 0)
	}

	var logs []models.UsageLog
	h.db.Where("user_id = ? AND created_at >= ?", userID, since).Find(&logs)

	summary := models.UsageSummary{
		Period:     period,
		ByEndpoint: make(map[string]models.EndpointUsage),
	}

	for _, log := range logs {
		summary.TotalRequests++
		summary.TotalAudioDurationSec += float64(log.AudioDuration) / 1000
		summary.TotalProcessTimeSec += float64(log.ProcessTime) / 1000

		ep := summary.ByEndpoint[log.Endpoint]
		ep.Requests++
		ep.AudioDurationSec += float64(log.AudioDuration) / 1000
		summary.ByEndpoint[log.Endpoint] = ep
	}

	return c.JSON(summary)
}

// History returns paginated usage log entries.
// GET /api/v1/usage/history
func (h *UsageHandler) History(c *fiber.Ctx) error {
	userID, err := middleware.ParseUserID(c)
	if err != nil {
		return fiber.ErrUnauthorized
	}

	page := c.QueryInt("page", 1)
	limit := c.QueryInt("limit", 20)
	if limit > 100 {
		limit = 100
	}
	if page < 1 {
		page = 1
	}
	endpoint := c.Query("endpoint")

	var total int64
	query := h.db.Model(&models.UsageLog{}).Where("user_id = ?", userID)
	if endpoint != "" {
		query = query.Where("endpoint = ?", endpoint)
	}
	query.Count(&total)

	var logs []models.UsageLog
	query.Order("created_at DESC").
		Offset((page - 1) * limit).
		Limit(limit).
		Find(&logs)

	pages := int(math.Ceil(float64(total) / float64(limit)))

	return c.JSON(models.UsageHistoryResponse{
		Items: logs,
		Total: total,
		Page:  page,
		Pages: pages,
	})
}

// KeysHandler handles API key CRUD.
type KeysHandler struct {
	db *gorm.DB
}

// NewKeysHandler creates a new KeysHandler.
func NewKeysHandler(db *gorm.DB) *KeysHandler {
	return &KeysHandler{db: db}
}

// Create generates a new API key.
// POST /api/v1/keys
func (h *KeysHandler) Create(c *fiber.Ctx) error {
	userID, err := middleware.ParseUserID(c)
	if err != nil {
		return fiber.ErrUnauthorized
	}

	var req models.CreateKeyRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "INVALID_INPUT",
				"message": "Invalid request body",
				"status":  422,
			},
		})
	}

	// Generate a random API key
	rawKey := generateAPIKey()
	hash := sha256.Sum256([]byte(rawKey))
	keyHash := hex.EncodeToString(hash[:])

	apiKey := models.APIKey{
		UserID:  userID,
		KeyHash: keyHash,
		Label:   req.Label,
		Scopes:  req.Scopes,
		Active:  true,
	}

	if result := h.db.Create(&apiKey); result.Error != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "CREATE_ERROR",
				"message": "Failed to create API key",
				"status":  500,
			},
		})
	}

	return c.Status(fiber.StatusCreated).JSON(models.CreateKeyResponse{
		ID:        apiKey.ID,
		Key:       rawKey,
		Label:     apiKey.Label,
		Scopes:    req.Scopes,
		CreatedAt: apiKey.CreatedAt,
	})
}

// List returns all API keys for the current user.
// GET /api/v1/keys
func (h *KeysHandler) List(c *fiber.Ctx) error {
	userID, err := middleware.ParseUserID(c)
	if err != nil {
		return fiber.ErrUnauthorized
	}

	var keys []models.APIKey
	h.db.Where("user_id = ? AND active = true", userID).Find(&keys)
	return c.JSON(keys)
}

// Revoke deactivates an API key.
// DELETE /api/v1/keys/:id
func (h *KeysHandler) Revoke(c *fiber.Ctx) error {
	userID, err := middleware.ParseUserID(c)
	if err != nil {
		return fiber.ErrUnauthorized
	}

	keyID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "INVALID_ID",
				"message": "Invalid key ID",
				"status":  400,
			},
		})
	}

	now := time.Now()
	result := h.db.Model(&models.APIKey{}).
		Where("id = ? AND user_id = ?", keyID, userID).
		Updates(map[string]interface{}{
			"active":     false,
			"revoked_at": &now,
		})

	if result.RowsAffected == 0 {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "NOT_FOUND",
				"message": "API key not found",
				"status":  404,
			},
		})
	}

	return c.JSON(fiber.Map{"message": "API key revoked"})
}

func generateAPIKey() string {
	bytes := make([]byte, 32)
	_, _ = rand.Read(bytes)
	return "gtx_live_" + hex.EncodeToString(bytes)
}
