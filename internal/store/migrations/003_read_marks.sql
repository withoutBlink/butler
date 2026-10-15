-- Per-agent read cursors: the highest message seq an agent has received in a
-- session (via wait long-poll batches, message fetches, or their own posts).
-- The panel renders "already read" marks from these cursors.
CREATE TABLE read_marks (
    session_id TEXT NOT NULL,
    agent_id   TEXT NOT NULL,
    last_seq   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (session_id, agent_id)
);
