-- Messages record the participants they @-mention (resolved server-side at
-- insert time to agent ids): exact agent id, agent kind, or @all/@everyone.
ALTER TABLE messages ADD COLUMN mentions TEXT NOT NULL DEFAULT '[]';
