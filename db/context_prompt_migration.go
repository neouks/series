package db

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"sort"
)

// MigrateContextPrompts appends defaults as a new prompt version. Custom text
// and prior versions remain unchanged; the report contains names, never content.
func (d *DB) MigrateContextPrompts(defaults, previousHashes map[string]string) error {
	hashes := map[string][]string{}
	for key, hash := range previousHashes {
		hashes[key] = []string{hash}
	}
	return d.migratePromptContracts("tool_context_prompts_v1", "tool_context_prompt_migration_report", defaults, hashes)
}

// Separate version: deployments that already completed the tool rename still
// need corrected code defaults. Only exact known defaults get a new version.
func (d *DB) MigrateBuiltinPromptContracts(defaults map[string]string, hashes map[string][]string) error {
	return d.migratePromptContracts("tool_prompt_contracts_v2", "tool_prompt_contracts_report_v2", defaults, hashes)
}

func (d *DB) migratePromptContracts(flag, reportKey string, defaults map[string]string, previousHashes map[string][]string) error {
	tx, e := d.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`SELECT pg_advisory_xact_lock(719047221)`); e != nil {
		return e
	}
	var done bool
	if e = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM settings WHERE key=$1 AND value='true')`, flag).Scan(&done); e != nil {
		return e
	}
	if done {
		return tx.Commit()
	}
	rows, e := tx.Query(`SELECT a.id,a.key,p.template_text FROM agents a JOIN agent_prompts p ON p.id=a.current_prompt_id FOR UPDATE OF a`)
	if e != nil {
		return e
	}
	type entry struct {
		id        int64
		key, text string
	}
	var entries []entry
	for rows.Next() {
		var a entry
		if e = rows.Scan(&a.id, &a.key, &a.text); e != nil {
			rows.Close()
			return e
		}
		entries = append(entries, a)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	report := map[string]any{}
	for _, a := range entries {
		sum := sha256.Sum256([]byte(a.text))
		next := defaults[a.key]
		if next != "" && a.text != next && slices.Contains(previousHashes[a.key], hex.EncodeToString(sum[:])) {
			var pid int64
			e = tx.QueryRow(`INSERT INTO agent_prompts(agent_id,version,template_text,note,updated_by) SELECT $1,COALESCE(max(version),0)+1,$2,'上下文工具迁移','system' FROM agent_prompts WHERE agent_id=$1 RETURNING id`, a.id, next).Scan(&pid)
			if e != nil {
				return e
			}
			if _, e = tx.Exec(`UPDATE agents SET current_prompt_id=$1 WHERE id=$2`, pid, a.id); e != nil {
				return e
			}
			report[a.key] = map[string]any{"action": "default_updated"}
		} else {
			refs := []string{}
			for old := range RetiredToolReplacements {
				if regexp.MustCompile(`\b` + regexp.QuoteMeta(old) + `\b`).MatchString(a.text) {
					refs = append(refs, old)
				}
			}
			if regexp.MustCompile(`\bupdate_skill\b`).MatchString(a.text) {
				refs = append(refs, "update_skill -> update_skill_file")
			}
			// Do not flag a correct report prompt just because get_finding_traffic
			// legitimately uses the independent finding_id in another sentence.
			if legacyFindingReportParameter.MatchString(a.text) {
				refs = append(refs, "update_finding_report.finding_node_id")
			}
			sort.Strings(refs)
			if len(refs) > 0 && a.text != next {
				report[a.key] = map[string]any{"action": "custom_preserved", "references": refs}
			}
		}
	}
	b, _ := json.Marshal(report)
	if _, e = tx.Exec(`INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, reportKey, string(b)); e != nil {
		return e
	}
	if _, e = tx.Exec(`INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT(key) DO UPDATE SET value='true'`, flag); e != nil {
		return e
	}
	return tx.Commit()
}

var legacyFindingReportParameter = regexp.MustCompile(`(?s)\bupdate_finding_report\s*\([^)]*\bfinding_id\s*(?:=|,|\))`)
