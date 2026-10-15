-- The panel's human agent is renamed from "you" to "admin": it is the
-- admin who signs in with the admin token and posts from the panel, so the
-- old id read oddly in transcripts. FK-safe order: insert the new row,
-- re-point every child row, drop leftovers, then delete the old row.
INSERT OR IGNORE INTO agents (id, display_name, kind, hostname, workdir, created_at, last_seen_at)
    SELECT 'admin', 'Admin', kind, hostname, workdir, created_at, last_seen_at
    FROM agents WHERE id = 'you';
UPDATE sessions SET created_by = 'admin' WHERE created_by = 'you';
UPDATE OR IGNORE participants SET agent_id = 'admin' WHERE agent_id = 'you';
UPDATE messages SET sender_id = 'admin' WHERE sender_id = 'you';
UPDATE context_items SET author_id = 'admin' WHERE author_id = 'you';
UPDATE agent_tokens SET agent_id = 'admin' WHERE agent_id = 'you';
UPDATE read_marks SET agent_id = 'admin' WHERE agent_id = 'you';
-- mentions are a JSON array of agent ids; the quoted token can only match
-- the standalone id, never an id that merely contains "you".
UPDATE messages SET mentions = replace(mentions, '"you"', '"admin"') WHERE mentions LIKE '%"you"%';
-- Participants that could not be re-pointed (an "admin" had already joined
-- that session): drop them rather than fail the rename.
DELETE FROM participants WHERE agent_id = 'you';
DELETE FROM agents WHERE id = 'you';
