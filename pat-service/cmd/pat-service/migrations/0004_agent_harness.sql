-- ADR 0025: detected agent harness and call role per ledger row. Rows
-- written before this migration keep '' (no prompt content is kept, so they
-- cannot be classified after the fact).
ALTER TABLE qos_events
 ADD COLUMN agent_harness text NOT NULL DEFAULT '',
 ADD COLUMN agent_role text NOT NULL DEFAULT '';
