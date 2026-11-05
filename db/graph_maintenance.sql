-- Rebuildable projections only. No historical payloads, transcripts or evidence
-- are rewritten. Existing explorations are initialized in bounded runtime pages.
CREATE SEQUENCE IF NOT EXISTS graph_projection_generation;
CREATE TABLE IF NOT EXISTS graph_revisions (
 exploration_id BIGINT PRIMARY KEY REFERENCES explorations(id) ON DELETE CASCADE,
 content_rev BIGINT NOT NULL DEFAULT 1,
 topology_rev BIGINT NOT NULL DEFAULT 1,
 access_rev BIGINT NOT NULL DEFAULT 1,
 view_rev BIGINT NOT NULL DEFAULT 1,
 bootstrap_cursor BIGINT NOT NULL DEFAULT 0,
 initialized BOOLEAN NOT NULL DEFAULT false
);
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS generation BIGINT NOT NULL DEFAULT nextval('graph_projection_generation');
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS summary_cursor BIGINT NOT NULL DEFAULT 0;
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS content_tx BIGINT NOT NULL DEFAULT 0;
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS topology_tx BIGINT NOT NULL DEFAULT 0;
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS access_tx BIGINT NOT NULL DEFAULT 0;
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS view_tx BIGINT NOT NULL DEFAULT 0;
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS count_rev BIGINT NOT NULL DEFAULT 1;
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS count_tx BIGINT NOT NULL DEFAULT 0;
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS summary_attempted_at TIMESTAMPTZ;
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS hot_wave BIGINT NOT NULL DEFAULT 0;
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS hot_build_generation BIGINT NOT NULL DEFAULT 0;
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS hot_seed_cursor BIGINT NOT NULL DEFAULT 0;
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS hot_seed_done BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS hot_ready BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS retired_content BIGINT NOT NULL DEFAULT 0;
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS retired_topology BIGINT NOT NULL DEFAULT 0;
-- Reachability is built once per topology epoch, using durable bounded edge
-- cursors. Old epochs are ignored; a node has at most one reusable work row.
CREATE TABLE IF NOT EXISTS graph_hot_reach (
 exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
 node_id BIGINT NOT NULL, wave BIGINT NOT NULL,
 expand BOOLEAN NOT NULL DEFAULT true, expanded BOOLEAN NOT NULL DEFAULT false,
 edge_cursor BIGINT NOT NULL DEFAULT 0, child_cursor BIGINT NOT NULL DEFAULT 0,
 children_done BOOLEAN NOT NULL DEFAULT false,
 PRIMARY KEY(exploration_id,node_id)
);
CREATE INDEX IF NOT EXISTS graph_hot_frontier ON graph_hot_reach(exploration_id,wave,node_id) WHERE NOT expanded;
CREATE TABLE IF NOT EXISTS graph_node_projection (
 node_id BIGINT PRIMARY KEY,
 exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
 kind TEXT NOT NULL, state TEXT NOT NULL, summary TEXT NOT NULL DEFAULT '',
 confidence TEXT NOT NULL DEFAULT '', source_hash TEXT NOT NULL DEFAULT '', source_version BIGINT NOT NULL DEFAULT 1,
 changed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 hot BOOLEAN NOT NULL DEFAULT true, cold_since_at TIMESTAMPTZ
);
ALTER TABLE graph_node_projection ADD COLUMN IF NOT EXISTS source_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE graph_node_projection ADD COLUMN IF NOT EXISTS payload_asset_ids BIGINT[] NOT NULL DEFAULT '{}';
ALTER TABLE graph_node_projection ADD COLUMN IF NOT EXISTS asset_metadata_ready BOOLEAN NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS graph_projection_asset_backfill ON graph_node_projection(exploration_id,node_id) WHERE NOT asset_metadata_ready;
ALTER TABLE graph_node_projection DROP CONSTRAINT IF EXISTS graph_node_projection_node_id_fkey;
CREATE INDEX IF NOT EXISTS graph_projection_cold ON graph_node_projection(exploration_id,hot,cold_since_at,node_id);
CREATE TABLE IF NOT EXISTS graph_dirty_nodes (
 exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
 node_id BIGINT NOT NULL, propagate BOOLEAN NOT NULL DEFAULT true,
 PRIMARY KEY(exploration_id,node_id)
);
CREATE TABLE IF NOT EXISTS planner_working_state (
 exploration_id BIGINT PRIMARY KEY REFERENCES explorations(id) ON DELETE CASCADE,
 todos JSONB NOT NULL DEFAULT '[]', revision BIGINT NOT NULL DEFAULT 0
);
-- Agent-authored navigation only; never execution authorization or evidence.
-- Latest state per role/intent is bounded; original tool calls remain in audit.
CREATE TABLE IF NOT EXISTS agent_execution_checkpoints (
 exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
 role TEXT NOT NULL CHECK(role IN ('planner','worker')),
 intent_id BIGINT NOT NULL DEFAULT 0,
 version BIGINT NOT NULL CHECK(version>0),
 body JSONB NOT NULL,
 sources JSONB NOT NULL DEFAULT '[]',
 access_revision TEXT NOT NULL,
 context_stamp TEXT NOT NULL DEFAULT '',
 saved_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(exploration_id,role,intent_id),
 CHECK ((role='planner' AND intent_id=0) OR (role='worker' AND intent_id>0))
);
ALTER TABLE agent_execution_checkpoints ADD COLUMN IF NOT EXISTS context_stamp TEXT NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS graph_repair_seen (
 exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
 node_id BIGINT NOT NULL, wave BIGINT NOT NULL,
 PRIMARY KEY(exploration_id,node_id)
);
CREATE TABLE IF NOT EXISTS graph_count_cache (
 exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
 policy_key TEXT NOT NULL, revision TEXT NOT NULL, counts JSONB NOT NULL,
 PRIMARY KEY(exploration_id,policy_key)
);
-- Source edits can invalidate summary readability even when node counts and
-- asset links do not change. Probe exact provenance keys instead of scanning
-- every summary payload for each ordinary body update.
CREATE INDEX IF NOT EXISTS graph_digest_source_keys ON exploration_nodes USING gin ((payload->'source_versions'))
 WHERE kind='digest' AND state='active';

