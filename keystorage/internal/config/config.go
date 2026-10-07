// Package config reads the keystorage settings from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"
)

var schemaRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Config holds the keystorage configuration.
type Config struct {
	HTTPAddr string
	// PGDSN connects as the keystorage role, which can reach only its own schema.
	PGDSN string
	// PGSchema is the schema holding the api_keys table; it becomes the connection's search_path.
	PGSchema string
	// AdminUsername and AdminPassword guard key management (HTTP Basic). When either is empty the
	// management endpoints are disabled; key validation is unaffected.
	AdminUsername string
	AdminPassword string

	// DefaultKeyTTL is the lifetime of a key created without a ttl_seconds; 0 means it never expires,
	// unless MaxKeyTTL is set, which then applies.
	DefaultKeyTTL time.Duration
	// MaxKeyTTL caps the ttl_seconds a key may be created with; 0 means no cap.
	MaxKeyTTL time.Duration
	// ExpiredKeyRetention is how long an expired key stays listed before it is deleted.
	ExpiredKeyRetention time.Duration
	// SweepInterval is how often expired keys past their retention are deleted.
	SweepInterval time.Duration
}

// Load reads the configuration with the given environment prefix (KEYSTORAGE_ in production).
func Load(prefix string) (*Config, error) {
	cfg := &Config{
		HTTPAddr:      envOrDefault(prefix+"HTTP_ADDR", ":8083"),
		PGDSN:         os.Getenv(prefix + "PG_DSN"),
		PGSchema:      envOrDefault(prefix+"PG_SCHEMA", "keys"),
		AdminUsername: os.Getenv(prefix + "ADMIN_USERNAME"),
		AdminPassword: os.Getenv(prefix + "ADMIN_PASSWORD"),
	}
	var errs []error
	for _, d := range []struct {
		name string
		dst  *time.Duration
		def  time.Duration
		min  time.Duration
	}{
		{"DEFAULT_KEY_TTL", &cfg.DefaultKeyTTL, 0, 0},
		{"MAX_KEY_TTL", &cfg.MaxKeyTTL, 0, 0},
		{"EXPIRED_KEY_RETENTION", &cfg.ExpiredKeyRetention, 24 * time.Hour, 0},
		{"SWEEP_INTERVAL", &cfg.SweepInterval, time.Minute, time.Second},
	} {
		v, err := durationEnv(prefix+d.name, d.def, d.min)
		if err != nil {
			errs = append(errs, err)
		}
		*d.dst = v
	}
	if cfg.MaxKeyTTL > 0 && cfg.DefaultKeyTTL > cfg.MaxKeyTTL {
		errs = append(errs, fmt.Errorf("%sDEFAULT_KEY_TTL (%s) exceeds %sMAX_KEY_TTL (%s)", prefix, cfg.DefaultKeyTTL, prefix, cfg.MaxKeyTTL))
	}
	if cfg.PGDSN == "" {
		errs = append(errs, fmt.Errorf("%sPG_DSN is required", prefix))
	}
	// The schema name is placed in a connection parameter, so keep it a plain identifier.
	if !schemaRe.MatchString(cfg.PGSchema) {
		errs = append(errs, fmt.Errorf("%sPG_SCHEMA %q is not a valid lowercase identifier", prefix, cfg.PGSchema))
	}
	return cfg, errors.Join(errs...)
}

// durationEnv reads a Go duration; unset gives def, and anything below min is an error.
func durationEnv(key string, def, min time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < min {
		return def, fmt.Errorf("%s: %q is not a duration of at least %s", key, v, min)
	}
	return d, nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
