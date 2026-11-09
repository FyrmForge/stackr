-- The password of the stackr_repl role standbys replicate as (encrypted).
-- Empty until the instance's first standby.
ALTER TABLE operated_instances ADD COLUMN repl_password TEXT NOT NULL DEFAULT '';
-- What the panel observes of a replica (State is what it wants): starting,
-- streaming or rebuild (its slot was lost: it has to be made again).
ALTER TABLE operated_members ADD COLUMN phase TEXT NOT NULL DEFAULT 'starting';
-- Whether the consumers on a replica's node were last deployed with it in the
-- read pool; differing from the pool now = a routing change still owed.
ALTER TABLE operated_members ADD COLUMN routed INTEGER NOT NULL DEFAULT 0;
