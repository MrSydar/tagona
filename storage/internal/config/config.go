package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"
)

// Config holds the application configuration.
type Config struct {
	HTTPAddr               string
	PGDSN                  string
	S3Endpoint             string
	S3Region               string
	S3Bucket               string
	S3AccessKey            string
	S3SecretKey            string
	S3ForcePathStyle       bool
	TagEngineURL           string
	TagEngineTimeout       time.Duration
	DefaultLimit           int
	MaxLimit               int
	DefaultTTL             time.Duration
	MaxTagsPerQuery        int
	MaxObjectSizeBytes     int64
	RetentionSweepInterval time.Duration
	RetentionBatchSize     int
}

// Load loads configuration from environment variables with defaults.
func Load(prefix string) (*Config, error) {
	slog.Debug("Load", "prefix", prefix)
	cfg := &Config{
		HTTPAddr:               envOrDefault(prefix+"HTTP_ADDR", ":8082"),
		PGDSN:                  os.Getenv(prefix + "PG_DSN"),
		S3Endpoint:             os.Getenv(prefix + "S3_ENDPOINT"),
		S3Region:               envOrDefault(prefix+"S3_REGION", "us-east-1"),
		S3Bucket:               os.Getenv(prefix + "S3_BUCKET"),
		S3AccessKey:            os.Getenv(prefix + "S3_ACCESS_KEY"),
		S3SecretKey:            os.Getenv(prefix + "S3_SECRET_KEY"),
		S3ForcePathStyle:       true,
		TagEngineURL:           os.Getenv(prefix + "TAG_ENGINE_URL"),
		TagEngineTimeout:       30 * time.Second,
		DefaultLimit:           5,
		MaxLimit:               100,
		DefaultTTL:             0,
		MaxTagsPerQuery:        100,
		MaxObjectSizeBytes:     10 * 1024 * 1024,
		RetentionSweepInterval: 60 * time.Second,
		RetentionBatchSize:     100,
	}

	if v := os.Getenv(prefix + "S3_FORCE_PATH_STYLE"); v != "" {
		cfg.S3ForcePathStyle = v == "true" || v == "1"
	}

	if v := os.Getenv(prefix + "TAG_ENGINE_TIMEOUT"); v != "" {
		if sec, err := strconv.Atoi(v); err == nil {
			cfg.TagEngineTimeout = time.Duration(sec) * time.Second
		} else {
			var err error
			cfg.TagEngineTimeout, err = time.ParseDuration(v)
			if err != nil {
				return nil, fmt.Errorf("invalid TAG_ENGINE_TIMEOUT: %w", err)
			}
		}
	}

	if v := os.Getenv(prefix + "DEFAULT_LIMIT"); v != "" {
		var err error
		cfg.DefaultLimit, err = strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid DEFAULT_LIMIT: %w", err)
		}
	}

	if v := os.Getenv(prefix + "MAX_LIMIT"); v != "" {
		var err error
		cfg.MaxLimit, err = strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid MAX_LIMIT: %w", err)
		}
	}

	if v := os.Getenv(prefix + "DEFAULT_TTL"); v != "" {
		if sec, err := strconv.Atoi(v); err == nil {
			cfg.DefaultTTL = time.Duration(sec) * time.Second
		} else {
			var err error
			cfg.DefaultTTL, err = time.ParseDuration(v)
			if err != nil {
				return nil, fmt.Errorf("invalid DEFAULT_TTL: %w", err)
			}
		}
	}

	if v := os.Getenv(prefix + "MAX_TAGS_PER_QUERY"); v != "" {
		var err error
		cfg.MaxTagsPerQuery, err = strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid MAX_TAGS_PER_QUERY: %w", err)
		}
	}

	if v := os.Getenv(prefix + "MAX_OBJECT_SIZE_BYTES"); v != "" {
		var err error
		cfg.MaxObjectSizeBytes, err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid MAX_OBJECT_SIZE_BYTES: %w", err)
		}
	}

	if v := os.Getenv(prefix + "RETENTION_SWEEP_INTERVAL"); v != "" {
		var err error
		cfg.RetentionSweepInterval, err = time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid RETENTION_SWEEP_INTERVAL: %w", err)
		}
	}

	if v := os.Getenv(prefix + "RETENTION_BATCH_SIZE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("invalid RETENTION_BATCH_SIZE %q: must be a positive integer", v)
		}
		cfg.RetentionBatchSize = n
	}

	slog.Debug("Load: config loaded", "HTTPAddr", cfg.HTTPAddr, "S3Bucket", cfg.S3Bucket, "TagEngineURL", cfg.TagEngineURL)
	return cfg, nil
}

func envOrDefault(key, def string) string {
	slog.Debug("envOrDefault", "key", key, "default", def)
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
