package config

import "testing"

func TestRetentionBatchSize(t *testing.T) {
	t.Run("defaults to 100", func(t *testing.T) {
		cfg, err := Load("TAGONA_")
		if err != nil || cfg.RetentionBatchSize != 100 {
			t.Fatalf("got %v, %v; want 100", cfg, err)
		}
	})
	t.Run("is read from the environment", func(t *testing.T) {
		t.Setenv("TAGONA_RETENTION_BATCH_SIZE", "250")
		cfg, err := Load("TAGONA_")
		if err != nil || cfg.RetentionBatchSize != 250 {
			t.Fatalf("got %v, %v; want 250", cfg, err)
		}
	})
	for _, bad := range []string{"0", "-5", "abc", "1.5"} {
		t.Run("rejects "+bad, func(t *testing.T) {
			t.Setenv("TAGONA_RETENTION_BATCH_SIZE", bad)
			if _, err := Load("TAGONA_"); err == nil {
				t.Errorf("RETENTION_BATCH_SIZE=%q was accepted", bad)
			}
		})
	}
}
