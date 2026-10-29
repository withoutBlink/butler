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

-- Tasks on the board. status is todo / in_progress / blocked / review / done.
-- priority is low / normal / high / urgent. assignee_id is the single owner
-- (NULL when nobody owns it yet). reviewer_id empty means the creator is the
-- reviewer. handoff_chain is a JSON array of agent ids, in order.
-- review_from_id is who owned the task when it last entered review, so a
-- reject can send it back. session_id optionally ties discussion to a room;
-- deleting that session drops the link (ON DELETE SET NULL) and leaves the task.
-- Columns added after the first board release are also applied to existing
-- databases by store.migrate (idempotent ALTER TABLE). A fresh database gets
-- them from this CREATE.
CREATE TABLE IF NOT EXISTS tasks (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'todo',
    priority TEXT NOT NULL DEFAULT 'normal',
    assignee_id TEXT REFERENCES agents (id),
    creator_id TEXT NOT NULL REFERENCES agents (id),
    reviewer_id TEXT REFERENCES agents (id),
    handoff_chain TEXT NOT NULL DEFAULT '[]',
    review_from_id TEXT REFERENCES agents (id),
    session_id TEXT REFERENCES sessions (id) ON DELETE SET NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks (status, updated_at);
CREATE INDEX IF NOT EXISTS idx_tasks_assignee ON tasks (assignee_id);

-- Progress log for a task. One row per comment; the board and get_task show
-- them in order. Deleting a task (no UI for it today) would take them too.
CREATE TABLE IF NOT EXISTS task_comments (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    author_id TEXT NOT NULL REFERENCES agents (id),
    body TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_task_comments_task ON task_comments (task_id, created_at);

-- Outbound webhook: one URL per agent, or one per kind. An agent URL wins
-- over the kind URL when both are set. Exactly one of agent_id / kind is
-- set. UNIQUE treats NULLs as distinct, so the unused column does not
-- collide across rows.
CREATE TABLE IF NOT EXISTS webhooks (
    id TEXT PRIMARY KEY,
    agent_id TEXT UNIQUE REFERENCES agents (id) ON DELETE CASCADE,
    kind TEXT UNIQUE REFERENCES kinds (id) ON DELETE CASCADE,
    url TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    CHECK ((agent_id IS NOT NULL AND kind IS NULL) OR (agent_id IS NULL AND kind IS NOT NULL))
);