// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	APIHost            string
	APIPort            int
	DatabaseURL        string
	ValkeyURL          string
	StorageRoot        string
	DefaultQuotaBytes  int64
	MultipartChunkSize int64
	JWTSecret          string
}

func Load() (*Config, error) {
	c := &Config{
		APIHost:     getEnv("API_HOST", "0.0.0.0"),
		DatabaseURL: getEnv("DATABASE_URL", ""),
		ValkeyURL:   getEnv("VALKEY_URL", ""),
		StorageRoot: getEnv("STORAGE_ROOT", "/storage"),
		JWTSecret:   getEnv("JWT_SECRET", ""),
	}

	port, err := strconv.Atoi(getEnv("API_PORT", "3000"))
	if err != nil {
		return nil, fmt.Errorf("invalid API_PORT: %w", err)
	}
	c.APIPort = port

	// 100 MB free per account by default — admin overrides per approval,
	// and the quota_requests flow is the path to more space.
	quota, err := strconv.ParseInt(getEnv("DEFAULT_QUOTA_BYTES", "104857600"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid DEFAULT_QUOTA_BYTES: %w", err)
	}
	c.DefaultQuotaBytes = quota

	chunk, err := strconv.ParseInt(getEnv("MULTIPART_CHUNK_SIZE", "5242880"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid MULTIPART_CHUNK_SIZE: %w", err)
	}
	c.MultipartChunkSize = chunk

	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}

	return c, nil
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
