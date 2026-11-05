package db

import "encoding/json"

type NodeAccess struct {
	CanRead bool     `json:"can_read"`
	Reasons []string `json:"reasons,omitempty"`
}

// TaskNodeAccess revalidates historic graph results in one query. Covers
// edges traverse into digest members; ordinary edges traverse ancestor lineage.
func (s *AssetStore) TaskNodeAccess(taskID int64, ids []int64) (map[int64]NodeAccess, error) {
	states := make(map[int64]NodeAccess, len(ids))
	for _, id := range ids {
		states[id] = NodeAccess{Reasons: []string{"unavailable"}}
	}
	if len(ids) == 0 {
		return states, nil
	}
	rows, err := s.nodeAccessQuery(taskNodeAccessSQL, taskID, ids, s.workerRead)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var reasons []string
		if err := json.Unmarshal(raw, &reasons); err != nil {
			return nil, err
		}
		states[id] = NodeAccess{CanRead: len(reasons) == 0, Reasons: reasons}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return states, nil
}

// Keep the legacy visibility projection for callers that only need a gate.
func (s *AssetStore) TaskNodeApprovalStates(taskID int64, ids []int64) (map[int64]string, error) {
	access, err := s.TaskNodeAccess(taskID, ids)
	if err != nil {
		return nil, err
	}
	states := make(map[int64]string, len(access))
	for id, row := range access {
		if row.CanRead {
			states[id] = ApprovalApproved
		} else {
			states[id] = row.Reasons[0]
		}
	}
	return states, nil
}

// TaskTargetAssetStates exposes only identities belonging to this task or a
// live direct source. Unknown and unrelated identities are indistinguishable.
func (s *AssetStore) TaskTargetAssetStates(taskID int64, ids []int64) (map[int64]string, error) {
	states := make(map[int64]string, len(ids))
	for _, id := range ids {
		states[id] = "unavailable"
	}
	if len(ids) == 0 {
		return states, nil
	}
	rows, err := s.query(`SELECT a.id,task_asset_effective_approval_state($1,a.id) FROM assets a
 WHERE a.id=ANY($2::bigint[]) AND EXISTS(SELECT 1 FROM tasks WHERE id=$1 AND deleted_at IS NULL) AND EXISTS (
 SELECT 1 FROM task_asset_links l JOIN tasks t ON t.id=l.task_id AND t.deleted_at IS NULL
 WHERE l.asset_id=a.id AND (l.task_id=$1 OR l.task_id IN (SELECT source_task_id FROM task_relations WHERE task_id=$1)))`, taskID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var state string
		if err := rows.Scan(&id, &state); err != nil {
			return nil, err
		}
		states[id] = state
	}
	return states, rows.Err()
}
