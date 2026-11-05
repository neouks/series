package agent

import "strings"

// Keep protocol diagnostics at their original positions, never the successful
// evidence body beside them. These records are not charged as disposable data:
// an unusually long real refusal can still exceed the soft request budget.
func retrievalDiagnostics(value map[string]any, name string) map[string]any {
	var project func(map[string]any) (map[string]any, bool)
	project = func(row map[string]any) (map[string]any, bool) {
		out := map[string]any{}
		failed := false
		for _, k := range []string{"ref", "id", "node_id", "asset_id", "intent_id", "step_id", "source_task_id"} {
			if v, ok := row[k]; ok {
				out[k] = v
			}
		}
		for k, v := range row {
			if k == "error" || strings.HasSuffix(k, "_error") || k == "unavailable" || k == "errors" {
				if v != nil && v != "" && v != false {
					out[k] = v
					failed = true
				}
			}
			if (k == "partial" || k == "context_unavailable") && v == true {
				out[k] = true
				failed = true
			}
		}
		for _, k := range []string{"payload", "details"} {
			if child, ok := row[k].(map[string]any); ok {
				if p, bad := project(child); bad {
					out[k] = p
					failed = true
				}
			}
		}
		for _, k := range []string{"items", "assets", "related_tasks"} {
			if rows, ok := row[k].([]any); ok {
				projected := make([]any, 0, len(rows))
				bad := false
				for _, r := range rows {
					if m, ok := r.(map[string]any); ok {
						p, f := project(m)
						if !f {
							p["history_omitted"] = true
						}
						projected = append(projected, p)
						bad = bad || f
					}
				}
				if bad {
					out[k] = projected
					failed = true
				}
			}
		}
		return out, failed
	}
	out, failed := project(value)
	if !failed {
		return nil
	}
	return out
}
