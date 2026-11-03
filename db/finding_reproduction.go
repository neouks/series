package db

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Reproduction is immutable, recorded at discovery time. Wire evidence is kept
// verbatim; only explicit traffic references are resolved, never fuzzy matches.
type FindingReproduction struct {
	EntryPoint    string                    `json:"entry_point,omitempty"`
	Prerequisites []string                  `json:"prerequisites,omitempty"`
	Steps         []FindingReproductionStep `json:"steps"`
}

// Traffic is a shared store: a known ID alone must not authorize an Agent to
// copy another task's restricted target into a step. Apply the same live host
// policy as other execution reads, under the existing asset mutation lock.
func validateReproductionTrafficTx(tx *sql.Tx, in RecordFindingInput, prepared []PreparedTrafficEvidence) error {
	if in.Reproduction == nil || in.AccessRole == "" {
		return nil
	}
	wanted := map[string]bool{}
	for _, ref := range in.Reproduction.TrafficReferences() {
		wanted[strings.TrimSpace(ref.TrafficID)] = true
	}
	var hosts []string
	for _, item := range prepared {
		if !wanted[item.Snapshot.SourceTrafficID] {
			continue
		}
		u, err := url.Parse(item.Snapshot.URL)
		if err != nil || u.Hostname() == "" {
			return fmt.Errorf("复现步骤流量目标无效")
		}
		hosts = append(hosts, u.Hostname())
	}
	if len(hosts) == 0 {
		return nil
	}
	assets := &AssetStore{tx: tx}
	execution := in.AccessRole == "worker" || in.AccessRole == "mainagent"
	// Unknown execution hosts can legitimately be pending, but a shared traffic
	// ID is not evidence that its target is in this task's visible asset scope.
	// Require an actual local/direct-source registration and check its effective
	// state (including source-task approval) in one batch before the host rules.
	var scoped bool
	err := tx.QueryRow(`SELECT NOT EXISTS (
 SELECT 1 FROM unnest($2::text[]) requested(host) WHERE NOT EXISTS (
  SELECT 1 FROM task_asset_links link JOIN assets a ON a.id=link.asset_id
  WHERE (link.task_id=$1 OR link.task_id IN (SELECT source_task_id FROM task_relations WHERE task_id=$1))
  AND task_asset_host_within(requested.host,task_asset_host(a))
  AND (task_asset_effective_approval_state($1,a.id)='approved'
       OR ($3 AND task_asset_effective_approval_state($1,a.id)='pending'))
 ))`, in.TaskID, hosts, execution).Scan(&scoped)
	if err != nil {
		return err
	}
	if !scoped {
		return fmt.Errorf("复现步骤流量目标不在本任务授权资产范围；请核对真实资产登记及审批，不能引用无关任务流量")
	}
	if execution {
		return assets.ValidateWorkerHosts(in.TaskID, hosts)
	}
	return assets.ValidateTaskHostsApproved(in.TaskID, hosts)
}

type FindingReproductionStep struct {
	Title       string       `json:"title"`
	Action      string       `json:"action"`
	Result      string       `json:"result"`
	Proves      string       `json:"proves,omitempty"`
	Request     string       `json:"request,omitempty"`
	Response    string       `json:"response,omitempty"`
	Command     string       `json:"command,omitempty"`
	Output      string       `json:"output,omitempty"`
	TrafficRefs []TrafficRef `json:"traffic_refs,omitempty"`
	BindingIDs  []string     `json:"binding_ids,omitempty"` // server-owned, scoped to this finding
}

func DecodeFindingReproduction(raw json.RawMessage) (*FindingReproduction, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("reproduction 必须为有效JSON")
	}
	if len(raw) > 1024*1024 {
		return nil, fmt.Errorf("reproduction 最多1MiB，请用流量引用保留长报文")
	}
	var out FindingReproduction
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("reproduction 格式错误: %w", err)
	}
	if err := out.ValidateInput(); err != nil {
		return nil, err
	}
	return &out, nil
}

func (r *FindingReproduction) ValidateInput() error {
	if r == nil {
		return nil
	}
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > 1024*1024 {
		return fmt.Errorf("reproduction 最多1MiB")
	}
	if len(r.Steps) == 0 || len(r.Steps) > 50 || len(r.Prerequisites) > 50 {
		return fmt.Errorf("reproduction.steps 必须为1..50步，前置条件最多50条")
	}
	for i, step := range r.Steps {
		if strings.TrimSpace(step.Title) == "" || strings.TrimSpace(step.Action) == "" || strings.TrimSpace(step.Result) == "" {
			return fmt.Errorf("reproduction.steps[%d] 需要 title/action/result；缺失的真实结果须明确说明", i)
		}
		if len(step.BindingIDs) > 0 {
			return fmt.Errorf("reproduction.steps[%d].binding_ids 由服务端生成，请使用真实 traffic_refs", i)
		}
		if _, err := NormalizeTrafficRefs(step.TrafficRefs); err != nil {
			return fmt.Errorf("reproduction.steps[%d].traffic_refs: %w", i, err)
		}
	}
	return nil
}

func (r *FindingReproduction) TrafficReferences() []TrafficRef {
	var refs []TrafficRef
	if r != nil {
		for _, step := range r.Steps {
			refs = append(refs, step.TrafficRefs...)
		}
	}
	return refs
}

// Resolve returns a copy; failed transactions must not mutate the caller's input.
func (r *FindingReproduction) Resolve(bindings []FindingTrafficBinding) (json.RawMessage, error) {
	if r == nil {
		return nil, nil
	}
	raw, _ := json.Marshal(r)
	var out FindingReproduction
	_ = json.Unmarshal(raw, &out)
	byTraffic := map[string]string{}
	for _, b := range bindings {
		byTraffic[b.Snapshot.SourceTrafficID] = fmt.Sprint(b.ID)
	}
	for i := range out.Steps {
		step := &out.Steps[i]
		seen := map[string]bool{}
		for _, ref := range step.TrafficRefs {
			id := byTraffic[strings.TrimSpace(ref.TrafficID)]
			if id == "" {
				return nil, fmt.Errorf("reproduction.steps[%d] 的流量未绑定到当前漏洞", i)
			}
			if !seen[id] {
				step.BindingIDs = append(step.BindingIDs, id)
				seen[id] = true
			}
		}
		step.TrafficRefs = nil
	}
	return json.Marshal(out)
}
