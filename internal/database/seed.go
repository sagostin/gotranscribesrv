package database

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"math/big"

	"github.com/shaunagostinho/gotranscribesrv/internal/models"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

const (
	defaultAdminEmail = "admin@gotranscribesrv.local"
)

// SeedAdmin creates a default admin user and API key on first run.
// A random password is generated and printed to the console (never stored in cleartext).
func SeedAdmin(db *gorm.DB) {
	var count int64
	db.Model(&models.User{}).Count(&count)
	if count > 0 {
		return // Users already exist, skip seeding
	}

	slog.Info("══════════════════════════════════════════════════")
	slog.Info("  First run detected — creating admin user")
	slog.Info("══════════════════════════════════════════════════")

	// Generate random password
	password := generateRandomPassword(20)

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		slog.Error("failed to hash admin password", "error", err)
		return
	}

	// Create admin user
	admin := models.User{
		Email:    defaultAdminEmail,
		Password: string(hash),
		Tier:     "enterprise",
	}

	if result := db.Create(&admin); result.Error != nil {
		slog.Error("failed to create admin user", "error", result.Error)
		return
	}

	// Generate API key
	rawKey := generateSeedAPIKey()
	keyHash := sha256.Sum256([]byte(rawKey))

	apiKey := models.APIKey{
		UserID:  admin.ID,
		KeyHash: hex.EncodeToString(keyHash[:]),
		Label:   "default-admin-key",
		Scopes:  []string{"asr", "tts", "admin", "usage", "keys"},
		Active:  true,
	}

	if result := db.Create(&apiKey); result.Error != nil {
		slog.Error("failed to create admin API key", "error", result.Error)
		return
	}

	slog.Info("")
	slog.Info("  ✅ Admin user created")
	slog.Info("  ┌─────────────────────────────────────────────")
	slog.Info("  │ Email:    " + defaultAdminEmail)
	slog.Info("  │ Password: " + password)
	slog.Info("  │ Tier:     enterprise")
	slog.Info("  │")
	slog.Info("  │ API Key:  " + rawKey)
	slog.Info("  └─────────────────────────────────────────────")
	slog.Info("")
	slog.Info("  ⚠️  Save these credentials now — they are shown only once!")
	slog.Info("══════════════════════════════════════════════════")
}

func generateSeedAPIKey() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return "gtx_live_" + hex.EncodeToString(b)
}

// generateRandomPassword creates a random alphanumeric password.
func generateRandomPassword(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$%"
	result := make([]byte, length)
	for i := range result {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		result[i] = charset[n.Int64()]
	}
	return string(result)
}
