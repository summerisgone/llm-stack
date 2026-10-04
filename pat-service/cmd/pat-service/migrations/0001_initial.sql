-- Schema as created by the inline migrate() before numbered migrations.
-- IF NOT EXISTS keeps it a no-op on databases that already have it.
CREATE TABLE IF NOT EXISTS personal_access_tokens (
 id text PRIMARY KEY, owner_subject text NOT NULL, owner_name text NOT NULL DEFAULT '', token_hash bytea NOT NULL UNIQUE,
 token_prefix text NOT NULL, name text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz, revoked_at timestamptz, last_used_at timestamptz);
ALTER TABLE personal_access_tokens ADD COLUMN IF NOT EXISTS owner_name text NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS personal_access_tokens_owner_active_idx
 ON personal_access_tokens (owner_subject, created_at DESC) WHERE revoked_at IS NULL;
CREATE TABLE IF NOT EXISTS qos_events (
 id bigserial PRIMARY KEY, owner_subject text NOT NULL, owner_name text NOT NULL DEFAULT '',
 session_id text NOT NULL DEFAULT '', model text NOT NULL, band text NOT NULL,
 prompt_tokens bigint NOT NULL DEFAULT 0, cached_tokens bigint NOT NULL DEFAULT 0, completion_tokens bigint NOT NULL DEFAULT 0,
 cost_amount numeric(12,4) NOT NULL DEFAULT 0, created_at timestamptz NOT NULL DEFAULT now());
CREATE INDEX IF NOT EXISTS qos_events_owner_day_idx ON qos_events (owner_subject, created_at);
CREATE INDEX IF NOT EXISTS qos_events_owner_session_idx ON qos_events (owner_subject, session_id, created_at);
ALTER TABLE qos_events ADD COLUMN IF NOT EXISTS token_id text NOT NULL DEFAULT '';
ALTER TABLE qos_events ADD COLUMN IF NOT EXISTS token_name text NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS qos_events_token_idx ON qos_events (token_id);
ALTER TABLE personal_access_tokens ADD COLUMN IF NOT EXISTS issued_by text NOT NULL DEFAULT 'user';
