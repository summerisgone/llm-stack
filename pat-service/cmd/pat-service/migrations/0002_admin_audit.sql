-- ADR 0022 section 1-2: users are keyed by (issuer, sub). Existing rows are
-- backfilled with the configured issuer at startup (backfillIssuer).
ALTER TABLE personal_access_tokens ADD COLUMN owner_issuer text NOT NULL DEFAULT '';
ALTER TABLE qos_events ADD COLUMN owner_issuer text NOT NULL DEFAULT '';

-- ADR 0022 section 1: append-only journal of every mutating admin action
-- and every rejected attempt.
CREATE TABLE admin_audit_events (
 id bigserial PRIMARY KEY,
 occurred_at timestamptz NOT NULL DEFAULT now(),
 request_id text NOT NULL DEFAULT '',
 operation_id text NOT NULL DEFAULT '',
 parent_operation_id text NOT NULL DEFAULT '',
 actor_issuer text NOT NULL DEFAULT '',
 actor_subject text NOT NULL DEFAULT '',
 actor_name text NOT NULL DEFAULT '',
 service_identity text NOT NULL DEFAULT '',
 action text NOT NULL,
 target_type text NOT NULL DEFAULT '',
 target_id text NOT NULL DEFAULT '',
 source text NOT NULL CHECK (source IN ('ui', 'api', 'cli', 'worker')),
 reason text NOT NULL DEFAULT '',
 expected_revision bigint,
 applied_revision bigint,
 before_value jsonb,
 after_value jsonb,
 outcome text NOT NULL CHECK (outcome IN ('requested', 'denied', 'started', 'succeeded', 'failed', 'cancelled', 'rolled_back', 'manual_intervention_required')),
 error_code text NOT NULL DEFAULT '',
 error_message text NOT NULL DEFAULT '');
CREATE INDEX admin_audit_events_time_idx ON admin_audit_events (occurred_at DESC, id DESC);
CREATE INDEX admin_audit_events_actor_idx ON admin_audit_events (actor_subject, occurred_at DESC);
CREATE INDEX admin_audit_events_action_idx ON admin_audit_events (action, occurred_at DESC);
CREATE INDEX admin_audit_events_target_idx ON admin_audit_events (target_type, target_id, occurred_at DESC);
CREATE INDEX admin_audit_events_operation_idx ON admin_audit_events (operation_id) WHERE operation_id <> '';

-- pat-service connects as the table owner, so GRANTs alone cannot stop it
-- rewriting history; the triggers do. Retention cleanup (ADR 0022 section 6)
-- must add an explicit exception here for its separate maintenance role.
CREATE FUNCTION admin_audit_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'admin_audit_events is append-only' USING ERRCODE = 'insufficient_privilege';
END $$;
CREATE TRIGGER admin_audit_events_no_update_delete BEFORE UPDATE OR DELETE ON admin_audit_events
 FOR EACH ROW EXECUTE FUNCTION admin_audit_events_append_only();
CREATE TRIGGER admin_audit_events_no_truncate BEFORE TRUNCATE ON admin_audit_events
 FOR EACH STATEMENT EXECUTE FUNCTION admin_audit_events_append_only();
