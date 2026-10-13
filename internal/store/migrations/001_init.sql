CREATE TABLE kinds (
    id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    created_at INTEGER NOT NULL
);

CREATE TABLE agents (
    id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    kind TEXT NOT NULL,
    hostname TEXT NOT NULL DEFAULT '',
    workdir TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    last_seen_at INTEGER
);

CREATE TABLE agent_tokens (
    token_hash TEXT PRIMARY KEY,
    agent_id TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL
);

CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    created_by TEXT NOT NULL REFERENCES agents (id),
    created_at INTEGER NOT NULL
);

CREATE TABLE participants (
    session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    agent_id TEXT NOT NULL REFERENCES agents (id),
    joined_at INTEGER NOT NULL,
    PRIMARY KEY (session_id, agent_id)
);

CREATE TABLE messages (
    global_seq INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    session_id TEXT NOT NULL REFERENCES sessions (id),
    sender_id TEXT NOT NULL REFERENCES agents (id),
    kind TEXT NOT NULL DEFAULT 'chat',
    content TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE INDEX idx_messages_session ON messages (session_id, global_seq);

CREATE TABLE context_items (
    session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    key TEXT NOT NULL,
    value TEXT NOT NULL,
    author_id TEXT NOT NULL REFERENCES agents (id),
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (session_id, key)
);
