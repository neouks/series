package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
)

// Tool aliases are for historical attribution only, never executable aliases.
var RetiredToolReplacements = map[string][]string{
	"list_facts": {"context_search"}, "list_findings": {"context_search"}, "list_worker_traces": {"context_search"}, "list_goals": {"context_search"},
	"get_worker_trace": {"context_search", "context_get"}, "search_all_worker_traces": {"context_search"},
	"node_detail": {"context_get"}, "get_worker_output": {"context_get"}, "expand_digest": {"context_get"},
	"list_assets": {"asset_search", "asset_get"}, "list_untested_assets": {"asset_search"},
	"traffic_blob": {"traffic_get"}, "get_task_graph": {"graph_overview"}, "list_task_findings": {"context_search"},
	"get_task_worker_trace": {"context_search", "context_get"}, "list_task_worker_traces": {"context_search"}, "search_task_worker_traces": {"context_search"}, "get_task_node_detail": {"context_get"}, "goal_met": {},
}

const ContextCapabilitiesSetting = "tool_context_capabilities_v1"

type ContextToolGrants struct {
	Local []string `json:"local"`
	Cross []string `json:"cross"`
}

var oldContextGrants = map[string][]string{
	"list_facts": {"fact"}, "list_findings": {"finding"}, "list_worker_traces": {"intent"}, "list_goals": {"goal"},
	"get_worker_trace": {"activity", "activity_detail"}, "search_all_worker_traces": {"activity", "activity_all"},
	"node_detail": {"detail"}, "get_worker_output": {"output"}, "expand_digest": {"members"}, "graph_overview": {"overview"},
	"list_assets": {"assets"}, "list_untested_assets": {"untested"},
}
var crossContextOld = map[string]string{"get_task_graph": "graph_overview", "list_task_findings": "list_findings", "get_task_worker_trace": "get_worker_trace", "list_task_worker_traces": "list_worker_traces", "search_task_worker_traces": "search_all_worker_traces", "get_task_node_detail": "node_detail"}

