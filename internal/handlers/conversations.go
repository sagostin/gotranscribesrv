package handlers

import (
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/shaunagostinho/gotranscribesrv/internal/logging"
	"github.com/shaunagostinho/gotranscribesrv/internal/middleware"
	"github.com/shaunagostinho/gotranscribesrv/internal/models"
	"gorm.io/gorm"
)

// ConversationsHandler implements the OpenAI Conversations API
// (/v1/conversations) — server-side conversation state for the Responses
// API. Conversations are owned by the authenticated user; items are stored
// verbatim (Responses-API item JSON) with a sequence number for ordering.
type ConversationsHandler struct {
	db *gorm.DB
	lm *logging.LogManager
}

// NewConversationsHandler constructs the handler.
func NewConversationsHandler(db *gorm.DB, lm *logging.LogManager) *ConversationsHandler {
	return &ConversationsHandler{db: db, lm: lm}
}

// userUUID extracts the authenticated user's UUID from Locals.
func userUUID(c *fiber.Ctx) uuid.UUID {
	userIDStr, _ := c.Locals("user_id").(string)
	userID, _ := uuid.Parse(userIDStr)
	return userID
}

// apiKeyUUID extracts the optional API key UUID from Locals.
func apiKeyUUID(c *fiber.Ctx) *uuid.UUID {
	apiKeyIDStr, _ := c.Locals("api_key_id").(string)
	if akID, err := uuid.Parse(apiKeyIDStr); err == nil {
		return &akID
	}
	return nil
}

// conversationJSON renders the OpenAI conversation object.
func conversationJSON(conv *models.Conversation) fiber.Map {
	var metadata interface{}
	if err := json.Unmarshal([]byte(conv.Metadata), &metadata); err != nil || metadata == nil {
		metadata = map[string]interface{}{}
	}
	return fiber.Map{
		"id":         conv.ID,
		"object":     "conversation",
		"created_at": conv.CreatedAt.Unix(),
		"metadata":   metadata,
	}
}

// findConversation loads a conversation scoped to the authenticated user.
func (h *ConversationsHandler) findConversation(c *fiber.Ctx, id string) (*models.Conversation, error) {
	var conv models.Conversation
	err := h.db.Where("id = ? AND user_id = ?", id, userUUID(c)).First(&conv).Error
	return &conv, err
}

// loadConversationItems returns the stored item payloads of a conversation,
// ordered by sequence. Used by the Responses handler to build history.
func loadConversationItems(db *gorm.DB, userID uuid.UUID, conversationID string) ([]json.RawMessage, error) {
	var conv models.Conversation
	if err := db.Where("id = ? AND user_id = ?", conversationID, userID).First(&conv).Error; err != nil {
		return nil, err
	}
	var rows []models.ConversationItem
	if err := db.Where("conversation_id = ?", conversationID).
		Order("seq ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		items = append(items, json.RawMessage(row.Item))
	}
	return items, nil
}

// appendConversationItems stores items at the tail of a conversation,
// assigning sequence numbers and injecting an id into items that lack one.
func appendConversationItems(db *gorm.DB, conversationID string, items []json.RawMessage) error {
	if len(items) == 0 {
		return nil
	}
	var maxSeq int64
	if err := db.Model(&models.ConversationItem{}).
		Where("conversation_id = ?", conversationID).
		Select("COALESCE(MAX(seq), 0)").Scan(&maxSeq).Error; err != nil {
		return err
	}
	for _, raw := range items {
		var item map[string]interface{}
		if err := json.Unmarshal(raw, &item); err != nil {
			continue // skip unparseable items rather than failing the turn
		}
		row := models.ConversationItem{
			ConversationID: conversationID,
			Seq:            maxSeq + 1,
		}
		if t, _ := item["type"].(string); t != "" {
			row.Type = t
		}
		// Reuse the item's own id when present; otherwise generate one and
		// inject it into the stored JSON so reads return a stable id.
		if id, _ := item["id"].(string); id != "" {
			row.ID = id
		} else {
			row.ID = models.NewPrefixedID("item")
			item["id"] = row.ID
		}
		itemJSON, err := json.Marshal(item)
		if err != nil {
			continue
		}
		row.Item = string(itemJSON)
		if err := db.Create(&row).Error; err != nil {
			return err
		}
		maxSeq = row.Seq
	}
	// Touch the conversation's updated_at.
	return db.Model(&models.Conversation{}).
		Where("id = ?", conversationID).
		Update("updated_at", time.Now()).Error
}

