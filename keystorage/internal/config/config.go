// Package config reads the keystorage settings from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
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
	if cfg.PGDSN == "" {
		errs = append(errs, fmt.Errorf("%sPG_DSN is required", prefix))
	}
	// The schema name is placed in a connection parameter, so keep it a plain identifier.
	if !schemaRe.MatchString(cfg.PGSchema) {
		errs = append(errs, fmt.Errorf("%sPG_SCHEMA %q is not a valid lowercase identifier", prefix, cfg.PGSchema))
	}
	return cfg, errors.Join(errs...)
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