-- Derived delivery metadata, not evidence or execution authorization. Keep
-- immutable per-transaction entries until a successful planning watermark.
CREATE TABLE IF NOT EXISTS planner_change_log (
 exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
 generation BIGINT NOT NULL DEFAULT 0,
 revision BIGINT NOT NULL,
 node_id BIGINT NOT NULL DEFAULT 0,
 kind TEXT NOT NULL DEFAULT 'change',
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(exploration_id,revision,node_id)
);
ALTER TABLE planner_change_log ADD COLUMN IF NOT EXISTS generation BIGINT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS planner_changes_node_page_v2 ON planner_change_log(exploration_id,generation,node_id,revision);
CREATE TABLE IF NOT EXISTS planner_delivery_state (
 exploration_id BIGINT PRIMARY KEY REFERENCES explorations(id) ON DELETE CASCADE,
 generation BIGINT NOT NULL,
 acknowledged_revision BIGINT NOT NULL DEFAULT 0,
 graph_revision TEXT NOT NULL DEFAULT '',
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION series_graph_payload_assets(p JSONB) RETURNS BIGINT[] LANGUAGE sql IMMUTABLE AS $$
 SELECT COALESCE(array_agg(CASE WHEN value ~ '^[0-9]{1,18}$' THEN value::bigint ELSE 0 END),'{}'::bigint[])
 FROM jsonb_array_elements_text(CASE WHEN jsonb_typeof(p->'asset_ids')='array' THEN p->'asset_ids' ELSE '[]'::jsonb END)
$$;



CREATE OR REPLACE FUNCTION series_digest_sources_current(p JSONB) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT CASE WHEN NOT(p ? 'source_versions') THEN true
 WHEN jsonb_typeof(p->'source_versions')<>'object' THEN false
 WHEN p ? 'projection_generation' AND NOT EXISTS(SELECT 1 FROM graph_revisions r WHERE r.exploration_id=CASE WHEN p->>'source_exploration_id' ~ '^[0-9]{1,18}$' THEN (p->>'source_exploration_id')::bigint ELSE 0 END
 AND r.generation::text=p->>'projection_generation') THEN false ELSE
 NOT EXISTS(SELECT 1 FROM jsonb_each_text(p->'source_versions') v
 LEFT JOIN graph_node_projection n ON n.node_id=CASE WHEN v.key ~ '^[0-9]{1,18}$' THEN v.key::bigint ELSE 0 END
 WHERE n.node_id IS NULL OR n.source_version::text<>v.value
 OR (p ? 'source_fingerprints' AND p->'source_fingerprints'->>v.key IS DISTINCT FROM n.source_hash))
 END
$$;

CREATE OR REPLACE FUNCTION series_graph_touch(e BIGINT, n BIGINT DEFAULT 0, topology BOOLEAN DEFAULT false, access_change BOOLEAN DEFAULT false, derived BOOLEAN DEFAULT false)
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM explorations WHERE id=e) THEN RETURN; END IF;
 PERFORM pg_advisory_xact_lock(e+8100000000);
 INSERT INTO graph_revisions(exploration_id,content_tx,topology_tx,access_tx,view_tx)
 VALUES(e,CASE WHEN NOT derived THEN txid_current() ELSE 0 END,
 CASE WHEN topology AND NOT derived THEN txid_current() ELSE 0 END,
 CASE WHEN access_change THEN txid_current() ELSE 0 END,txid_current())
 ON CONFLICT(exploration_id) DO UPDATE SET
 content_rev=graph_revisions.content_rev+CASE WHEN NOT derived AND graph_revisions.content_tx<>txid_current() THEN 1 ELSE 0 END,
 topology_rev=graph_revisions.topology_rev+CASE WHEN topology AND NOT derived AND graph_revisions.topology_tx<>txid_current() THEN 1 ELSE 0 END,
 access_rev=graph_revisions.access_rev+CASE WHEN access_change AND graph_revisions.access_tx<>txid_current() THEN 1 ELSE 0 END,
 view_rev=graph_revisions.view_rev+CASE WHEN graph_revisions.view_tx<>txid_current() THEN 1 ELSE 0 END,
 content_tx=CASE WHEN NOT derived THEN txid_current() ELSE graph_revisions.content_tx END,
 topology_tx=CASE WHEN topology AND NOT derived THEN txid_current() ELSE graph_revisions.topology_tx END,
 access_tx=CASE WHEN access_change THEN txid_current() ELSE graph_revisions.access_tx END,
 view_tx=txid_current()
 WHERE graph_revisions.view_tx<>txid_current()
 OR (NOT derived AND graph_revisions.content_tx<>txid_current())
 OR (topology AND NOT derived AND graph_revisions.topology_tx<>txid_current())
 OR (access_change AND graph_revisions.access_tx<>txid_current());
 IF n>0 AND NOT derived THEN
  INSERT INTO graph_dirty_nodes(exploration_id,node_id,propagate) VALUES(e,n,topology)
  ON CONFLICT(exploration_id,node_id) DO UPDATE SET propagate=true WHERE NOT graph_dirty_nodes.propagate AND EXCLUDED.propagate;
 END IF;
 IF NOT derived THEN
  INSERT INTO planner_change_log(exploration_id,generation,revision,node_id,kind)
  SELECT e,generation,content_rev,greatest(n,0),COALESCE((
   SELECT CASE WHEN kind='intent' AND state IN ('done','blocked','exhausted','stopped') THEN 'done'
    WHEN kind IN ('goal','hint','finding') THEN kind ELSE 'change' END
   FROM exploration_nodes WHERE id=n AND exploration_id=e),'change')
  FROM graph_revisions WHERE exploration_id=e
  ON CONFLICT(exploration_id,revision,node_id) DO UPDATE SET generation=EXCLUDED.generation,kind=EXCLUDED.kind
  WHERE planner_change_log.generation<>EXCLUDED.generation;
 END IF;