// logDBError ships a CONVERSATION_DB_ERROR event to Loki and returns the
// OpenAI-style 500 envelope for the client.
func (h *ConversationsHandler) logDBError(c *fiber.Ctx, op, conversationID string, err error) error {
	h.lm.SendLog(h.lm.BuildLog("CONVERSATION_DB_ERROR", "ConversationDBError", slog.LevelWarn, map[string]interface{}{
		"endpoint":        "llm_conversations",
		"operation":       op,
		"conversation_id": conversationID,
		"error":           errString(err),
		"request_id":      middleware.RequestIDFromCtx(c),
	}, err))
	return llmError(c, fiber.StatusInternalServerError, "server_error", "failed to "+op, "db_error")
}

// ── Conversation CRUD ─────────────────────────────────────────────────

type createConversationRequest struct {
	Items    []json.RawMessage `json:"items"`
	Metadata json.RawMessage   `json:"metadata"`
}

// Create handles POST /v1/conversations.
func (h *ConversationsHandler) Create(c *fiber.Ctx) error {
	var req createConversationRequest
	if len(c.Body()) > 0 {
		if err := json.Unmarshal(c.Body(), &req); err != nil {
			return llmError(c, fiber.StatusBadRequest, "invalid_request_error", "malformed JSON body")
		}
	}
	metadata := "{}"
	if len(req.Metadata) > 0 && string(req.Metadata) != "null" {
		metadata = string(req.Metadata)
	}
	conv := models.Conversation{
		UserID:   userUUID(c),
		APIKeyID: apiKeyUUID(c),
		Metadata: metadata,
	}
	if err := h.db.Create(&conv).Error; err != nil {
		return h.logDBError(c, "create conversation", "", err)
	}
	if err := appendConversationItems(h.db, conv.ID, req.Items); err != nil {
		return h.logDBError(c, "store conversation items", conv.ID, err)
	}
	h.lm.SendLog(h.lm.BuildLog("CONVERSATION_CREATED", "ConversationCreated", slog.LevelInfo, map[string]interface{}{
		"endpoint":        "llm_conversations",
		"conversation_id": conv.ID,
		"initial_items":   len(req.Items),
		"request_id":      middleware.RequestIDFromCtx(c),
	}))
	return c.Status(fiber.StatusOK).JSON(conversationJSON(&conv))
}

// Get handles GET /v1/conversations/:id.
func (h *ConversationsHandler) Get(c *fiber.Ctx) error {
	conv, err := h.findConversation(c, c.Params("id"))
	if err != nil {
		return llmError(c, fiber.StatusNotFound, "invalid_request_error", "Conversation not found")
	}
	return c.JSON(conversationJSON(conv))
}

