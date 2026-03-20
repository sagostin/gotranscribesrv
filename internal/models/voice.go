package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Voice represents a stored voice clone or reference for TTS.
// Each voice belongs to a user and stores an extracted voice embedding
// that can be reused for synthesis without re-cloning.
type Voice struct {
	ID          uuid.UUID      `json:"id" gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	UserID      uuid.UUID      `json:"user_id" gorm:"type:uuid;not null;uniqueIndex:idx_voice_user_name"`
	Name        string         `json:"name" gorm:"not null;uniqueIndex:idx_voice_user_name"`
	Description string         `json:"description,omitempty"`
	FilePath    string         `json:"-" gorm:"not null"`      // Relative: {user_id}/{voice_id}.bin
	SizeBytes   int64          `json:"size_bytes"`             // Embedding file size
	SampleRate  int            `json:"sample_rate,omitempty"`  // Source audio sample rate
	DurationSec float64        `json:"duration_sec,omitempty"` // Source audio duration
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	DeletedAt   gorm.DeletedAt `json:"-" gorm:"index"`

	// Associations
	User User `json:"-" gorm:"foreignKey:UserID"`
}

// BeforeCreate generates a UUID if not already set.
func (v *Voice) BeforeCreate(tx *gorm.DB) error {
	if v.ID == uuid.Nil {
		v.ID = uuid.New()
	}
	return nil
}

// VoiceResponse is the API response for a single voice.
type VoiceResponse struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Type        string    `json:"type"` // "custom" or "system"
	SizeBytes   int64     `json:"size_bytes,omitempty"`
	DurationSec float64   `json:"duration_sec,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitempty"`
}

// ToResponse converts a Voice model to its API response.
func (v *Voice) ToResponse() VoiceResponse {
	return VoiceResponse{
		ID:          v.ID,
		Name:        v.Name,
		Description: v.Description,
		Type:        "custom",
		SizeBytes:   v.SizeBytes,
		DurationSec: v.DurationSec,
		CreatedAt:   v.CreatedAt,
	}
}
