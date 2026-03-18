package database

import (
	"fmt"
	"log/slog"

	"github.com/shaunagostinho/gotranscribesrv/internal/models"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DB wraps a GORM database connection.
type DB struct {
	*gorm.DB
}

// Connect creates a new GORM database connection.
func Connect(databaseURL string, isProd bool) (*DB, error) {
	logLevel := logger.Info
	if isProd {
		logLevel = logger.Warn
	}

	db, err := gorm.Open(postgres.Open(databaseURL), &gorm.Config{
		Logger: logger.Default.LogMode(logLevel),
	})
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get underlying sql.DB: %w", err)
	}

	sqlDB.SetMaxOpenConns(20)
	sqlDB.SetMaxIdleConns(5)

	slog.Info("connected to PostgreSQL via GORM")
	return &DB{db}, nil
}

// Migrate runs GORM auto-migrations for all models.
func (db *DB) Migrate() error {
	slog.Info("running database migrations")
	return db.AutoMigrate(
		&models.User{},
		&models.APIKey{},
		&models.UsageLog{},
	)
}

// Close shuts down the database connection.
func (db *DB) Close() error {
	sqlDB, err := db.DB.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
