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

func TestTaggerConcurrency(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg, err := Load("TAGONA_")
		if err != nil || cfg.QueryConcurrency != 4 || cfg.TagEngineMaxConcurrency != 16 {
			t.Fatalf("got %+v, %v", cfg, err)
		}
	})
	t.Run("are read from the environment", func(t *testing.T) {
		t.Setenv("TAGONA_QUERY_CONCURRENCY", "1")
		t.Setenv("TAGONA_TAG_ENGINE_MAX_CONCURRENCY", "64")
		cfg, err := Load("TAGONA_")
		if err != nil || cfg.QueryConcurrency != 1 || cfg.TagEngineMaxConcurrency != 64 {
			t.Fatalf("got %+v, %v", cfg, err)
		}
	})
	for _, name := range []string{"QUERY_CONCURRENCY", "TAG_ENGINE_MAX_CONCURRENCY"} {
		for _, bad := range []string{"0", "-2", "many", "1.5"} {
			t.Run("rejects "+name+"="+bad, func(t *testing.T) {
				t.Setenv("TAGONA_"+name, bad)
				if _, err := Load("TAGONA_"); err == nil {
					t.Errorf("%s=%q was accepted", name, bad)
				}
			})
		}
	}
}
