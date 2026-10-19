package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"time"
)

var ErrSuppressionChanged = errors.New("漏洞禁报规则已变化，请重新检查后提交")

type FindingSuppressionRule struct {
	ID         int64           `json:"id,string"`
	FeedbackID int64           `json:"feedback_id,string"`
	TaskID     *int64          `json:"task_id,omitempty"`
	Title      string          `json:"title"`
	VulnClass  string          `json:"vulnclass"`
	Summary    string          `json:"summary"`
	AssetIDs   []int64         `json:"asset_ids"`
	Targets    []FindingTarget `json:"targets"`
	Reason     string          `json:"reason"`
	CreatedAt  time.Time       `json:"created_at"`
	RevokedAt  *time.Time      `json:"revoked_at,omitempty"`
}

// Stable, minimal target identities survive asset deletion/recreation. Never
// include auth, params, request/response bodies, URL userinfo or query strings.
type FindingTarget struct {
	Type     string `json:"type"`
	Domain   string `json:"domain,omitempty"`
	IP       string `json:"ip,omitempty"`
	Port     *int   `json:"port,omitempty"`
	URL      string `json:"url,omitempty"`
	BundleID string `json:"bundle_id,omitempty"`
}
type findingTargetQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func findingTargets(ctx context.Context, q findingTargetQuery, ids []int64) ([]FindingTarget, error) {
	out := []FindingTarget{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT type,COALESCE(domain,''),COALESCE(ip,''),port,COALESCE(url,''),COALESCE(bundle_id,'') FROM assets WHERE id=ANY($1::bigint[]) ORDER BY id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var target FindingTarget
		if err := rows.Scan(&target.Type, &target.Domain, &target.IP, &target.Port, &target.URL, &target.BundleID); err != nil {
			return nil, err
		}
		if u, err := url.Parse(target.URL); err == nil && u.IsAbs() && u.Host != "" {
			u.User = nil
			u.RawQuery = ""
			u.ForceQuery = false
			u.Fragment = ""
			u.RawFragment = ""
			target.URL = u.String()
		} else {
			target.URL = ""
		}
		out = append(out, target)
	}
	return out, rows.Err()
}
func (d *DB) FindingTargets(ctx context.Context, ids []int64) ([]FindingTarget, error) {
	return findingTargets(ctx, d, ids)
}

// Check is server-generated, never decoded from tool arguments. Version binds
// the judgement to the exact active rule set at the final transaction boundary.
type FindingSuppressionCheck struct {
	Status    string  `json:"status"`
	Reason    string  `json:"reason,omitempty"`
	RuleIDs   []int64 `json:"rule_ids,omitempty"`
	Version   string  `json:"-"`
	ElapsedMS int64   `json:"elapsed_ms"`
}

type suppressionQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func suppressionVersion(ctx context.Context, q suppressionQuery, taskID int64) (string, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT COALESCE(md5(string_agg(id::text,',' ORDER BY id)), '') FROM finding_suppression_rules WHERE task_id=$1 AND revoked_at IS NULL`, taskID).Scan(&v)
	return v, err
}

func (d *DB) FindingSuppressionVersion(ctx context.Context, taskID int64) (string, error) {
	return suppressionVersion(ctx, d, taskID)
}

func (d *DB) FindingSuppressionRules(ctx context.Context, taskID, before int64, limit int, active bool) ([]FindingSuppressionRule, error) {
	if taskID <= 0 || before < 0 || limit < 1 || limit > 50 {
		return nil, errors.New("无效的禁报规则查询")
	}
	rows, err := d.QueryContext(ctx, `SELECT id,feedback_id,task_id,title,vulnclass,summary,asset_ids,targets,reason,created_at,revoked_at FROM finding_suppression_rules WHERE task_id=$1 AND ($2::bigint=0 OR id<$2) AND (NOT $4 OR revoked_at IS NULL) ORDER BY id DESC LIMIT $3`, taskID, before, limit, active)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FindingSuppressionRule{}
	for rows.Next() {
		var r FindingSuppressionRule
		var assets, targets []byte
		if err := rows.Scan(&r.ID, &r.FeedbackID, &r.TaskID, &r.Title, &r.VulnClass, &r.Summary, &assets, &targets, &r.Reason, &r.CreatedAt, &r.RevokedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(assets, &r.AssetIDs); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(targets, &r.Targets); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) RevokeFindingSuppression(ctx context.Context, taskID, ruleID int64) error {
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		if err := LockTaskEvidenceTx(tx, taskID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE finding_suppression_rules SET revoked_at=COALESCE(revoked_at,now()) WHERE id=$1 AND task_id=$2`, ruleID, taskID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err == nil && n == 0 {
			return sql.ErrNoRows
		}
		return err
	})
}

// ValidateFindingSuppressionTx runs under the evidence lock before copying
// bodies, and again under the task lock before inserting nodes. Delete/revoke
// use the same evidence/task locks; final writes recheck task availability.
func ValidateFindingSuppressionTx(tx *sql.Tx, in RecordFindingInput) error {
	if in.TaskID <= 0 {
		return nil
	}
	v, err := suppressionVersion(context.Background(), tx, in.TaskID)
	if err != nil {
		return err
	}
	if in.SuppressionCheck == nil {
		if v != "" {
			return ErrSuppressionChanged
		}
		return nil
	}
	if v != in.SuppressionCheck.Version {
		return ErrSuppressionChanged
	}
	if in.SuppressionCheck.Status != "allowed" && in.SuppressionCheck.Status != "unchecked" {
		return errors.New("漏洞命中禁报规则")
	}
	return nil
}
