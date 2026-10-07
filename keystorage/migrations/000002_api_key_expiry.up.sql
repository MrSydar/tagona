-- Optional expiry of a key. NULL means the key never expires. Idempotent: it runs on every start.
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;
-- The sweeper looks for expired keys; keys that never expire are not in the index.
CREATE INDEX IF NOT EXISTS idx_api_keys_expires_at ON api_keys (expires_at) WHERE expires_at IS NOT NULL;