END $$;

-- Direct dependency metadata is rebuildable. A ready row is valid only for its
-- exact source revision and rebuild generation. Partially built rows never
-- authorize a read; consumers fall back to the original adjacency in one query.
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS dependency_generation BIGINT NOT NULL DEFAULT nextval('graph_projection_generation');
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS dependency_cursor BIGINT NOT NULL DEFAULT 0;
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS dependency_initialized BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS dependency_work_cursor BIGINT NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS graph_dependency_state (
 node_id BIGINT PRIMARY KEY,
 exploration_id BIGINT NOT NULL REFERENCES graph_revisions(exploration_id) ON DELETE CASCADE,
 generation BIGINT NOT NULL, rule_version INT NOT NULL DEFAULT 1,
 revision BIGINT NOT NULL DEFAULT 1, projected_revision BIGINT NOT NULL DEFAULT 0,
 building_revision BIGINT NOT NULL DEFAULT 0,
 building_count BIGINT NOT NULL DEFAULT 0, projected_count BIGINT NOT NULL DEFAULT 0,
 edge_cursor BIGINT NOT NULL DEFAULT 0, anchor_cursor INT NOT NULL DEFAULT 0,
 edges_done BOOLEAN NOT NULL DEFAULT false, anchors_done BOOLEAN NOT NULL DEFAULT false,
 work BOOLEAN NOT NULL DEFAULT true
);
ALTER TABLE graph_dependency_state DROP CONSTRAINT IF EXISTS graph_dependency_state_node_id_fkey;
ALTER TABLE graph_dependency_state ADD COLUMN IF NOT EXISTS building_count BIGINT NOT NULL DEFAULT 0;
ALTER TABLE graph_dependency_state ADD COLUMN IF NOT EXISTS projected_count BIGINT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS graph_dependency_work ON graph_dependency_state(exploration_id,node_id) WHERE work;
CREATE TABLE IF NOT EXISTS graph_dependencies (
 node_id BIGINT NOT NULL REFERENCES graph_dependency_state(node_id) ON DELETE CASCADE,
 dependency_id BIGINT NOT NULL,
 generation BIGINT NOT NULL, revision BIGINT NOT NULL,
 PRIMARY KEY(node_id,dependency_id)
);
CREATE INDEX IF NOT EXISTS graph_dependency_version ON graph_dependencies(node_id,generation,revision) INCLUDE(dependency_id);
CREATE INDEX IF NOT EXISTS graph_dependency_incoming ON exploration_edges(exploration_id,dst_id,src_id) WHERE rel<>'covers';

