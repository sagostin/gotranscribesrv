package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"gorm.io/gorm"
)

// APIKey represents a user-generated API key for programmatic access.
type APIKey struct {
	ID        uuid.UUID      `json:"id" gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	UserID    uuid.UUID      `json:"user_id" gorm:"type:uuid;not null;index"`
	KeyHash   string         `json:"-" gorm:"not null"`
	Label     string         `json:"label"`
	Scopes    pq.StringArray `json:"scopes" gorm:"type:text[]"`
	Active    bool           `json:"active" gorm:"default:true"`
	CreatedAt time.Time      `json:"created_at"`
	RevokedAt *time.Time     `json:"revoked_at,omitempty"`

	// Association
	User User `json:"-" gorm:"foreignKey:UserID"`
}

// BeforeCreate generates a UUID if not already set.
func (k *APIKey) BeforeCreate(tx *gorm.DB) error {
	if k.ID == uuid.Nil {
		k.ID = uuid.New()
	}
	return nil
}

// CreateKeyRequest is the payload for generating a new API key.
type CreateKeyRequest struct {
	Label  string   `json:"label"`
	Scopes []string `json:"scopes"`
}

// CreateKeyResponse includes the raw key (shown only once).
type CreateKeyResponse struct {
	ID        uuid.UUID `json:"id"`
	Key       string    `json:"key"`
	Label     string    `json:"label"`
	Scopes    []string  `json:"scopes"`
	CreatedAt time.Time `json:"created_at"`
}
