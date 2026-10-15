-- Soft eviction for kicked agents. The agents row must survive a kick:
-- messages.sender_id and sessions.created_by reference agents without ON
-- DELETE CASCADE, so history has to keep resolving. Instead of deleting,
-- kicking marks the agent removed: listings, session participants and
-- mention resolution skip removed agents, and their tokens are revoked.
-- Re-registering the same identity (a live process restarting) clears the
-- flag and revives the agent.
ALTER TABLE agents ADD COLUMN removed_at INTEGER;