CREATE OR REPLACE FUNCTION series_graph_dependency_invalidate(e BIGINT,n BIGINT) RETURNS void LANGUAGE plpgsql AS $$
DECLARE g BIGINT;
BEGIN
 IF NOT EXISTS(SELECT 1 FROM exploration_nodes WHERE id=n AND exploration_id=e) THEN RETURN; END IF;
 PERFORM pg_advisory_xact_lock(e+8100000000);
 INSERT INTO graph_revisions(exploration_id) VALUES(e) ON CONFLICT DO NOTHING;
 SELECT dependency_generation INTO g FROM graph_revisions WHERE exploration_id=e;
 INSERT INTO graph_dependency_state(node_id,exploration_id,generation) VALUES(n,e,g)
 ON CONFLICT(node_id) DO UPDATE SET exploration_id=e,generation=g,revision=graph_dependency_state.revision+1,work=true,rule_version=1;
END $$;

CREATE OR REPLACE FUNCTION series_graph_dependency_node_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN
  -- Cleanup follows the same graph->projection lock order as builders. A node
  -- FK cascade would lock projection rows before our AFTER trigger acquired
  -- the graph lock, allowing a delete/build deadlock.
  PERFORM pg_advisory_xact_lock(OLD.exploration_id+8100000000);
  DELETE FROM graph_dependency_state WHERE node_id=OLD.id;
  RETURN OLD;
 END IF;
 IF TG_OP='UPDATE' AND (NEW.exploration_id,NEW.kind,CASE WHEN NEW.kind='digest' THEN NEW.payload->'anchor_ids' END)
 IS NOT DISTINCT FROM (OLD.exploration_id,OLD.kind,CASE WHEN OLD.kind='digest' THEN OLD.payload->'anchor_ids' END) THEN RETURN NEW; END IF;
 PERFORM series_graph_dependency_invalidate(NEW.exploration_id,NEW.id);
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS series_graph_dependency_node ON exploration_nodes;
CREATE TRIGGER series_graph_dependency_node AFTER INSERT OR UPDATE OR DELETE ON exploration_nodes FOR EACH ROW EXECUTE FUNCTION series_graph_dependency_node_changed();

