package db

type dependencyAssetProjection uint8

const (
	dependencyFilterAssets dependencyAssetProjection = iota
	dependencyAllAssets
	dependencyEventAssets
)

// The dependency walk is shared by all roots in a scoped request. In a chain
// of N candidate nodes, (root,ancestor) creates N*(N+1)/2 rows even when every
// node is readable. Reachability and reverse denial propagation keep distinct
// nodes/dependencies instead. This is a snapshot projection, never a grant:
// asset permissions and digest freshness are evaluated in the current query.
func contextDependencyVisibility(scope, roots string, worker, allowStaleRoot, requireMembers bool, assetMode dependencyAssetProjection) string {
	stale := "NOT series_digest_sources_current(n.payload)"
	if allowStaleRoot {
		stale = "(n.id<>$2 AND " + stale + ")"
	}
	if requireMembers {
		stale += ` OR NOT EXISTS(SELECT 1 FROM exploration_edges de WHERE de.exploration_id=n.exploration_id AND de.src_id=n.id AND de.rel='covers')`
	}
	assetSeed := " WHERE $5::bigint<>0 AND asset_id=$5"
	eventAssets := ""
	if assetMode == dependencyAllAssets {
		assetSeed = ""
	} else if assetMode == dependencyEventAssets {
		// Planner only compares active directions with this round's visible
		// events. Do not propagate every asset through the entire candidate graph
		// just to discover that an empty event set has no related directions.
		eventAssets = `), event_dependencies(id,exp) AS (
 SELECT r.id,r.exploration_id FROM roots r WHERE r.id=ANY($3::bigint[])
 AND NOT EXISTS(SELECT 1 FROM unreadable_roots bad WHERE bad.root=r.id)
 UNION SELECT d.dependency_id,d.exp FROM event_dependencies e JOIN dependencies d ON d.node_id=e.id AND d.exp=e.exp
 ), event_assets AS MATERIALIZED (
 SELECT DISTINCT a.asset_id FROM event_dependencies e JOIN direct_assets a ON a.node_id=e.id AND a.exp=e.exp
 `
		assetSeed = ` WHERE asset_id IN(SELECT asset_id FROM event_assets)`
	}
	return workerReadSQL(scope+`, roots AS MATERIALIZED (`+roots+`
 ), reachable(id,exp) AS (
 SELECT id,exploration_id FROM roots WHERE owner<>0 OR $5::bigint<>0
 UNION SELECT step.id,r.exp FROM reachable r CROSS JOIN LATERAL (`+contextDependencySteps+`
 )step
 ), dependencies AS MATERIALIZED (
 SELECT r.id node_id,step.id dependency_id,r.exp FROM reachable r CROSS JOIN LATERAL (`+contextDependencySteps+`
 )step
 ), direct_assets AS MATERIALIZED (
 SELECT r.id node_id,r.exp,a.asset_id FROM reachable r JOIN exploration_nodes source_node ON source_node.id=r.id AND source_node.exploration_id=r.exp
 JOIN exploration_anchors a ON a.node_id=r.id
 UNION SELECT r.id,r.exp,CASE WHEN x.value ~ '^[0-9]{1,18}$' THEN x.value::bigint ELSE 0 END
 FROM reachable r JOIN exploration_nodes n ON n.id=r.id AND n.exploration_id=r.exp
 LEFT JOIN graph_node_projection gp ON gp.node_id=n.id AND gp.asset_metadata_ready
 CROSS JOIN LATERAL jsonb_array_elements_text(CASE WHEN gp.node_id IS NOT NULL THEN to_jsonb(gp.payload_asset_ids)
 ELSE CASE WHEN jsonb_typeof(n.payload->'asset_ids')='array' THEN n.payload->'asset_ids' ELSE '[]'::jsonb END END)x(value)
 ), permission_keys AS MATERIALIZED (
 SELECT DISTINCT c.owner,a.asset_id FROM direct_assets a JOIN contexts c ON c.exp=a.exp WHERE c.owner<>0
 ), permissions AS MATERIALIZED (
 SELECT owner,asset_id,task_asset_effectively_approved(owner,asset_id)
 AND task_asset_effectively_approved((SELECT id FROM current_task),asset_id) allowed FROM permission_keys
 ), unreadable_nodes(id,exp) AS (
 SELECT r.id,r.exp FROM reachable r LEFT JOIN exploration_nodes n ON n.id=r.id AND n.exploration_id=r.exp
 WHERE n.id IS NULL OR (n.kind='digest' AND (`+stale+`))
 UNION SELECT a.node_id,a.exp FROM direct_assets a JOIN contexts c ON c.exp=a.exp
 JOIN permissions p ON p.owner=c.owner AND p.asset_id=a.asset_id WHERE NOT p.allowed
 UNION SELECT d.node_id,d.exp FROM unreadable_nodes bad JOIN dependencies d ON d.exp=bad.exp AND d.dependency_id=bad.id
 ), unreadable_roots AS MATERIALIZED (
 SELECT r.id root FROM roots r JOIN unreadable_nodes bad ON bad.id=r.id AND bad.exp=r.exploration_id
 `+eventAssets+`), dependency_assets(node_id,exp,asset_id) AS (
 SELECT node_id,exp,asset_id FROM direct_assets`+assetSeed+`
 UNION SELECT d.node_id,d.exp,a.asset_id FROM dependency_assets a JOIN dependencies d ON d.exp=a.exp AND d.dependency_id=a.node_id
 ), anchors AS MATERIALIZED (
 SELECT a.node_id root,a.asset_id FROM dependency_assets a JOIN roots r ON r.id=a.node_id AND r.exploration_id=a.exp
 ), visible AS MATERIALIZED (
 SELECT r.* FROM roots r WHERE ($5::bigint=0 OR EXISTS(SELECT 1 FROM anchors a WHERE a.root=r.id AND a.asset_id=$5))
 AND NOT EXISTS(SELECT 1 FROM unreadable_roots denied WHERE denied.root=r.id)
 )`, worker)
}

