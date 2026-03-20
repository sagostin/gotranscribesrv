package models

import (
	"time"

	"github.com/google/uuid"
)

// TokenBlacklist stores revoked JWT token IDs.
// When a user logs out, both their access and refresh token IDs are added here.
// Expired entries are periodically cleaned up.
type TokenBlacklist struct {
	ID        uuid.UUID `json:"id" gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	TokenID   string    `json:"token_id" gorm:"uniqueIndex;not null"`
	UserID    uuid.UUID `json:"user_id" gorm:"type:uuid;not null;index"`
	ExpiresAt time.Time `json:"expires_at" gorm:"not null;index"`
	CreatedAt time.Time `json:"created_at"`
}