CREATE OR REPLACE FUNCTION series_graph_dependency_edge_changed() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE e BIGINT;
BEGIN
 IF TG_OP='UPDATE' AND NEW IS NOT DISTINCT FROM OLD THEN RETURN NEW; END IF;
 -- Acquire both exploration locks in order before invalidating either side of
 -- an UPDATE. Source updates never scan/rewrite a whole fanout in this trigger.
 FOR e IN SELECT DISTINCT x FROM unnest(ARRAY[CASE WHEN TG_OP<>'INSERT' THEN OLD.exploration_id END,
 CASE WHEN TG_OP<>'DELETE' THEN NEW.exploration_id END])x WHERE x IS NOT NULL ORDER BY x
 LOOP PERFORM pg_advisory_xact_lock(e+8100000000); END LOOP;
 IF TG_OP<>'INSERT' THEN
  PERFORM series_graph_dependency_invalidate(OLD.exploration_id,CASE WHEN OLD.rel='covers' THEN OLD.src_id ELSE OLD.dst_id END);
 END IF;
 IF TG_OP<>'DELETE' THEN
  PERFORM series_graph_dependency_invalidate(NEW.exploration_id,CASE WHEN NEW.rel='covers' THEN NEW.src_id ELSE NEW.dst_id END);
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS series_graph_dependency_edge ON exploration_edges;
CREATE TRIGGER series_graph_dependency_edge AFTER INSERT OR UPDATE OR DELETE ON exploration_edges FOR EACH ROW EXECUTE FUNCTION series_graph_dependency_edge_changed();

-- Counts change independently of body/maintenance revisions. Access changes
-- remain part of the cache vector and are never represented by a count receipt.
CREATE OR REPLACE FUNCTION series_graph_count_touch(e BIGINT) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
 IF e IS NULL OR NOT EXISTS(SELECT 1 FROM explorations WHERE id=e) THEN RETURN; END IF;
 PERFORM pg_advisory_xact_lock(e+8100000000);
 INSERT INTO graph_revisions(exploration_id,count_tx) VALUES(e,txid_current())
 ON CONFLICT(exploration_id) DO UPDATE SET count_rev=graph_revisions.count_rev+1,count_tx=txid_current()
 WHERE graph_revisions.count_tx<>txid_current();
END $$;

-- Establish lightweight epochs for legacy explorations before Agents start.
-- Lazy projection initialization must not look like a new business change on
-- the first unchanged heartbeat. Bodies remain entirely outside startup work.
INSERT INTO graph_revisions(exploration_id)
SELECT e.id FROM explorations e WHERE NOT EXISTS(SELECT 1 FROM graph_revisions r WHERE r.exploration_id=e.id)
ON CONFLICT DO NOTHING;

