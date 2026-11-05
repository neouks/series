-- Navigation only, not evidence or authorization. Source row identity is read
-- from heap metadata, so a valid prefix does not detoast the original payload.
CREATE OR REPLACE FUNCTION series_graph_navigation_rule() RETURNS integer LANGUAGE sql IMMUTABLE AS $$ SELECT 1 $$;
CREATE OR REPLACE FUNCTION series_graph_navigation_preview(p JSONB) RETURNS JSONB LANGUAGE sql IMMUTABLE AS $$
 SELECT jsonb_build_object('name',left(p->>'name',500),'summary',left(p->>'summary',600),
 'text',left(p->>'text',600),'description',left(p->>'description',500),'body',left(p->>'body',1200))
$$;
CREATE OR REPLACE FUNCTION series_graph_navigation_valid(p JSONB) RETURNS boolean LANGUAGE sql IMMUTABLE AS $$
 SELECT CASE WHEN jsonb_typeof(p)='object' THEN p ?& ARRAY['name','summary','text','description','body']
 AND NOT EXISTS(SELECT 1 FROM jsonb_each(p) x WHERE x.key NOT IN ('name','summary','text','description','body')
 OR jsonb_typeof(x.value) NOT IN ('string','null')
 OR char_length(x.value#>>'{}')>CASE x.key WHEN 'name' THEN 500 WHEN 'summary' THEN 600 WHEN 'text' THEN 600 WHEN 'description' THEN 500 ELSE 1200 END) ELSE false END
$$;
CREATE TABLE IF NOT EXISTS graph_navigation_projection (
 node_id BIGINT PRIMARY KEY,
 exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
 source_stamp TEXT NOT NULL,
 rule_version INTEGER NOT NULL,
 preview JSONB NOT NULL CHECK(series_graph_navigation_valid(preview)),
 CHECK(octet_length(preview::text)<=32768)
);
CREATE INDEX IF NOT EXISTS graph_navigation_scope ON graph_navigation_projection(exploration_id,node_id);
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS navigation_cursor BIGINT NOT NULL DEFAULT 0;
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS navigation_rule INTEGER NOT NULL DEFAULT 0;
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS navigation_relation OID NOT NULL DEFAULT 0;
ALTER TABLE graph_revisions ADD COLUMN IF NOT EXISTS navigation_ready BOOLEAN NOT NULL DEFAULT false;

CREATE OR REPLACE FUNCTION series_graph_navigation_changed() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE e BIGINT; current_stamp TEXT;
BEGIN
 -- Acquire the existing graph lock before touching cache rows. Do not take a
 -- node FK lock during background repair: a concurrent DELETE may already own
 -- the raw row and be waiting for this graph lock. Deletion is explicit below.
 FOR e IN SELECT DISTINCT x FROM unnest(CASE WHEN TG_OP='INSERT' THEN ARRAY[NEW.exploration_id]
 WHEN TG_OP='DELETE' THEN ARRAY[OLD.exploration_id] ELSE ARRAY[OLD.exploration_id,NEW.exploration_id] END)x ORDER BY x
 LOOP PERFORM pg_advisory_xact_lock(e+8100000000); END LOOP;
 IF TG_OP='DELETE' THEN DELETE FROM graph_navigation_projection WHERE node_id=OLD.id; RETURN OLD; END IF;
 IF TG_OP='UPDATE' AND (NEW.origin,NEW.created_at) IS DISTINCT FROM (OLD.origin,OLD.created_at) THEN
  PERFORM series_graph_touch(NEW.exploration_id,NEW.id,false,false,true);
 END IF;
 SELECT n.xmin::text||':'||n.ctid::text INTO current_stamp FROM exploration_nodes n WHERE n.id=NEW.id;
 IF TG_OP='UPDATE' AND current_stamp=NEW.xmin::text||':'||NEW.ctid::text AND NEW.payload IS NOT DISTINCT FROM OLD.payload THEN
  UPDATE graph_navigation_projection SET source_stamp=current_stamp,exploration_id=NEW.exploration_id
  WHERE node_id=NEW.id AND source_stamp=OLD.xmin::text||':'||OLD.ctid::text AND rule_version=series_graph_navigation_rule();
  IF FOUND THEN RETURN NEW; END IF;
 END IF;
 -- Read the actual current row, not a possibly superseded NEW record from an
 -- earlier nested trigger. Every update (including no-op updates) changes CTID.
 INSERT INTO graph_navigation_projection(node_id,exploration_id,source_stamp,rule_version,preview)
 SELECT n.id,n.exploration_id,n.xmin::text||':'||n.ctid::text,series_graph_navigation_rule(),series_graph_navigation_preview(n.payload)
 FROM exploration_nodes n WHERE n.id=NEW.id
 ON CONFLICT(node_id) DO UPDATE SET exploration_id=EXCLUDED.exploration_id,source_stamp=EXCLUDED.source_stamp,
 rule_version=EXCLUDED.rule_version,preview=EXCLUDED.preview;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS zz_series_graph_navigation ON exploration_nodes;
CREATE TRIGGER zz_series_graph_navigation AFTER INSERT OR UPDATE OR DELETE ON exploration_nodes FOR EACH ROW EXECUTE FUNCTION series_graph_navigation_changed();
