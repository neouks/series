
WITH context_tasks(task_id, exploration_id) AS (
    SELECT task_current.id, task_current.exploration_id
    FROM tasks task_current
    WHERE task_current.exploration_id=$1 AND task_current.deleted_at IS NULL
    UNION
    SELECT source.id, source.exploration_id
    FROM tasks task_current
    JOIN task_relations relation ON relation.task_id=task_current.id
    JOIN tasks source ON source.id=relation.source_task_id AND source.deleted_at IS NULL
    WHERE task_current.exploration_id=$1 AND task_current.deleted_at IS NULL
),
visible_exp(id) AS (
    SELECT $1::bigint
    UNION
    SELECT exploration_id FROM context_tasks
),
context_assets(id) AS (
    SELECT asset.id
    FROM assets asset
    WHERE asset.task_ids && ARRAY(SELECT task_id FROM context_tasks)
    UNION
    SELECT link.asset_id
    FROM task_asset_links link
    JOIN context_tasks context ON context.task_id=link.task_id
    UNION
    SELECT anchor.asset_id
    FROM exploration_anchors anchor
    JOIN exploration_nodes node ON node.id=anchor.node_id
    WHERE node.exploration_id IN (SELECT id FROM visible_exp)
    UNION
    SELECT asset.id
    FROM assets asset
    JOIN task_scope scope ON (
         (scope.kind='company'     AND asset.company_id=scope.company_id)
      OR (scope.kind='root_domain' AND asset.root_domain=scope.domain)
      OR (scope.kind='subdomain'   AND asset.domain=scope.domain)
      OR (scope.kind IN ('ip','cidr') AND scope.net >>= try_inet(asset.ip))
      OR (scope.kind='icp' AND (
           lower(regexp_replace(COALESCE(asset.icp,''), '[[:space:]]+', '', 'g'))=scope.value
           OR lower(regexp_replace(COALESCE(asset.app_icp,''), '[[:space:]]+', '', 'g'))=scope.value
         ))
    )
    JOIN context_tasks context ON context.task_id=scope.task_id
),
context_companies(id) AS (
    SELECT DISTINCT scope.company_id
    FROM task_scope scope
    JOIN context_tasks context ON context.task_id=scope.task_id
    WHERE scope.kind='company' AND scope.company_id IS NOT NULL
)
SELECT md5(
    COALESCE((SELECT string_agg(concat_ws(':', id, status, md5(COALESCE(description,'') || chr(31) || goal)), '|' ORDER BY id)
              FROM explorations WHERE id IN (SELECT id FROM visible_exp)), '') || '#' ||
    COALESCE((SELECT string_agg(concat_ws(':', id, exploration_id, kind, state, priority,
                                          md5(payload::text), COALESCE(origin,''), COALESCE(owner,''), COALESCE(blocked_reason,'')),
                                     '|' ORDER BY exploration_id, id)
              FROM exploration_nodes WHERE exploration_id IN (SELECT id FROM visible_exp)), '') || '#' ||
    COALESCE((SELECT string_agg(concat_ws(':', exploration_id, src_id, rel, dst_id),
                                     '|' ORDER BY exploration_id, src_id, rel, dst_id)
              FROM exploration_edges WHERE exploration_id IN (SELECT id FROM visible_exp)), '') || '#' ||
    COALESCE((SELECT string_agg(concat_ws(':', anchor.node_id, anchor.asset_id),
                                     '|' ORDER BY anchor.node_id, anchor.asset_id)
              FROM exploration_anchors anchor
              JOIN exploration_nodes node ON node.id=anchor.node_id
              WHERE node.exploration_id IN (SELECT id FROM visible_exp)), '') || '#' ||
    COALESCE((SELECT string_agg(concat_ws(':', id, exploration_id, kind, md5(text), COALESCE(origin,'')),
                                     '|' ORDER BY exploration_id, id)
              FROM task_constraints WHERE exploration_id IN (SELECT id FROM visible_exp)), '') || '#' ||
    COALESCE((SELECT string_agg(concat_ws(':', id, task_id, kind, COALESCE(company_id,0),
                                          COALESCE(domain,''), COALESCE(net::text,''), COALESCE(value,''),
                                          source, COALESCE(reason,'')),
                                     '|' ORDER BY task_id, id)
              FROM task_scope WHERE task_id IN (SELECT task_id FROM context_tasks)), '') || '#' ||
    COALESCE((SELECT string_agg(concat_ws(':', task_id, asset_id, source, md5(source_summary),
                                          COALESCE(source_node_id,0), updated_at),
                                     '|' ORDER BY task_id, asset_id)
              FROM task_asset_links WHERE task_id IN (SELECT task_id FROM context_tasks)
              AND task_asset_effectively_approved((SELECT id FROM tasks WHERE exploration_id=$1),asset_id)), '') || '#' ||
    COALESCE((SELECT string_agg(concat_ws(':', asset.id, asset.updated_at), '|' ORDER BY asset.id)
              FROM assets asset WHERE asset.id IN (SELECT id FROM context_assets)
              AND task_asset_effectively_approved((SELECT id FROM tasks WHERE exploration_id=$1),asset.id)), '') || '#' ||
    COALESCE((SELECT string_agg(concat_ws(':', company.id, company.updated_at), '|' ORDER BY company.id)
              FROM companies company WHERE company.id IN (SELECT id FROM context_companies)), '') || '#' ||
    COALESCE((SELECT string_agg(concat_ws(':', scope.id, scope.company_id, scope.kind,
                                          COALESCE(scope.domain,''), COALESCE(scope.net::text,''),
                                          COALESCE(scope.value,''), scope.raw, COALESCE(scope.reason,'')),
                                     '|' ORDER BY scope.company_id, scope.id)
              FROM company_scope scope WHERE scope.company_id IN (SELECT id FROM context_companies)), '')
)