CREATE OR REPLACE FUNCTION series_graph_node_changed() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE r BIGINT; source_referenced BOOLEAN; access_changed BOOLEAN;
BEGIN
 IF TG_OP='DELETE' THEN
  source_referenced:=false;
  IF OLD.kind<>'digest' THEN
   SELECT EXISTS(SELECT 1 FROM exploration_nodes d WHERE d.exploration_id=OLD.exploration_id AND d.kind='digest' AND d.state='active'
     AND d.payload->'source_versions' ? OLD.id::text) INTO source_referenced;
  END IF;
  PERFORM series_graph_touch(OLD.exploration_id,OLD.id,true,source_referenced,OLD.kind='digest');
  PERFORM series_graph_count_touch(OLD.exploration_id);
  DELETE FROM graph_node_projection WHERE node_id=OLD.id;
  DELETE FROM graph_repair_seen WHERE exploration_id=OLD.exploration_id AND node_id=OLD.id;
  DELETE FROM graph_hot_reach WHERE exploration_id=OLD.exploration_id AND node_id=OLD.id;
  RETURN OLD;
 END IF;
 IF TG_OP='UPDATE' AND (NEW.kind,NEW.state,NEW.payload,NEW.priority,NEW.owner,NEW.blocked_reason)
   IS NOT DISTINCT FROM (OLD.kind,OLD.state,OLD.payload,OLD.priority,OLD.owner,OLD.blocked_reason) THEN RETURN NEW; END IF;
 -- Insertion already changes count_rev and cannot revoke an existing readable
 -- source. Avoid a summary-key probe on the high-volume append-only path.
 source_referenced:=false;
 IF TG_OP='UPDATE' AND NEW.kind<>'digest' AND NEW.kind IS NOT DISTINCT FROM OLD.kind THEN
  SELECT EXISTS(SELECT 1 FROM exploration_nodes d WHERE d.exploration_id=NEW.exploration_id AND d.kind='digest' AND d.state='active'
    AND d.payload->'source_versions' ? NEW.id::text) INTO source_referenced;
 END IF;
 access_changed:=source_referenced OR NEW.payload->'asset_ids' IS DISTINCT FROM OLD.payload->'asset_ids'
   OR (TG_OP='UPDATE' AND NEW.kind IS DISTINCT FROM OLD.kind)
   OR (NEW.kind='digest' AND (NEW.payload->'anchor_ids',NEW.payload->'source_versions',NEW.payload->'source_fingerprints',NEW.payload->'projection_generation',NEW.payload->'source_exploration_id')
      IS DISTINCT FROM (OLD.payload->'anchor_ids',OLD.payload->'source_versions',OLD.payload->'source_fingerprints',OLD.payload->'projection_generation',OLD.payload->'source_exploration_id'));
 -- A new derived summary has no previous readable body to revoke. Its count
 -- epoch changes below, but publication alone must not wake the Planner.
 IF TG_OP='INSERT' AND NEW.kind='digest' THEN access_changed:=false; END IF;
 PERFORM series_graph_touch(NEW.exploration_id,NEW.id,TG_OP='INSERT' OR NEW.state IS DISTINCT FROM OLD.state,access_changed,NEW.kind='digest');
 IF TG_OP='INSERT' OR (NEW.kind,NEW.state) IS DISTINCT FROM (OLD.kind,OLD.state) THEN
  PERFORM series_graph_count_touch(NEW.exploration_id);
 END IF;
 IF NEW.kind<>'digest' THEN
  SELECT content_rev INTO r FROM graph_revisions WHERE exploration_id=NEW.exploration_id;
  INSERT INTO graph_node_projection(node_id,exploration_id,kind,state,summary,confidence,source_version,source_hash,payload_asset_ids,asset_metadata_ready)
  VALUES(NEW.id,NEW.exploration_id,NEW.kind,NEW.state,left(COALESCE(NEW.payload->>'summary',NEW.payload->>'text',''),600),
   left(COALESCE(NEW.payload->>'confidence',''),40),r,md5(NEW.payload::text||NEW.state||NEW.kind),series_graph_payload_assets(NEW.payload),true)
  ON CONFLICT(node_id) DO UPDATE SET kind=EXCLUDED.kind,state=EXCLUDED.state,summary=EXCLUDED.summary,
   confidence=EXCLUDED.confidence,source_version=EXCLUDED.source_version,source_hash=EXCLUDED.source_hash,
   payload_asset_ids=EXCLUDED.payload_asset_ids,asset_metadata_ready=true,changed_at=clock_timestamp(),hot=true,cold_since_at=NULL;
 ELSIF TG_OP='UPDATE' AND OLD.kind<>'digest' THEN
  -- A converted node must not retain a fact's cached asset metadata or appear
  -- to remain a valid source in older versioned summaries.
  DELETE FROM graph_node_projection WHERE node_id=NEW.id;
 END IF;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS series_graph_node ON exploration_nodes;
CREATE TRIGGER series_graph_node AFTER INSERT OR UPDATE OR DELETE ON exploration_nodes FOR EACH ROW EXECUTE FUNCTION series_graph_node_changed();

