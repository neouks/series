WITH scope AS (
 SELECT $1::bigint id UNION SELECT t.exploration_id FROM tasks c JOIN task_relations r ON r.task_id=c.id
 JOIN tasks t ON t.id=r.source_task_id AND t.deleted_at IS NULL WHERE c.exploration_id=$1 AND c.deleted_at IS NULL
 ) SELECT COALESCE(string_agg(concat_ws(':',s.id,COALESCE(r.generation,0),COALESCE(r.content_rev,0),COALESCE(r.topology_rev,0),COALESCE(r.access_rev,0),
 CASE WHEN $2 THEN COALESCE(r.view_rev,0) ELSE 0 END),'|' ORDER BY s.id),'') FROM scope s LEFT JOIN graph_revisions r ON r.exploration_id=s.id