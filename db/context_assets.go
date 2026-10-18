package db

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ContextAssets authorizes and projects the requested groups in one snapshot.
// Credentials are never selected implicitly. Large values stay in PostgreSQL;
// the caller receives JSON pointers or a bounded text/collection window.
func (s *AssetStore) ContextAssets(ctx context.Context, taskID int64, ids []int64, groups []string, field string, index, offset, budget int) ([]map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	path := []string{}
	if field != "" {
		if !strings.HasPrefix(field, "/") {
			return nil, fmt.Errorf("field 必须为 JSON Pointer")
		}
		for _, p := range strings.Split(field[1:], "/") {
			path = append(path, strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~"))
		}
	}
	query := `WITH visible AS MATERIALIZED (
 SELECT a.*,CASE WHEN $2=0 THEN '' ELSE task_asset_effective_approval_state($2,a.id) END approval_state FROM assets a WHERE a.id=ANY($1::bigint[]) AND ($2::bigint=0 OR task_asset_effectively_approved($2,a.id))
 ), hosts AS MATERIALIZED (
 SELECT DISTINCT host FROM visible a CROSS JOIN LATERAL (SELECT COALESCE(a.ip,'') host UNION SELECT unnest(a.bound_domains) UNION SELECT unnest(a.record_value) WHERE a.record_type IN ('A','AAAA','CNAME')) h
 ), allowed_hosts AS MATERIALIZED (SELECT host FROM hosts WHERE $2=0 OR task_host_approval_state($2,lower(rtrim(host,'.')))='approved' OR ($10 AND task_host_approval_state($2,lower(rtrim(host,'.')))='pending')),
 content AS (SELECT a.id,a.type,a.approval_state,(
 SELECT COALESCE(jsonb_object_agg(g.key,g.value),'{}'::jsonb) FROM (VALUES
 ('identity',jsonb_build_object('domain',a.domain,'ip',CASE WHEN a.ip IN (SELECT host FROM allowed_hosts) THEN a.ip ELSE '' END,'url',a.url,'port',a.port,'method',a.method,'app_name',a.app_name,'bundle_id',a.bundle_id)),
 ('fingerprint',jsonb_build_object('page_title',a.page_title,'status_code',a.status_code,'technologies',a.technologies,'service_name',a.service_name,'favicon_mmh3',a.favicon_mmh3)),
 ('dns',jsonb_build_object('record_type',a.record_type,'record_value',CASE WHEN $2=0 OR a.record_type NOT IN ('A','AAAA','CNAME') THEN to_jsonb(a.record_value) ELSE (SELECT COALESCE(jsonb_agg(v),'[]'::jsonb) FROM unnest(a.record_value)v WHERE v IN (SELECT host FROM allowed_hosts)) END,'bound_domains',(SELECT COALESCE(jsonb_agg(v),'[]'::jsonb) FROM unnest(a.bound_domains)v WHERE v IN (SELECT host FROM allowed_hosts)),'open_ports',a.open_ports)),
 ('params',to_jsonb(a.params)),('auth',to_jsonb(a.auth)),('extra',a.extra)
 )g(key,value) WHERE g.key=ANY($3::text[]))#>$4::text[] value FROM visible a)
 SELECT jsonb_build_object('id',c.id,'asset_id',c.id,'type',c.type,'approval_state',c.approval_state,'details',
 CASE WHEN c.value IS NULL THEN jsonb_build_object('error','字段不存在或未选择所属 fields')
 WHEN jsonb_typeof(c.value)='string' THEN jsonb_build_object('value',substring(c.value#>>'{}' FROM $6+1 FOR $7),'offset',$6,'total_chars',char_length(c.value#>>'{}'),'next_offset',CASE WHEN char_length(c.value#>>'{}')>$6+$7 THEN $6+$7 END)
 WHEN jsonb_typeof(c.value) IN ('array','object') THEN (WITH children AS (
 SELECT child.key,child.value,row_number() OVER(ORDER BY child.key) seq FROM jsonb_each(CASE WHEN jsonb_typeof(c.value)='object' THEN c.value ELSE '{}'::jsonb END)child
 UNION ALL SELECT (child.ordinality-1)::text,child.value,child.ordinality FROM jsonb_array_elements(CASE WHEN jsonb_typeof(c.value)='array' THEN c.value ELSE '[]'::jsonb END) WITH ORDINALITY child
 ), page AS (SELECT * FROM children ORDER BY seq OFFSET $5 LIMIT 20)
 SELECT jsonb_build_object('entries',COALESCE((SELECT jsonb_agg(CASE WHEN octet_length(value::text)<=$7/20 THEN jsonb_build_object('field',$8||'/'||replace(replace(key,'~','~0'),'/','~1'),'value',value) ELSE jsonb_build_object('field',$8||'/'||replace(replace(key,'~','~0'),'/','~1'),'deferred',true,'type',jsonb_typeof(value)) END ORDER BY seq) FROM page),'[]'::jsonb),'next_index',CASE WHEN (SELECT count(*) FROM children)>$5+20 THEN $5+20 END))
 ELSE jsonb_build_object('value',c.value) END)
 FROM content c WHERE $9::boolean ORDER BY array_position($1::bigint[],c.id)`
	rows, e := s.db.QueryContext(ctx, workerReadSQL(query, s.workerRead), ids, taskID, groups, path, index, offset, budget, field, true, s.workerRead)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var b []byte
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		var row map[string]any
		if e = decodeContextJSON(b, &row); e != nil {
			return nil, e
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
