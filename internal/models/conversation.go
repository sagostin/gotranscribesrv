package models

import (
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Prefixed OpenAI-style ID, e.g. prefixedID("conv") -> "conv_9f3c...".
func prefixedID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

// NewPrefixedID returns a prefixed OpenAI-style ID (e.g. "item_...") for
// callers that need the id before a row is created.
func NewPrefixedID(prefix string) string { return prefixedID(prefix) }

// Conversation is a server-side conversation for the OpenAI Responses API
// (POST /v1/conversations). Owned by the user (and optionally the API key)
// that created it; items are stored verbatim in ConversationItem rows.
type Conversation struct {
	ID        string         `json:"id" gorm:"primaryKey"` // conv_...
	UserID    uuid.UUID      `json:"user_id" gorm:"type:uuid;not null;index:idx_conversation_user"`
	APIKeyID  *uuid.UUID     `json:"api_key_id,omitempty" gorm:"type:uuid;index"`
	Metadata  string         `json:"metadata" gorm:"type:jsonb;default:'{}'"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`

	Items []ConversationItem `json:"-" gorm:"foreignKey:ConversationID"`
}

// BeforeCreate generates the conv_ id if not already set.
func (c *Conversation) BeforeCreate(tx *gorm.DB) error {
	if c.ID == "" {
		c.ID = prefixedID("conv")
	}
	return nil
}

// ConversationItem is one Responses-API item (message, function_call,
// function_call_output, …) belonging to a conversation. Item is the
// verbatim JSON payload; Seq orders items within the conversation.
type ConversationItem struct {
	ID             string    `json:"id" gorm:"primaryKey"` // item_...
	ConversationID string    `json:"conversation_id" gorm:"not null;index:idx_convitem_conv_seq,priority:1"`
	Seq            int64     `json:"seq" gorm:"not null;index:idx_convitem_conv_seq,priority:2"`
	Type           string    `json:"type" gorm:"not null;default:''"`
	Item           string    `json:"item" gorm:"type:jsonb;not null"`
	CreatedAt      time.Time `json:"created_at"`

	Conversation Conversation `json:"-" gorm:"foreignKey:ConversationID"`
}

// BeforeCreate generates the item_ id if not already set.
func (i *ConversationItem) BeforeCreate(tx *gorm.DB) error {
	if i.ID == "" {
		i.ID = prefixedID("item")
	}
	return nil
}

// ResponseRecord persists one Responses-API response so later requests can
// chain off it via previous_response_id. Input/Output hold the verbatim
// Responses-API item arrays.
type ResponseRecord struct {
	ID                 string     `json:"id" gorm:"primaryKey"` // resp_...
	UserID             uuid.UUID  `json:"user_id" gorm:"type:uuid;not null;index:idx_response_user"`
	APIKeyID           *uuid.UUID `json:"api_key_id,omitempty" gorm:"type:uuid;index"`
	PreviousResponseID string     `json:"previous_response_id,omitempty" gorm:"index"`
	ConversationID     string     `json:"conversation_id,omitempty" gorm:"index"`
	Model              string     `json:"model" gorm:"not null"`
	Input              string     `json:"input" gorm:"type:jsonb;not null"`  // []item
	Output             string     `json:"output" gorm:"type:jsonb;not null"` // []item
	CreatedAt          time.Time  `json:"created_at"`
}
