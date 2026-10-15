-- Per-client-session sub-agents. One OpenCode app instance hosts many
-- sessions; the MCP server it spawns cannot tell them apart, so the relay
-- plugin (which can) registers one butler identity per OpenCode session as a
-- "sibling" of the app-level identity. client_session stores that
-- discriminator (the OpenCode session id); '' is the app-level identity
-- registered by butler-mcp, which behaves exactly as before.
ALTER TABLE agents ADD COLUMN client_session TEXT NOT NULL DEFAULT '';