// r always describes a reached node, including a missing/out-of-scope node.
// Keep malformed references in the projection: they must propagate denial,
// rather than disappear because a join silently dropped the bad dependency.
const contextDependencySteps = `
 WITH cached AS MATERIALIZED (
 SELECT p.node_id,p.generation,p.projected_set revision FROM graph_dependency_state p JOIN graph_revisions v ON v.exploration_id=p.exploration_id
 WHERE p.node_id=r.id AND p.exploration_id=r.exp AND p.generation=v.dependency_generation AND p.rule_version=1 AND p.projected_revision=p.revision AND p.projected_set>0
 AND p.projected_count=(SELECT count(*) FROM graph_dependencies check_rows WHERE check_rows.node_id=p.node_id AND check_rows.generation=p.generation AND check_rows.revision=p.projected_set)
 ) SELECT d.dependency_id id FROM cached c JOIN graph_dependencies d ON d.node_id=c.node_id AND d.generation=c.generation AND d.revision=c.revision
 UNION SELECT live.id FROM (` + liveContextDependencySteps + `)live WHERE NOT EXISTS(SELECT 1 FROM cached)`

const liveContextDependencySteps = `
 SELECT e.dst_id id FROM exploration_edges e WHERE e.exploration_id=r.exp AND e.rel='covers' AND e.src_id=r.id
 UNION SELECT e.src_id id FROM exploration_edges e WHERE e.exploration_id=r.exp AND e.rel<>'covers' AND e.dst_id=r.id
 UNION SELECT CASE WHEN x.value ~ '^[0-9]{1,18}$' THEN x.value::bigint ELSE 0 END
 FROM exploration_nodes dn CROSS JOIN LATERAL jsonb_array_elements_text(CASE WHEN jsonb_typeof(dn.payload->'anchor_ids')='array' THEN dn.payload->'anchor_ids' ELSE '[]'::jsonb END)x(value)
 WHERE dn.id=r.id AND dn.exploration_id=r.exp AND dn.kind='digest'`
