package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// UsageLog represents a single tracked API usage event.
type UsageLog struct {
	ID            uuid.UUID  `json:"id" gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	UserID        uuid.UUID  `json:"user_id" gorm:"type:uuid;not null;index:idx_usage_user_created"`
	APIKeyID      *uuid.UUID `json:"api_key_id,omitempty" gorm:"type:uuid;index:idx_usage_apikey"`
	Endpoint      string     `json:"endpoint" gorm:"not null"`
	AudioDuration int        `json:"audio_duration_ms" gorm:"not null;column:audio_duration"`
	ProcessTime   int        `json:"processing_time_ms" gorm:"not null;column:process_time"`
	Diarized      bool       `json:"diarized" gorm:"default:false"`
	Metadata      string     `json:"metadata,omitempty" gorm:"type:jsonb;default:'{}'"`
	CreatedAt     time.Time  `json:"created_at" gorm:"index:idx_usage_user_created"`

	// Associations
	User   User    `json:"-" gorm:"foreignKey:UserID"`
	APIKey *APIKey `json:"-" gorm:"foreignKey:APIKeyID"`
}

// BeforeCreate generates a UUID if not already set.
func (u *UsageLog) BeforeCreate(tx *gorm.DB) error {
	if u.ID == uuid.Nil {
		u.ID = uuid.New()
	}
	return nil
}

// UsageSummary is the aggregated usage response.
type UsageSummary struct {
	Period                string                   `json:"period"`
	TotalRequests         int64                    `json:"total_requests"`
	TotalAudioDurationSec float64                  `json:"total_audio_duration_sec"`
	TotalProcessTimeSec   float64                  `json:"total_processing_time_sec"`
	ByEndpoint            map[string]EndpointUsage `json:"by_endpoint"`
	ByKey                 []KeyUsageSummary        `json:"by_key"`
}

// KeyUsageSummary holds per-API-key usage stats.
type KeyUsageSummary struct {
	KeyID                 uuid.UUID                `json:"key_id"`
	Label                 string                   `json:"label"`
	TotalRequests         int64                    `json:"total_requests"`
	TotalAudioDurationSec float64                  `json:"total_audio_duration_sec"`
	TotalProcessTimeSec   float64                  `json:"total_processing_time_sec"`
	ByEndpoint            map[string]EndpointUsage `json:"by_endpoint"`
}

// EndpointUsage holds per-endpoint stats.
type EndpointUsage struct {
	Requests         int64   `json:"requests"`
	AudioDurationSec float64 `json:"audio_duration_sec"`
}

// UsageHistoryResponse is the paginated usage history.
type UsageHistoryResponse struct {
	Items []UsageLog `json:"items"`
	Total int64      `json:"total"`
	Page  int        `json:"page"`
	Pages int        `json:"pages"`
}