// MigrateContextTools is atomic and repeatable. Preserve explicit disabled tools
// and per-capability binding; never union permissions merely because names merge.
func (d *DB) MigrateContextTools() error {
	const flag = "tool_context_catalog_v1"
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`SELECT pg_advisory_xact_lock(719047221)`); err != nil {
		return err
	}
	var done bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM settings WHERE key=$1 AND value='true')`, flag).Scan(&done); err != nil {
		return err
	}
	if done {
		return tx.Commit()
	}
	var collision string
	err = tx.QueryRow(`SELECT key FROM tools WHERE NOT system AND key=ANY($1::text[]) LIMIT 1`, []string{"context_search", "context_get", "asset_search", "asset_get", "graph_overview", "traffic_get", "traffic_search"}).Scan(&collision)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if collision != "" {
		return fmt.Errorf("内置工具名称 %s 与自定义工具冲突；请先修改自定义工具 Key", collision)
	}
	rows, err := tx.Query(`SELECT key,agents,enabled FROM tools WHERE system`)
	if err != nil {
		return err
	}
	type entry struct {
		key     string
		agents  []string
		enabled bool
	}
	all := []entry{}
	legacy := false
	for rows.Next() {
		var e entry
		var b []byte
		if err = rows.Scan(&e.key, &b, &e.enabled); err != nil {
			rows.Close()
			return err
		}
		if err = json.Unmarshal(b, &e.agents); err != nil {
			rows.Close()
			return err
		}
		all = append(all, e)
		if _, ok := RetiredToolReplacements[e.key]; ok {
			legacy = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if legacy {
		policies := map[string]ContextToolGrants{}
		bindings := map[string]map[string]bool{}
		for _, targets := range RetiredToolReplacements {
			for _, target := range targets {
				bindings[target] = map[string]bool{}
			}
		}
		// Existing graph_overview and traffic_get also remain independently bindable.
		for _, e := range all {
			targets, retired := RetiredToolReplacements[e.key]
			if e.key == "graph_overview" || e.key == "traffic_get" {
				targets = []string{e.key}
				retired = true
			}
			if !retired || !e.enabled {
				continue
			}
			for _, role := range e.agents {
				for _, target := range targets {
					bindings[target][role] = true
				}
			}
		}
		for _, e := range all {
			if !e.enabled {
				continue
			}
			key := e.key
			cross := false
			if k, ok := crossContextOld[key]; ok {
				key = k
				cross = true
			}
			caps := oldContextGrants[key]
			for _, role := range e.agents {
				p := policies[role]
				if cross {
					p.Cross = append(p.Cross, caps...)
				} else {
					p.Local = append(p.Local, caps...)
				}
				policies[role] = p
			}
		}
		b, _ := json.Marshal(policies)
		if _, err = tx.Exec(`INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, ContextCapabilitiesSetting, string(b)); err != nil {
			return err
		}
		for target, set := range bindings {
			agents := []string{}
			for role := range set {
				agents = append(agents, role)
			}
			sort.Strings(agents)
			b, _ = json.Marshal(agents)
			if _, err = tx.Exec(`UPDATE tools SET agents=$2,enabled=$3 WHERE key=$1 AND system`, target, b, len(agents) > 0); err != nil {
				return err
			}
		}
	}
	for key := range RetiredToolReplacements {
		if _, err = tx.Exec(`DELETE FROM tools WHERE key=$1 AND system`, key); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT(key) DO UPDATE SET value='true'`, flag); err != nil {
		return fmt.Errorf("迁移工具目录: %w", err)
	}
	return tx.Commit()
}

func CanonicalToolUsageKey(key string) string {
	if key == "get_worker_trace" || key == "get_task_worker_trace" {
		return "context_get"
	}
	if keys, ok := RetiredToolReplacements[key]; ok && len(keys) > 0 {
		return keys[0]
	}
	return key
}

func DefaultContextGrants(role string) ContextToolGrants {
	p := ContextToolGrants{Local: []string{"assets"}}
	switch role {
	case "planner":
		p.Local = append(p.Local, "fact", "finding", "intent", "goal", "hint", "digest", "activity", "activity_all", "activity_detail", "detail", "output", "members", "overview", "untested")
	case "mainagent":
		p.Local = append(p.Local, "fact", "finding", "intent", "goal", "hint", "digest", "activity", "activity_all", "activity_detail", "detail", "output", "members", "overview", "untested")
	case "worker":
		p.Local = append(p.Local, "finding", "activity", "activity_all", "activity_detail", "detail")
	case "pentest":
		p.Local = append(p.Local, "finding", "detail")
	case "auto", "reporter":
		p.Local = append(p.Local, "finding", "detail")
		p.Cross = []string{"finding", "intent", "activity", "activity_all", "activity_detail", "detail", "overview"}
	default:
		p.Local = []string{"fact", "finding", "intent", "goal", "hint", "digest", "activity", "activity_all", "activity_detail", "detail", "output", "members", "overview", "assets", "untested"}
	}
	return p
}

// An explicit new binding grants the new tool's local capabilities. Unchanged
// bindings keep their migrated subset, except Planner's bound readers need local
// intent search and normal asset search to inspect and manage its frontier.
func updateContextBindingGrants(tx *sql.Tx, key string, agents json.RawMessage, enabled bool) error {
	caps := map[string][]string{
		"context_search": {"fact", "finding", "intent", "goal", "hint", "digest", "activity", "activity_all"},
		"context_get":    {"detail", "activity_detail", "output", "members"},
		"asset_search":   {"assets", "untested"}, "asset_get": {"assets"}, "graph_overview": {"overview"},
	}[key]
	if len(caps) == 0 || !enabled {
		return nil
	}
	var raw, oldAgents []byte
	var oldEnabled, system bool
	err := tx.QueryRow(`SELECT agents,enabled,system FROM tools WHERE key=$1 FOR UPDATE`, key).Scan(&oldAgents, &oldEnabled, &system)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if !system {
		return nil
	}
	err = tx.QueryRow(`SELECT value FROM settings WHERE key=$1 FOR UPDATE`, ContextCapabilitiesSetting).Scan(&raw)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	policies := map[string]ContextToolGrants{}
	if len(raw) > 0 {
		if err = json.Unmarshal(raw, &policies); err != nil {
			return err
		}
	}
	if policies == nil {
		policies = map[string]ContextToolGrants{}
	}
	var old, next []string
	if err = json.Unmarshal(oldAgents, &old); err != nil {
		return err
	}
	if err = json.Unmarshal(agents, &next); err != nil {
		return err
	}
	changed := false
	required := map[string]string{"asset_search": "assets", "context_search": "intent"}[key]
	for _, role := range next {
		p, exists := policies[role]
		if !exists {
			p = DefaultContextGrants(role)
		}
		if role == "planner" && required != "" && !slices.Contains(p.Local, required) {
			p.Local = append(p.Local, required)
			policies[role] = p
			changed = true
		}
		if slices.Contains(old, role) {
			// A disable/re-enable preserves any migrated subset. Only a
			// previously disabled, ungranted capability needs a fresh grant.
			hasGrant := false
			for _, cap := range caps {
				hasGrant = hasGrant || slices.Contains(p.Local, cap) || slices.Contains(p.Cross, cap)
			}
			if oldEnabled || hasGrant {
				continue
			}
		}
		p.Local = append(p.Local, caps...)
		sort.Strings(p.Local)
		p.Local = slices.Compact(p.Local)
		policies[role] = p
		changed = true
	}
	if !changed {
		return nil
	}
	raw, err = json.Marshal(policies)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, ContextCapabilitiesSetting, string(raw))
	return err
}

const plannerAssetSearchMigration = "tool_planner_asset_search_v1"
const plannerIntentSearchMigration = "tool_planner_intent_search_v1"

// MigratePlannerAssetSearch repairs the legacy untested-only Planner binding.
// It grants only local asset reads, never enables or binds a disabled tool, and
// leaves all other roles and cross-task grants intact. No asset approval changes.
func (d *DB) MigratePlannerAssetSearch() error {
	return d.migratePlannerContextRead(plannerAssetSearchMigration, "asset_search")
}

// MigratePlannerIntentSearch restores local intent reads for an enabled Planner
// context_search binding. Legacy list readers did not grant this capability even
// though Planner must inspect queued/running intents before steering/stopping them.
// Other roles, cross-task grants, disabled tools and custom tools are preserved.
func (d *DB) MigratePlannerIntentSearch() error {
	return d.migratePlannerContextRead(plannerIntentSearchMigration, "context_search")
}

func (d *DB) migratePlannerContextRead(flag, toolKey string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`SELECT pg_advisory_xact_lock(719047221)`); err != nil {
		return err
	}
	var done bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM settings WHERE key=$1 AND value='true')`, flag).Scan(&done); err != nil {
		return err
	}
	if done {
		return tx.Commit()
	}
	var agents json.RawMessage
	err = tx.QueryRow(`SELECT agents FROM tools WHERE key=$1 AND system AND enabled AND agents ? 'planner' FOR UPDATE`, toolKey).Scan(&agents)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		if err = updateContextBindingGrants(tx, toolKey, agents, true); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT(key) DO UPDATE SET value='true'`, flag); err != nil {
		return err
	}
	return tx.Commit()
}