CREATE OR REPLACE FUNCTION series_graph_edge_changed() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE j JSONB; changes JSONB; e BIGINT; a BIGINT; b BIGINT;
BEGIN
 IF TG_OP='UPDATE' AND NEW IS NOT DISTINCT FROM OLD THEN RETURN NEW; END IF;
 changes:=CASE WHEN TG_OP='UPDATE' THEN jsonb_build_array(to_jsonb(OLD),to_jsonb(NEW))
 WHEN TG_OP='DELETE' THEN jsonb_build_array(to_jsonb(OLD)) ELSE jsonb_build_array(to_jsonb(NEW)) END;
 -- UPDATE can remove a dependency from the old scope as well as add one to
 -- the new scope. Invalidate both, with the same sorted graph lock order.
 FOR e IN SELECT DISTINCT CASE WHEN TG_TABLE_NAME='exploration_edges' THEN (x.value->>'exploration_id')::BIGINT
 ELSE (SELECT exploration_id FROM exploration_nodes WHERE id=(x.value->>'node_id')::BIGINT) END
 FROM jsonb_array_elements(changes)x ORDER BY 1
 LOOP IF e IS NOT NULL THEN PERFORM pg_advisory_xact_lock(e+8100000000); END IF; END LOOP;
 FOR j IN SELECT value FROM jsonb_array_elements(changes) LOOP
  IF TG_TABLE_NAME='exploration_edges' THEN
   e:=(j->>'exploration_id')::BIGINT; a:=(j->>'src_id')::BIGINT; b:=(j->>'dst_id')::BIGINT;
   PERFORM series_graph_touch(e,a,true,j->>'rel'<>'covers',j->>'rel'='covers');
   IF j->>'rel'='covers' THEN PERFORM series_graph_count_touch(e); END IF;
   IF j->>'rel'<>'covers' THEN
    PERFORM series_graph_touch(e,b,true,true);
    UPDATE graph_node_projection SET source_version=(SELECT content_rev FROM graph_revisions WHERE exploration_id=e),hot=true,cold_since_at=NULL
    WHERE exploration_id=e AND node_id IN (a,b) AND (source_version<>(SELECT content_rev FROM graph_revisions WHERE exploration_id=e) OR NOT hot OR cold_since_at IS NOT NULL);
   END IF;
  ELSE
   a:=(j->>'node_id')::BIGINT;
   SELECT exploration_id INTO e FROM exploration_nodes WHERE id=a;
   PERFORM series_graph_touch(e,a,true,true);
   UPDATE graph_node_projection SET source_version=(SELECT content_rev FROM graph_revisions WHERE exploration_id=e),hot=true,cold_since_at=NULL
   WHERE exploration_id=e AND node_id=a AND (source_version<>(SELECT content_rev FROM graph_revisions WHERE exploration_id=e) OR NOT hot OR cold_since_at IS NOT NULL);
  END IF;
 END LOOP;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS series_graph_edge ON exploration_edges;
CREATE TRIGGER series_graph_edge AFTER INSERT OR UPDATE OR DELETE ON exploration_edges FOR EACH ROW EXECUTE FUNCTION series_graph_edge_changed();
DROP TRIGGER IF EXISTS series_graph_anchor ON exploration_anchors;
CREATE TRIGGER series_graph_anchor AFTER INSERT OR UPDATE OR DELETE ON exploration_anchors FOR EACH ROW EXECUTE FUNCTION series_graph_edge_changed();