// Update handles POST /v1/conversations/:id (metadata update).
func (h *ConversationsHandler) Update(c *fiber.Ctx) error {
	conv, err := h.findConversation(c, c.Params("id"))
	if err != nil {
		return llmError(c, fiber.StatusNotFound, "invalid_request_error", "Conversation not found")
	}
	var req struct {
		Metadata json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(c.Body(), &req); err != nil {
		return llmError(c, fiber.StatusBadRequest, "invalid_request_error", "malformed JSON body")
	}
	if len(req.Metadata) > 0 && string(req.Metadata) != "null" {
		conv.Metadata = string(req.Metadata)
		if err := h.db.Model(conv).Update("metadata", conv.Metadata).Error; err != nil {
			return h.logDBError(c, "update conversation", conv.ID, err)
		}
	}
	return c.JSON(conversationJSON(conv))
}

// Delete handles DELETE /v1/conversations/:id.
func (h *ConversationsHandler) Delete(c *fiber.Ctx) error {
	conv, err := h.findConversation(c, c.Params("id"))
	if err != nil {
		return llmError(c, fiber.StatusNotFound, "invalid_request_error", "Conversation not found")
	}
	if err := h.db.Where("conversation_id = ?", conv.ID).Delete(&models.ConversationItem{}).Error; err != nil {
		return h.logDBError(c, "delete conversation items", conv.ID, err)
	}
	if err := h.db.Delete(conv).Error; err != nil {
		return h.logDBError(c, "delete conversation", conv.ID, err)
	}
	return c.JSON(fiber.Map{
		"id":      conv.ID,
		"object":  "conversation.deleted",
		"deleted": true,
	})
}

// ── Items ─────────────────────────────────────────────────────────────

// ListItems handles GET /v1/conversations/:id/items?limit&after&order.
func (h *ConversationsHandler) ListItems(c *fiber.Ctx) error {
	conv, err := h.findConversation(c, c.Params("id"))
	if err != nil {
		return llmError(c, fiber.StatusNotFound, "invalid_request_error", "Conversation not found")
	}
	limit := c.QueryInt("limit", 20)
	if limit < 1 || limit > 100 {
		limit = 20
	}
	order := "ASC"
	if strings.EqualFold(c.Query("order"), "desc") {
		order = "DESC"
	}

	query := h.db.Where("conversation_id = ?", conv.ID)
	if after := c.Query("after"); after != "" {
		var cursor models.ConversationItem
		if err := h.db.Where("id = ? AND conversation_id = ?", after, conv.ID).First(&cursor).Error; err == nil {
			op := ">"
			if order == "DESC" {
				op = "<"
			}
			query = query.Where("seq "+op+" ?", cursor.Seq)
		}
	}

	var rows []models.ConversationItem
	if err := query.Order("seq " + order).Limit(limit + 1).Find(&rows).Error; err != nil {
		return h.logDBError(c, "list items", conv.ID, err)
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}

	data := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		data = append(data, json.RawMessage(row.Item))
	}
	resp := fiber.Map{
		"object":   "list",
		"data":     data,
		"has_more": hasMore,
	}
	if len(rows) > 0 {
		resp["first_id"] = rows[0].ID
		resp["last_id"] = rows[len(rows)-1].ID
	}
	return c.JSON(resp)
}

// CreateItems handles POST /v1/conversations/:id/items.
func (h *ConversationsHandler) CreateItems(c *fiber.Ctx) error {
	conv, err := h.findConversation(c, c.Params("id"))
	if err != nil {
		return llmError(c, fiber.StatusNotFound, "invalid_request_error", "Conversation not found")
	}
	var req struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(c.Body(), &req); err != nil || len(req.Items) == 0 {
		return llmError(c, fiber.StatusBadRequest, "invalid_request_error", "items is required")
	}
	if err := appendConversationItems(h.db, conv.ID, req.Items); err != nil {
		return h.logDBError(c, "store items", conv.ID, err)
	}
	return c.JSON(fiber.Map{
		"object":   "list",
		"data":     req.Items,
		"has_more": false,
	})
}

// GetItem handles GET /v1/conversations/:id/items/:itemID.
func (h *ConversationsHandler) GetItem(c *fiber.Ctx) error {
	conv, err := h.findConversation(c, c.Params("id"))
	if err != nil {
		return llmError(c, fiber.StatusNotFound, "invalid_request_error", "Conversation not found")
	}
	var row models.ConversationItem
	if err := h.db.Where("id = ? AND conversation_id = ?", c.Params("itemID"), conv.ID).
		First(&row).Error; err != nil {
		return llmError(c, fiber.StatusNotFound, "invalid_request_error", "Item not found")
	}
	c.Set("Content-Type", "application/json")
	return c.SendString(row.Item)
}

// DeleteItem handles DELETE /v1/conversations/:id/items/:itemID.
func (h *ConversationsHandler) DeleteItem(c *fiber.Ctx) error {
	conv, err := h.findConversation(c, c.Params("id"))
	if err != nil {
		return llmError(c, fiber.StatusNotFound, "invalid_request_error", "Conversation not found")
	}
	result := h.db.Where("id = ? AND conversation_id = ?", c.Params("itemID"), conv.ID).
		Delete(&models.ConversationItem{})
	if result.Error != nil {
		return h.logDBError(c, "delete item", conv.ID, result.Error)
	}
	if result.RowsAffected == 0 {
		return llmError(c, fiber.StatusNotFound, "invalid_request_error", "Item not found")
	}
	return c.JSON(fiber.Map{
		"id":      c.Params("itemID"),
		"object":  "conversation.item.deleted",
		"deleted": true,
	})
}
