package db

import (
	"encoding/json"
	"slices"
)

const plannerGraphReadsMigration = "tool_planner_graph_reads_v1"

func (d *DB) MigratePlannerOnDemandPrompt(template string, hashes []string) error {
	return d.migratePromptContracts("planner_on_demand_prompt_v1", "planner_on_demand_prompt_report_v1", map[string]string{"planner": template}, map[string][]string{"planner": hashes})
}

// Only replace implicit overview reads with equivalent task-local searches.
// Disabled/unbound/custom tools and all cross-task grants remain untouched.
func (d *DB) MigratePlannerGraphReads() error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`SELECT pg_advisory_xact_lock(719047221)`); err != nil {
		return err
	}
	var done, eligible bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM settings WHERE key=$1 AND value='true')`, plannerGraphReadsMigration).Scan(&done); err != nil {
		return err
	}
	if done {
		return tx.Commit()
	}
	if err = tx.QueryRow(`SELECT count(*)=2 FROM tools WHERE key IN ('context_search','graph_overview') AND system AND enabled AND agents ? 'planner'`).Scan(&eligible); err != nil {
		return err
	}
	if eligible {
		var raw string
		if err = tx.QueryRow(`SELECT COALESCE((SELECT value FROM settings WHERE key=$1),'{}')`, ContextCapabilitiesSetting).Scan(&raw); err != nil {
			return err
		}
		policies := map[string]ContextToolGrants{}
		if err = json.Unmarshal([]byte(raw), &policies); err != nil {
			return err
		}
		if p, ok := policies["planner"]; ok && slices.Contains(p.Local, "overview") {
			for _, cap := range []string{"hint", "digest"} {
				if !slices.Contains(p.Local, cap) {
					p.Local = append(p.Local, cap)
				}
			}
			policies["planner"] = p
			b, _ := json.Marshal(policies)
			if _, err = tx.Exec(`UPDATE settings SET value=$2 WHERE key=$1`, ContextCapabilitiesSetting, string(b)); err != nil {
				return err
			}
		}
	}
	if _, err = tx.Exec(`INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT(key) DO UPDATE SET value='true'`, plannerGraphReadsMigration); err != nil {
		return err
	}
	return tx.Commit()
}
