package config

import (
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	t.Setenv("KS_PG_DSN", "postgres://x")
	cfg, err := Load("KS_")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8083" || cfg.PGSchema != "keys" || cfg.AdminUsername != "" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}

	t.Setenv("KS_HTTP_ADDR", ":9")
	t.Setenv("KS_PG_SCHEMA", "other_keys")
	t.Setenv("KS_ADMIN_USERNAME", "admin")
	t.Setenv("KS_ADMIN_PASSWORD", "pw")
	cfg, err = Load("KS_")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":9" || cfg.PGSchema != "other_keys" || cfg.AdminUsername != "admin" || cfg.AdminPassword != "pw" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	t.Run("missing dsn", func(t *testing.T) {
		if _, err := Load("KS_"); err == nil {
			t.Fatal("expected an error")
		}
	})
	for _, schema := range []string{"Keys", "keys;drop", "1keys", "a-b", "public,keys"} {
		t.Run("schema "+schema, func(t *testing.T) {
			t.Setenv("KS_PG_DSN", "postgres://x")
			t.Setenv("KS_PG_SCHEMA", schema)
			if _, err := Load("KS_"); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestKeyTTLSettings(t *testing.T) {
	t.Setenv("KS_PG_DSN", "postgres://x")
	cfg, err := Load("KS_")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultKeyTTL != 0 || cfg.MaxKeyTTL != 0 || cfg.ExpiredKeyRetention != 24*time.Hour || cfg.SweepInterval != time.Minute {
		t.Fatalf("defaults: %+v", cfg)
	}

	t.Setenv("KS_DEFAULT_KEY_TTL", "4h")
	t.Setenv("KS_MAX_KEY_TTL", "24h")
	t.Setenv("KS_EXPIRED_KEY_RETENTION", "0s")
	t.Setenv("KS_SWEEP_INTERVAL", "10s")
	cfg, err = Load("KS_")
	if err != nil || cfg.DefaultKeyTTL != 4*time.Hour || cfg.MaxKeyTTL != 24*time.Hour || cfg.ExpiredKeyRetention != 0 || cfg.SweepInterval != 10*time.Second {
		t.Fatalf("overrides: %+v, %v", cfg, err)
	}
}

func TestKeyTTLSettingsAreValidated(t *testing.T) {
	for name, kv := range map[string]map[string]string{
		"default above the cap": {"KS_DEFAULT_KEY_TTL": "48h", "KS_MAX_KEY_TTL": "24h"},
		"negative default":      {"KS_DEFAULT_KEY_TTL": "-1h"},
		"not a duration":        {"KS_MAX_KEY_TTL": "a day"},
		"negative retention":    {"KS_EXPIRED_KEY_RETENTION": "-1m"},
		"sweep interval zero":   {"KS_SWEEP_INTERVAL": "0s"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("KS_PG_DSN", "postgres://x")
			for k, v := range kv {
				t.Setenv(k, v)
			}
			if _, err := Load("KS_"); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	t.Run("a default equal to the cap is fine", func(t *testing.T) {
		t.Setenv("KS_PG_DSN", "postgres://x")
		t.Setenv("KS_DEFAULT_KEY_TTL", "1h")
		t.Setenv("KS_MAX_KEY_TTL", "1h")
		if _, err := Load("KS_"); err != nil {
			t.Fatal(err)
		}
	})
}
