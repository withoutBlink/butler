-- The complete butler schema. There is no migration history: butler is
-- pre-release, so this file is the single source of truth and is applied
-- with CREATE ... IF NOT EXISTS on every open. Changing the schema means
-- editing this file and recreating the database (`butler reset`); a database
-- created by an older schema is not upgraded in place.

CREATE TABLE IF NOT EXISTS kinds (
    id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    created_at INTEGER NOT NULL
);

-- Agents are identified by a random UUID (ids.UUID): the id is the identity,
-- display_name is the human-readable label (an OpenCode session title for
-- roles, the uuid itself otherwise) and can change without affecting the id.
-- client_session separates the app-level agent (empty) from its per-session
-- siblings; registration key = (kind, hostname, workdir, client_session).
-- removed_at soft-evicts an identity (kick / revoke): tokens revoked and
-- hidden from listings, participants and mentions, row kept for history.
CREATE TABLE IF NOT EXISTS agents (
    id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    kind TEXT NOT NULL,
    hostname TEXT NOT NULL DEFAULT '',
    workdir TEXT NOT NULL DEFAULT '',
    client_session TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    last_seen_at INTEGER,
    removed_at INTEGER
);

CREATE TABLE IF NOT EXISTS agent_tokens (
    token_hash TEXT PRIMARY KEY,
    agent_id TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    created_by TEXT NOT NULL REFERENCES agents (id),
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS participants (
    session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    agent_id TEXT NOT NULL REFERENCES agents (id),
    joined_at INTEGER NOT NULL,
    PRIMARY KEY (session_id, agent_id)
);

CREATE TABLE IF NOT EXISTS messages (
    global_seq INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    session_id TEXT NOT NULL REFERENCES sessions (id),
    sender_id TEXT NOT NULL REFERENCES agents (id),
    kind TEXT NOT NULL DEFAULT 'chat',
    content TEXT NOT NULL,
    mentions TEXT NOT NULL DEFAULT '[]',
    created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_messages_session ON messages (session_id, global_seq);

CREATE TABLE IF NOT EXISTS context_items (
    session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    key TEXT NOT NULL,
    value TEXT NOT NULL,
    author_id TEXT NOT NULL REFERENCES agents (id),
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (session_id, key)
);

-- Per-agent per-session read cursor: highest global_seq the agent received.
CREATE TABLE IF NOT EXISTS read_marks (
    session_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    last_seq INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (session_id, agent_id)
);