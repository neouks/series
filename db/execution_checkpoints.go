package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var ErrCheckpointConflict = errors.New("execution checkpoint changed; reload current version")

// Stage notes must use the role/source-aware checkpoint reader. Otherwise an
// old write/read activity would bypass freshness by exposing its raw JSON as
// Worker evidence. Raw activities and the user-facing audit stay unchanged.
const contextCheckpointNoteSQL = `(COALESCE(a.metadata->>'checkpoint_note','false')='true'
 OR (a.tool='TodoWrite' AND a.kind IN ('tool_use','command') AND strpos(COALESCE(a.detail,''),'"checkpoint"')>0))`

// A stamp is server generated from the exact source version read by the Agent.
// Accepting only a node ID when saving would relabel stale claims as fresh.
type CheckpointSource struct {
	Ref   string `json:"ref"`
	Stamp string `json:"stamp"`
	Kind  string `json:"kind,omitempty"`
}

type CheckpointSources struct {
	AccessRevision string             `json:"access_revision"`
	ContextStamp   string             `json:"context_stamp"`
	OwnerStamp     string             `json:"owner_stamp,omitempty"`
	Sources        []CheckpointSource `json:"sources"`
}

// Same stamp as ContextNode. No evidence/report plaintext leaves this query.
// The hash is freshness metadata, not a credential or proof of the claim.
const checkpointNodeStampSQL = `md5(jsonb_build_array(n.id,n.exploration_id,n.kind,n.state,
 COALESCE(gr.generation,0),COALESCE(gp.source_version,0),COALESCE(ds.revision,0),COALESCE(NULLIF(gp.source_hash,''),md5(n.payload::text)),
 COALESCE(f.id,0),md5(COALESCE(f.report,'')),COALESCE(f.evidence_version,0),COALESCE(f.status,''))::text)`

// CheckpointSourceSnapshot batches at most 16 references plus the Worker owner and
// reuses the standard live ancestor/source-task authorization. It never scans
// the task's node bodies to construct a checkpoint or infer a relationship.
func (s *ExplorationStore) CheckpointSourceSnapshot(ctx context.Context, ids []int64, owners ...int64) (CheckpointSources, error) {
	if len(ids) > 17 {
		return CheckpointSources{}, fmt.Errorf("checkpoint supports at most 16 sources plus its owner")
	}
	if ids == nil {
		ids = []int64{}
	}
	owner := int64(0)
	if len(owners) > 0 {
		owner = owners[0]
	}
	roots := `SELECT n.id,n.exploration_id,n.state,n.kind,n.created_at,c.owner
 FROM exploration_nodes n JOIN contexts c ON c.exp=n.exploration_id
 WHERE n.id=ANY($2::bigint[]) AND $3::boolean AND $4::boolean
 AND (n.exploration_id=$1 OR n.kind<>'intent' OR n.state IN ('done','blocked','exhausted','stopped'))`
	query := contextVisibility(roots, s.workerRead) + ` SELECT jsonb_build_object(
 'access_revision',(` + graphAccessRevisionSQL + `),
 'context_stamp',concat('checkpoint-context-v1:',
 (SELECT md5(jsonb_build_array(t.goal,e.description,e.goal,t.company_id)::text) FROM tasks t JOIN explorations e ON e.id=t.exploration_id WHERE t.exploration_id=$1 AND t.deleted_at IS NULL),
 ':',(SELECT md5(COALESCE(string_agg(concat_ws(':',c.id,c.kind,md5(c.text)),'|' ORDER BY c.id),'')) FROM task_constraints c WHERE c.exploration_id=$1)),
 'owner_stamp',COALESCE((SELECT md5(jsonb_build_array(n.payload,n.kind,gr.generation,ds.revision)::text)
 FROM visible v JOIN exploration_nodes n ON n.id=v.id LEFT JOIN graph_revisions gr ON gr.exploration_id=n.exploration_id
 LEFT JOIN graph_dependency_state ds ON ds.node_id=n.id AND ds.exploration_id=n.exploration_id
 WHERE n.id=$6 AND n.exploration_id=$1 AND n.kind='intent'),''),
 'sources',COALESCE((SELECT jsonb_agg(jsonb_build_object('ref','node:'||n.id,'kind',n.kind,'stamp',` + checkpointNodeStampSQL + `) ORDER BY n.id)
 FROM visible v JOIN exploration_nodes n ON n.id=v.id
 LEFT JOIN graph_node_projection gp ON gp.node_id=n.id AND gp.exploration_id=n.exploration_id AND gp.kind=n.kind AND gp.state=n.state
 LEFT JOIN graph_dependency_state ds ON ds.node_id=n.id AND ds.exploration_id=n.exploration_id
 LEFT JOIN graph_revisions gr ON gr.exploration_id=n.exploration_id
 LEFT JOIN LATERAL (SELECT id,report,evidence_version,status FROM findings WHERE node_id=n.id ORDER BY id LIMIT 1) f ON true),'[]'::jsonb))`
	var raw []byte
	var out CheckpointSources
	err := s.contextRow(ctx, query, s.expID, ids, true, true, int64(0), owner).Scan(&raw)
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	return out, err
}

