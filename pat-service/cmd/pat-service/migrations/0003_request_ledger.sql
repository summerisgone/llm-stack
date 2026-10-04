-- ADR 0022 section 6: qos_events becomes the request ledger. One row per
-- chat-completion or embeddings request that reached the gateway, whatever
-- its outcome. Defaults describe the rows written before this migration:
-- every one was a successful response with parsed usage and no timing.
ALTER TABLE qos_events
 ADD COLUMN request_id text NOT NULL DEFAULT '',
 ADD COLUMN attempt integer NOT NULL DEFAULT 1,
 ADD COLUMN source text NOT NULL DEFAULT 'pat',
 ADD COLUMN outcome text NOT NULL DEFAULT 'ok',
 ADD COLUMN http_status integer,
 ADD COLUMN streaming boolean,
 ADD COLUMN usage_quality text NOT NULL DEFAULT 'actual',
 ADD COLUMN received_at timestamptz,
 ADD COLUMN dispatched_at timestamptz,
 ADD COLUMN first_output_at timestamptz,
 ADD COLUMN last_output_at timestamptz,
 ADD COLUMN finished_at timestamptz;
CREATE INDEX qos_events_created_idx ON qos_events (created_at);
