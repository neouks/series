package agent

import (
	"slices"

	actool "github.com/Autumn-27/norma/tool"
)

// OnDemandTools runs after role/configuration filtering. Core tools keep native schemas
// and concise help; less frequent tools load their full definitions on demand.
// Existing deferred entries retain their gates, including skill-owned MCPs.
func OnDemandTools(tools []actool.CoreTool, def DeferredInfo, roles ...string) ([]actool.CoreTool, DeferredInfo) {
	role := ""
	if len(roles) > 0 {
		role = roles[0]
	}
	docs := map[string]string{}
	previous := make(map[string]bool, len(def.Deferred))
	global := make(map[string]bool, len(def.GlobalNames))
	for _, name := range def.Deferred {
		previous[name] = true
	}
	for _, name := range def.GlobalNames {
		global[name] = true
	}
	if def.Unlock == nil {
		def.Unlock = actool.NewUnlockSet(def.GlobalNames...)
	}
	def.Deferred, def.GlobalNames = nil, nil
	out := make([]actool.CoreTool, 0, len(tools))
	seen := map[string]bool{}
	for _, tool := range tools {
		name := tool.Name()
		if seen[name] {
			continue
		}
		seen[name] = true
		if policy := builtinPolicy(tool); policy != nil && policy.options.Known[name] {
			out = append(out, tool)
			continue
		}
		if name == actool.SearchExtraToolsName || name == actool.ExecuteExtraToolName || name == toolHelpName {
			out = append(out, tool)
			continue
		}
		// The SDK discovery tool reads Description but not Prompt. Fold extended
		// help into the deferred description so Skill's directory and SDK usage
		// instructions remain discoverable without being loaded every turn.
		guarded, ok := tool.(*guardedTool)
		if !ok {
			guarded = &guardedTool{CoreTool: tool}
		}
		if coreTool(name, role) && !previous[name] {
			if name == "Skill" {
				// Preserve the registry's name/applicability directory, never body.
				out = append(out, tool)
			} else if _, already := tool.(*residentTool); already {
				out = append(out, tool)
			} else {
				full := tool.Description()
				if prompt := tool.Prompt(); prompt != "" {
					full += "\n\n" + prompt
				}
				docs[name] = full
				brief := compactToolText(tool.Description(), 220)
				if brief == "" {
					brief = name + " 工具。"
				}
				out = append(out, &residentTool{guardedTool: guarded, description: brief + " 完整用法及配置见 GetToolHelp。"})
			}
			continue
		}
		out = append(out, &discoveredTool{guardedTool: guarded})
		def.Deferred = append(def.Deferred, name)
		if !previous[name] || global[name] {
			def.GlobalNames = append(def.GlobalNames, name)
			def.Unlock.Add(name)
		}
	}
	if len(docs) > 0 && !seen[toolHelpName] {
		out = append(out, guardPanic(newToolHelp(docs)))
	}
	slices.Sort(def.Deferred)
	slices.Sort(def.GlobalNames)
	return out, def
}

type discoveredTool struct{ *guardedTool }

func (t *discoveredTool) Description() string {
	desc := t.CoreTool.Description()
	if prompt := t.CoreTool.Prompt(); prompt != "" {
		desc += "\n\n" + prompt
	}
	return desc
}
func (*discoveredTool) Prompt() string { return "" }
