package config

import "testing"

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
