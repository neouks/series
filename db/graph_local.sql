-- Durable region maintenance. No raw payload or evidence is copied here.
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS hot_local_mode INT NOT NULL DEFAULT 0;
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS hot_local_cutoff BIGINT NOT NULL DEFAULT 0;
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS hot_local_phase INT NOT NULL DEFAULT 0;
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS hot_local_cursor BIGINT NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS graph_hot_changes (
 exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
 node_id BIGINT NOT NULL, ticket BIGINT NOT NULL DEFAULT nextval('graph_projection_generation'),
 children BOOLEAN NOT NULL DEFAULT false,PRIMARY KEY(exploration_id,node_id)
);
CREATE INDEX IF NOT EXISTS graph_hot_changes_queue ON graph_hot_changes(exploration_id,ticket,node_id);
CREATE TABLE IF NOT EXISTS graph_hot_region (
 exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
 node_id BIGINT NOT NULL,children BOOLEAN NOT NULL DEFAULT false,
 discovered BOOLEAN NOT NULL DEFAULT false,parent_cursor BIGINT NOT NULL DEFAULT 0,
 child_cursor BIGINT NOT NULL DEFAULT 0,PRIMARY KEY(exploration_id,node_id)
);
CREATE INDEX IF NOT EXISTS graph_hot_region_frontier ON graph_hot_region(exploration_id,node_id) WHERE NOT discovered;
CREATE OR REPLACE FUNCTION series_graph_hot_enqueue(e BIGINT,n BIGINT) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM explorations WHERE id=e) THEN RETURN; END IF;
 INSERT INTO graph_hot_changes(exploration_id,node_id,children)
 VALUES(e,n,EXISTS(SELECT 1 FROM exploration_nodes WHERE id=n AND exploration_id=e AND kind='intent')
 OR EXISTS(SELECT 1 FROM graph_node_projection WHERE node_id=n AND exploration_id=e AND kind='intent'))
 ON CONFLICT(exploration_id,node_id) DO UPDATE SET ticket=nextval('graph_projection_generation'),children=graph_hot_changes.children OR EXCLUDED.children;
END $$;

-- Logical changes and physical build identity are separate: edge churn must
-- not restart a paged full build. Rows are reusable, not one copy per revision.
ALTER TABLE graph_dependency_state ADD COLUMN IF NOT EXISTS full_revision BIGINT NOT NULL DEFAULT 1;
ALTER TABLE graph_dependency_state ADD COLUMN IF NOT EXISTS building_full_revision BIGINT NOT NULL DEFAULT 0;
ALTER TABLE graph_dependency_state ADD COLUMN IF NOT EXISTS projected_set BIGINT NOT NULL DEFAULT 0;
ALTER TABLE graph_dependency_state ADD COLUMN IF NOT EXISTS edge_upper BIGINT NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS graph_dependency_changes (
 node_id BIGINT NOT NULL REFERENCES graph_dependency_state(node_id) ON DELETE CASCADE,
 dependency_id BIGINT NOT NULL,PRIMARY KEY(node_id,dependency_id)
);
ALTER TABLE graph_dependency_changes ADD COLUMN IF NOT EXISTS ticket BIGINT NOT NULL DEFAULT nextval('graph_projection_generation');
ALTER TABLE graph_dependency_state ADD COLUMN IF NOT EXISTS patch_cutoff BIGINT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS graph_dependency_change_queue ON graph_dependency_changes(node_id,ticket,dependency_id);
CREATE OR REPLACE FUNCTION series_graph_dependency_patch(e BIGINT,n BIGINT,d BIGINT) RETURNS void LANGUAGE plpgsql AS $$
DECLARE g BIGINT;
BEGIN
 IF NOT EXISTS(SELECT 1 FROM exploration_nodes WHERE id=n AND exploration_id=e) THEN RETURN; END IF;
 PERFORM pg_advisory_xact_lock(e+8100000000);
 INSERT INTO graph_revisions(exploration_id) VALUES(e) ON CONFLICT DO NOTHING;
 SELECT dependency_generation INTO g FROM graph_revisions WHERE exploration_id=e;
 INSERT INTO graph_dependency_state(node_id,exploration_id,generation) VALUES(n,e,g)
 ON CONFLICT(node_id) DO UPDATE SET revision=graph_dependency_state.revision+1,work=true;
 INSERT INTO graph_dependency_changes(node_id,dependency_id) VALUES(n,d) ON CONFLICT(node_id,dependency_id) DO UPDATE SET ticket=nextval('graph_projection_generation');
END $$;

-- Older cached sets are readable only via live fallback until this task's
-- bounded bootstrap upgrades their format. New graphs start at format 2.
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS dependency_format INT NOT NULL DEFAULT 0;
ALTER TABLE graph_revisions ALTER COLUMN dependency_format SET DEFAULT 2;

-- Distinguish digest anchors from edge-derived dependencies so one edge patch
-- never needs to expand the digest's complete raw anchor array again.
ALTER TABLE graph_dependencies ADD COLUMN IF NOT EXISTS anchored BOOLEAN NOT NULL DEFAULT false;
