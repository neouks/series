package db

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// MigrateContextPrompts appends defaults as a new prompt version. Custom text
// and prior versions remain unchanged; the report contains names, never content.
func (d *DB) MigrateContextPrompts(defaults, previousHashes map[string]string) error {
	const flag = "tool_context_prompts_v1"
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
		if next != "" && a.text != next && previousHashes[a.key] == hex.EncodeToString(sum[:]) {
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
				if strings.Contains(a.text, old) {
					refs = append(refs, old)
				}
			}
			// A renamed parameter also requires review in customized reporter prompts.
			if strings.Contains(a.text, "update_finding_report") && strings.Contains(a.text, "finding_id") {
				refs = append(refs, "update_finding_report.finding_node_id")
			}
			sort.Strings(refs)
			if len(refs) > 0 && a.text != next {
				report[a.key] = map[string]any{"action": "custom_preserved", "references": refs}
			}
		}
	}
	b, _ := json.Marshal(report)
	if _, e = tx.Exec(`INSERT INTO settings(key,value) VALUES('tool_context_prompt_migration_report',$1) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, string(b)); e != nil {
		return e
	}
	if _, e = tx.Exec(`INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT(key) DO UPDATE SET value='true'`, flag); e != nil {
		return e
	}
	return tx.Commit()
}
