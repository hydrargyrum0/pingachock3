ALTER TABLE nodes ADD COLUMN is_virtual boolean NOT NULL DEFAULT false;

-- At most one virtual node ever - it's a singleton (the backend representing
-- itself), not a category of nodes. See internal/serveragent.
CREATE UNIQUE INDEX idx_nodes_one_virtual ON nodes ((is_virtual)) WHERE is_virtual;
