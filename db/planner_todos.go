package db

import (
	"context"
	"encoding/json"
	"fmt"
)

// PlannerTodoSnapshot is navigation, never the complete replaceable list.
// Only a changed source is expanded. A warm read checks heap metadata without
// evaluating the JSON array; the caller's small preview cache is not authority.
type PlannerTodoSnapshot struct {
	Stamp     string
	Unchanged bool
	Counts    map[string]int64
	Items     []PlannerTodoPreview
	Total     int64
}

const plannerTodoSnapshotSQL = `WITH head AS MATERIALIZED (
 SELECT COALESCE((SELECT xmin::text||':'||ctid::text FROM planner_working_state WHERE exploration_id=$1),'absent') stamp
 ), changed AS MATERIALIZED (
 SELECT p.todos FROM planner_working_state p CROSS JOIN head h WHERE p.exploration_id=$1 AND h.stamp<>$2
 ), entries AS MATERIALIZED (
 SELECT (a.ordinality-1)::int index,a.value->>'status' status,
 COALESCE(jsonb_typeof(a.value)='object' AND jsonb_typeof(a.value->'content')='string'
 AND a.value->>'status' IN ('pending','in_progress','completed'),false) valid
 FROM changed c CROSS JOIN LATERAL jsonb_array_elements(c.todos) WITH ORDINALITY a(value,ordinality)
 ), picked AS MATERIALIZED (
 SELECT index,status FROM entries WHERE status IN ('in_progress','pending')
 ORDER BY CASE status WHEN 'in_progress' THEN 0 ELSE 1 END,index LIMIT 12
 ) SELECT jsonb_build_object('stamp',h.stamp,'unchanged',h.stamp=$2,
 'valid',COALESCE((SELECT bool_and(valid) FROM entries),true),
 'counts',COALESCE((SELECT jsonb_object_agg(status,n) FROM (SELECT status,count(*) n FROM entries GROUP BY status)g),'{}'::jsonb),
 'total',(SELECT count(*) FROM entries),
 'items',COALESCE((SELECT jsonb_agg(jsonb_build_object('Index',p.index,'Status',p.status,
 'Content',left(c.todos->p.index->>'content',500),'TotalChars',char_length(c.todos->p.index->>'content'))
 ORDER BY CASE p.status WHEN 'in_progress' THEN 0 ELSE 1 END,p.index) FROM picked p CROSS JOIN changed c),'[]'::jsonb))
 FROM head h`

func (s *ExplorationStore) PlannerTodoSnapshot(ctx context.Context, known string) (PlannerTodoSnapshot, error) {
	ctx, cancel := graphReadContext(ctx)
	defer cancel()
	var out PlannerTodoSnapshot
	var raw []byte
	err := s.db.QueryRowContext(ctx, plannerTodoSnapshotSQL, s.expID, known).Scan(&raw)
	if err != nil {
		return out, err
	}
	var result struct {
		Stamp     string
		Unchanged bool
		Valid     bool
		Counts    map[string]int64
		Items     []PlannerTodoPreview
		Total     int64
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		return out, err
	}
	if !result.Valid {
		return out, fmt.Errorf("invalid persisted planner todo; original list was preserved")
	}
	out = PlannerTodoSnapshot{Stamp: result.Stamp, Unchanged: result.Unchanged, Counts: result.Counts, Items: result.Items, Total: result.Total}
	return out, nil
}