-- Task scope/auth epochs are invalidated at mutation time, never by hashing all
-- payloads on every heartbeat. Inspect both old and new ownership on updates.
CREATE OR REPLACE FUNCTION series_graph_context_changed() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE j JSONB; oldj JSONB; e BIGINT; tid BIGINT; cid BIGINT; oldcid BIGINT; aid BIGINT;
BEGIN
 j:=CASE WHEN TG_OP='DELETE' THEN to_jsonb(OLD) ELSE to_jsonb(NEW) END;
 oldj:=CASE WHEN TG_OP='INSERT' THEN '{}'::jsonb ELSE to_jsonb(OLD) END;
 IF TG_OP='UPDATE' AND (j-'updated_at') IS NOT DISTINCT FROM (oldj-'updated_at') THEN RETURN NEW; END IF;
 IF TG_TABLE_NAME='explorations' THEN
  IF TG_OP='UPDATE' AND (j->'description',j->'goal',j->'status') IS NOT DISTINCT FROM (oldj->'description',oldj->'goal',oldj->'status') THEN RETURN NEW; END IF;
  PERFORM series_graph_touch((j->>'id')::BIGINT);
 ELSIF TG_TABLE_NAME='assets' THEN
  aid:=(j->>'id')::BIGINT;
  FOR e IN SELECT DISTINCT t.exploration_id FROM tasks t WHERE t.deleted_at IS NULL AND (
   j->'task_ids' @> to_jsonb(ARRAY[t.id]) OR oldj->'task_ids' @> to_jsonb(ARRAY[t.id])
   OR EXISTS(SELECT 1 FROM task_asset_links l WHERE l.task_id=t.id AND l.asset_id=aid)
   OR EXISTS(SELECT 1 FROM exploration_anchors a JOIN exploration_nodes n ON n.id=a.node_id WHERE a.asset_id=aid AND n.exploration_id=t.exploration_id)
   OR (task_asset_effectively_approved(t.id,aid) AND EXISTS(SELECT 1 FROM task_scope s WHERE s.task_id=t.id AND (
    (s.kind='company' AND s.company_id IN (NULLIF(j->>'company_id','')::BIGINT,NULLIF(oldj->>'company_id','')::BIGINT))
    OR (s.kind IN ('root_domain','subdomain') AND s.domain IN (j->>'domain',j->>'root_domain',oldj->>'domain',oldj->>'root_domain'))
    OR (s.kind IN ('ip','cidr') AND (s.net >>= try_inet(j->>'ip') OR s.net >>= try_inet(oldj->>'ip')))
    OR (s.kind='icp' AND s.value IN (j->>'icp',j->>'app_icp',oldj->>'icp',oldj->>'app_icp'))
    OR (s.kind='keyword' AND (strpos(lower(j::text),lower(s.value))>0 OR strpos(lower(oldj::text),lower(s.value))>0)))))) ORDER BY t.exploration_id
  LOOP PERFORM series_graph_touch(e,0,false,true); END LOOP;
 ELSIF TG_TABLE_NAME IN ('companies','company_scope') THEN
  cid:=CASE WHEN TG_TABLE_NAME='companies' THEN (j->>'id')::BIGINT ELSE (j->>'company_id')::BIGINT END;
  oldcid:=CASE WHEN TG_TABLE_NAME='companies' THEN NULLIF(oldj->>'id','')::BIGINT ELSE NULLIF(oldj->>'company_id','')::BIGINT END;
  FOR e IN SELECT DISTINCT t.exploration_id FROM tasks t WHERE t.company_id IN (cid,oldcid) OR EXISTS(SELECT 1 FROM task_scope s WHERE s.task_id=t.id AND s.company_id IN (cid,oldcid)) ORDER BY t.exploration_id
  LOOP PERFORM series_graph_touch(e,0,false,true); END LOOP;
 ELSIF TG_TABLE_NAME='tasks' THEN
  IF TG_OP='UPDATE' AND (j->'goal',j->'description',j->'status',j->'paused',j->'deleted_at',j->'execution_mode',j->'company_id')
   IS NOT DISTINCT FROM (oldj->'goal',oldj->'description',oldj->'status',oldj->'paused',oldj->'deleted_at',oldj->'execution_mode',oldj->'company_id') THEN RETURN NEW; END IF;
  PERFORM series_graph_touch((j->>'exploration_id')::BIGINT,0,false,true);
 ELSE
  FOR tid IN SELECT DISTINCT x FROM unnest(ARRAY[NULLIF(j->>'task_id','')::BIGINT,NULLIF(oldj->>'task_id','')::BIGINT])x WHERE x IS NOT NULL ORDER BY x LOOP
   SELECT exploration_id INTO e FROM tasks WHERE id=tid;
   PERFORM series_graph_touch(e,CASE WHEN TG_TABLE_NAME='findings' THEN COALESCE((
    SELECT id FROM exploration_nodes WHERE id=NULLIF(j->>'node_id','')::BIGINT AND exploration_id=e),0) ELSE 0 END,false,
    TG_TABLE_NAME IN ('task_relations','task_asset_links','task_asset_blocks','task_scope','task_asset_grants','task_asset_skips','task_asset_dns_evidence')
    OR (TG_TABLE_NAME='findings' AND (TG_OP<>'UPDATE' OR (j->'task_id',j->'node_id',j->'asset_ids') IS DISTINCT FROM (oldj->'task_id',oldj->'node_id',oldj->'asset_ids'))));
  END LOOP;
  IF j ? 'exploration_id' THEN PERFORM series_graph_touch((j->>'exploration_id')::BIGINT); END IF;
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW;
END $$;
DO $$ DECLARE tab TEXT; BEGIN
 FOREACH tab IN ARRAY ARRAY['explorations','tasks','assets','companies','company_scope','task_relations','task_asset_links','task_asset_blocks','task_scope','task_asset_grants','task_asset_skips','task_asset_dns_evidence','task_constraints','findings','finding_suppression_rules'] LOOP
  IF to_regclass(tab) IS NOT NULL THEN
   EXECUTE format('DROP TRIGGER IF EXISTS series_graph_context ON %I',tab);
   EXECUTE format('CREATE TRIGGER series_graph_context AFTER INSERT OR UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION series_graph_context_changed()',tab);
  END IF;
 END LOOP;
END $$;
