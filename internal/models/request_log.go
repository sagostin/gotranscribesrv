package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// RequestLog tracks failed API requests (4xx/5xx) from authenticated users.
// Provides rough info for analytics; verbose detail goes to slog (console/Loki).
type RequestLog struct {
	ID        uuid.UUID  `json:"id" gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	UserID    *uuid.UUID `json:"user_id,omitempty" gorm:"type:uuid;index:idx_reqlog_user"`
	APIKeyID  *uuid.UUID `json:"api_key_id,omitempty" gorm:"type:uuid;index:idx_reqlog_apikey"`
	Endpoint  string     `json:"endpoint"`
	Method    string     `json:"method"`
	Status    int        `json:"status"`
	ErrorCode string     `json:"error_code"`
	IP        string     `json:"ip"`
	CreatedAt time.Time  `json:"created_at" gorm:"index"`
}

// BeforeCreate generates a UUID if not already set.
func (r *RequestLog) BeforeCreate(tx *gorm.DB) error {
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	return nil
}