type ExecutionCheckpoint struct {
	Version        int64              `json:"version"`
	Body           json.RawMessage    `json:"body"`
	Sources        []CheckpointSource `json:"sources"`
	AccessRevision string             `json:"access_revision"`
	ContextStamp   string             `json:"context_stamp"`
	SavedAt        time.Time          `json:"saved_at"`
}

func validCheckpointOwner(role string, intent int64) bool {
	return role == "planner" && intent == 0 || role == "worker" && intent > 0
}

func (s *ExplorationStore) ExecutionCheckpoint(ctx context.Context, role string, intent int64) (*ExecutionCheckpoint, error) {
	if !validCheckpointOwner(role, intent) {
		return nil, fmt.Errorf("invalid checkpoint owner")
	}
	ctx, cancel := graphReadContext(ctx)
	defer cancel()
	var out ExecutionCheckpoint
	var sources []byte
	err := s.db.QueryRowContext(ctx, `SELECT version,body,sources,access_revision,context_stamp,saved_at
 FROM agent_execution_checkpoints WHERE exploration_id=$1 AND role=$2 AND intent_id=$3`, s.expID, role, intent).
		Scan(&out.Version, &out.Body, &sources, &out.AccessRevision, &out.ContextStamp, &out.SavedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(sources, &out.Sources); err != nil {
		return nil, err
	}
	return &out, nil
}

// SaveExecutionCheckpoint is a compare-and-set, not a source write. There is no
// model call or graph mutation under this short transaction. Current source
// stamps are checked by the host before save and again on every model request.
func (s *ExplorationStore) SaveExecutionCheckpoint(ctx context.Context, role string, intent, base int64, body json.RawMessage, sources CheckpointSources) (int64, error) {
	if !validCheckpointOwner(role, intent) || base < 0 || !json.Valid(body) || len(body) > 32768 || len(sources.Sources) > 16 || sources.AccessRevision == "" || sources.ContextStamp == "" {
		return 0, fmt.Errorf("invalid execution checkpoint")
	}
	rawSources, err := json.Marshal(sources.Sources)
	if err != nil {
		return 0, err
	}
	ctx, cancel := graphReadContext(ctx)
	defer cancel()
	var version int64
	query := `INSERT INTO agent_execution_checkpoints(exploration_id,role,intent_id,version,body,sources,access_revision,context_stamp)
 SELECT $1,$2,$3,1,$5,$6,$7,$8 WHERE $4::bigint=0
 AND EXISTS(SELECT 1 FROM tasks t WHERE t.exploration_id=$1 AND t.deleted_at IS NULL)
 AND ($3::bigint=0 OR EXISTS(SELECT 1 FROM exploration_nodes n WHERE n.exploration_id=$1 AND n.id=$3 AND n.kind='intent'))
 ON CONFLICT(exploration_id,role,intent_id) DO NOTHING RETURNING version`
	if base > 0 {
		query = `UPDATE agent_execution_checkpoints SET version=version+1,body=$5,sources=$6,access_revision=$7,context_stamp=$8,saved_at=clock_timestamp()
 WHERE exploration_id=$1 AND role=$2 AND intent_id=$3 AND version=$4
 AND EXISTS(SELECT 1 FROM tasks t WHERE t.exploration_id=$1 AND t.deleted_at IS NULL)
 AND ($3::bigint=0 OR EXISTS(SELECT 1 FROM exploration_nodes n WHERE n.exploration_id=$1 AND n.id=$3 AND n.kind='intent'))
 RETURNING version`
	}
	err = s.db.QueryRowContext(ctx, query, s.expID, role, intent, base, string(body), string(rawSources), sources.AccessRevision, sources.ContextStamp).Scan(&version)
	if err == sql.ErrNoRows {
		return 0, ErrCheckpointConflict
	}
	return version, err
}
