package db

import (
	"database/sql"
	"errors"
	"fmt"
)

// Match asset mutation's lock order: evidence -> asset scope -> task. The
// model review has already finished; no network work runs under these locks.
func LockFindingRecordTx(tx *sql.Tx, in RecordFindingInput) error {
	if in.AccessRole != "" {
		if err := lockCompanyScopeMutation(tx); err != nil {
			return err
		}
	}
	return LockTaskEvidenceTx(tx, in.TaskID)
}

// Run against the committing transaction, not a pool read before it waits for
// locks. This closes the review/commit race for revoked assets and stopped Workers.
func ValidateFindingAccessTx(tx *sql.Tx, in RecordFindingInput) error {
	if in.AccessRole == "" {
		return nil
	} // Non-agent import/legacy DB callers.
	if in.TaskID <= 0 {
		return errors.New("漏洞登记缺少任务权限上下文")
	}
	execution := in.AccessRole == "mainagent" || in.AccessRole == "worker"
	if !execution && in.AccessRole != "planner" {
		return errors.New("漏洞登记角色无效")
	}
	assets := &AssetStore{tx: tx, workerRead: execution}
	var err error
	if execution {
		err = assets.ValidateWorkerAssets(in.TaskID, in.AssetIDs)
	} else {
		err = assets.ValidateTaskAssetsApproved(in.TaskID, in.AssetIDs)
	}
	if err != nil {
		return fmt.Errorf("漏洞资产未获授权：%w", err)
	}
	if in.AccessRole == "worker" {
		var state string
		if err := tx.QueryRow(`SELECT state FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 AND kind='intent' FOR UPDATE`, in.IntentID, in.ExplorationID).Scan(&state); err != nil {
			return err
		}
		if state != "running" {
			return errors.New("Worker 意图已停止，不能写回漏洞")
		}
	}
	if in.IntentID > 0 {
		access, err := assets.TaskNodeAccess(in.TaskID, []int64{in.IntentID})
		if err != nil {
			return err
		}
		if !access[in.IntentID].CanRead {
			return errors.New("intent_id 关联的任务资产未获授权或已被封禁")
		}
	}
	return nil
}
