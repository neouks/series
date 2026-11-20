package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

type builtinDiscoveryInput struct {
	Query          string `json:"query"`
	Mode           string `json:"mode,omitempty"`
	Limit          int    `json:"limit,omitempty"`
	Offset         int    `json:"offset,omitempty"`
	CatalogVersion string `json:"catalog_version,omitempty"`
}
type builtinCandidate struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
	Active  bool   `json:"active"`
	Matched bool   `json:"matched"`
}
type builtinDiscoveryResult struct {
	Version        int                    `json:"discovery_version"`
	Status         string                 `json:"status"`
	Mode           string                 `json:"mode"`
	Loaded         []string               `json:"loaded"`
	AlreadyLoaded  []string               `json:"already_loaded"`
	Message        string                 `json:"message"`
	Candidates     []builtinCandidate     `json:"candidates,omitempty"`
	Total          int                    `json:"total_candidates,omitempty"`
	MatchCount     int                    `json:"match_count,omitempty"`
	CatalogVersion string                 `json:"catalog_version,omitempty"`
	HasMore        bool                   `json:"has_more"`
	Next           *builtinDiscoveryInput `json:"next,omitempty"`
	Fallback       string                 `json:"fallback,omitempty"`
}

func newBuiltinSearch(prototype *builtinCatalog) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: BuiltinSearchName, Description: "发现或加载当前角色获授权的内置工具。exact 精确加载；keyword 默认加载1个（最多20个）；catalog 分页查看名称与短说明、不加载；auto 自动判断。已知名称优先 exact；关键词按相关度匹配，翻页原样使用 next，不自行更换查询或模式。参数在下一轮工具声明中出现，然后直接调用原工具名。自定义/MCP 使用 SearchExtraTools。",
		Schema: map[string]any{"type": "object", "properties": map[string]any{
			"query":           map[string]any{"type": "string", "description": "完整工具名或关键词；catalog 可为空字符串"},
			"mode":            map[string]any{"type": "string", "enum": []string{"auto", "exact", "keyword", "catalog"}, "default": "auto"},
			"limit":           map[string]any{"type": "integer", "minimum": 1, "maximum": 20, "default": 1},
			"offset":          map[string]any{"type": "integer", "minimum": 0, "default": 0},
			"catalog_version": map[string]any{"type": "string", "description": "翻页使用上次结果返回的版本"},
		}, "required": []string{"query"}, "additionalProperties": false},
		ReadOnly: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, raw json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			p := prototype
			if session, ok := ctx.Value(builtinSessionKey{}).(*builtinCatalog); ok {
				p = session
			}
			if p == nil {
				return actool.Errorf("内置工具目录尚未就绪"), nil
			}
			var in builtinDiscoveryInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return actool.Errorf(fmt.Sprintf("参数错误: %v", err)), nil
			}
			// Defaults apply only when omitted; explicit 0 is an invalid limit.
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(raw, &fields)
			if _, ok := fields["limit"]; !ok {
				in.Limit = 1
			}
			if in.Mode == "" {
				in.Mode = "auto"
			}
			result, err := p.discover(ctx, in)
			if err != nil {
				return actool.Errorf(fmt.Sprintf("发现失败: %v", err)), nil
			}
			encoded, _ := json.Marshal(result)
			out := actool.Text(string(encoded))
			out.IsError = result.Status == "unauthorized" || result.Status == "not_found" || result.Status == "unavailable" || result.Status == "catalog_changed"
			return out, nil
		},
	})
}

func (p *builtinCatalog) discover(ctx context.Context, in builtinDiscoveryInput) (builtinDiscoveryResult, error) {
	out := builtinDiscoveryResult{Version: 1, Mode: in.Mode, Loaded: []string{}, AlreadyLoaded: []string{}}
	in.Query = strings.TrimSpace(in.Query)
	if in.Limit < 1 || in.Limit > 20 || in.Offset < 0 {
		return out, fmt.Errorf("limit 必须为1–20的整数，offset必须非负")
	}
	if in.Query == "" && in.Mode != "catalog" {
		return out, fmt.Errorf("query不能为空；目录请用catalog")
	}
	switch in.Mode {
	case "auto", "exact", "keyword", "catalog":
	default:
		return out, fmt.Errorf("未知mode")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if status, err := p.check(ctx, BuiltinSearchName); err != nil {
		return out, err
	} else if status != "" {
		out.Status = status
		out.Message = "当前角色不能使用工具发现。"
		return out, nil
	}
	if in.Mode == "auto" {
		in.Mode = "keyword"
		if p.options.Known[in.Query] || (strings.Contains(in.Query, "_") && !strings.ContainsAny(in.Query, " \t")) {
			in.Mode = "exact"
		}
	}
	out.Mode = in.Mode
	if in.Mode == "exact" {
		name := in.Query
		if !p.options.Known[name] {
			out.Status = "not_found"
			out.Message = "内置目录不存在此名称；请用keyword/catalog查看，或用SearchExtraTools查找自定义/MCP。"
			return out, nil
		}
		status, err := p.check(ctx, name)
		if err != nil {
			return out, err
		}
		switch {
		case status != "":
			out.Status = status
			out.Message = "工具未授权、已禁用/解绑或服务未就绪；没有加载。"
		case !p.options.Known[name]:
			out.Status = "not_found"
			out.Message = "内置目录不存在此名称，请核对名称或使用keyword/catalog；自定义/MCP使用SearchExtraTools。"
		case !p.eligible[name]:
			out.Status = "unavailable"
			out.Message = "工具实现尚未就绪或当前执行阶段不可用；没有加载。"
		default:
			p.active[name] = p.request
			if p.visible[name] {
				out.Status = "already_loaded"
				out.AlreadyLoaded = []string{name}
				out.Message = "本轮已声明，可直接调用。"
			} else {
				out.Status = "loaded"
				out.Loaded = []string{name}
				out.Message = "已加载；下一轮读取工具声明中的完整参数后直接调用。"
			}
		}
		return out, nil
	}
	names := []string{}
	matched := map[string]bool{}
	scores := map[string]int{}
	for name, schema := range p.schemas {
		if name == BuiltinSearchName || !p.eligible[name] {
			continue
		}
		status, err := p.check(ctx, name)
		if err != nil {
			return out, err
		}
		if status != "" {
			continue
		}
		names = append(names, name)
		score := 0
		if in.Mode != "catalog" {
			score = builtinKeywordScore(in.Query, name, schema.Description)
		}
		scores[name] = score
		match := score > 0
		matched[name] = match
		if match {
			out.MatchCount++
		}
	}
	if out.MatchCount > 0 {
		filtered := names[:0]
		for _, name := range names {
			if matched[name] {
				filtered = append(filtered, name)
			}
		}
		names = filtered
	}
	sort.Slice(names, func(i, j int) bool {
		if scores[names[i]] != scores[names[j]] {
			return scores[names[i]] > scores[names[j]]
		}
		return names[i] < names[j]
	})
	// Version covers metadata and permissions, so paging never silently skips a
	// renamed/disabled/rebound item. Activation itself does not change the version.
	hash := sha256.New()
	// Hash the ordered view, not mode/query spelling. An unmatched keyword
	// fallback and the same catalog page share a cursor safely.
	for _, name := range names {
		raw, _ := json.Marshal(p.schemas[name])
		_, _ = hash.Write(raw)
	}
	version := hex.EncodeToString(hash.Sum(nil))[:16]
	out.CatalogVersion = version
	out.Total = len(names)
	if in.Offset > 0 && in.CatalogVersion != version {
		out.Status = "catalog_changed"
		out.Message = "目录或权限已变化；请从next重新读取。"
		out.Next = &builtinDiscoveryInput{Query: in.Query, Mode: in.Mode, Limit: in.Limit}
		return out, nil
	}
	offset := min(in.Offset, len(names))
	end := min(len(names), offset+max(8, in.Limit))
	out.Status = "candidates"
	out.Message = "目录仅提供名称和短说明；按完整名称精确加载。"
	if in.Mode != "catalog" && out.MatchCount == 0 {
		out.Fallback = "catalog"
		out.Message = "关键词未命中，已返回获授权目录候选；不表示没有该能力。"
	}
	selected := 0
	selection := append([]string(nil), names[offset:end]...)
	sort.SliceStable(selection, func(i, j int) bool {
		if scores[selection[i]] != scores[selection[j]] {
			return scores[selection[i]] > scores[selection[j]]
		}
		return !p.visible[selection[i]] && p.visible[selection[j]]
	})
	for _, name := range selection {
		if in.Mode != "catalog" && matched[name] && selected < in.Limit {
			p.active[name] = p.request
			selected++
			if p.visible[name] {
				out.AlreadyLoaded = append(out.AlreadyLoaded, name)
			} else {
				out.Loaded = append(out.Loaded, name)
			}
		}
	}
	for _, name := range names[offset:end] {
		out.Candidates = append(out.Candidates, builtinCandidate{Name: name, Summary: compactToolText(p.schemas[name].Description, 60), Active: p.visible[name] || slicesContains(out.Loaded, name), Matched: matched[name]})
	}
	if len(out.Loaded) > 0 {
		out.Status = "loaded"
		out.Message = "已加载所选工具；下一轮读取完整声明并直接调用，其余候选不加载。"
	} else if len(out.AlreadyLoaded) > 0 {
		out.Status = "already_loaded"
		out.Message = "命中工具已声明，可直接复用。"
	}
	out.HasMore = end < len(names)
	if out.HasMore {
		out.Next = &builtinDiscoveryInput{Query: in.Query, Mode: in.Mode, Limit: in.Limit, Offset: end, CatalogVersion: version}
	}
	return out, nil
}

func slicesContains(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// Score explicit names strongly, then matching words/Chinese bigrams. Extra
// English/Chinese query words do not turn a useful partial match into zero hits.
// This ranks metadata only; execution still requires the complete trusted schema.
func builtinKeywordScore(query, name, description string) int {
	q := strings.ToLower(strings.TrimSpace(query))
	n := strings.ToLower(name)
	d := strings.ToLower(description)
	if q == n {
		return 10000
	}
	score := 0
	words := strings.FieldsFunc(q, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(",，。:：;；/()（）", r) })
	seen := map[string]bool{}
	for _, w := range words {
		if w == "" || seen[w] {
			continue
		}
		seen[w] = true
		if w == n {
			score += 10000
			continue
		}
		if strings.Contains(n, w) {
			score += 16
		}
		if strings.Contains(d, w) {
			score += 8
			continue
		}
		if len(w) > 4 && strings.HasSuffix(w, "s") && strings.Contains(strings.ReplaceAll(n, "_", " "), strings.TrimSuffix(w, "s")) {
			score += 8
		}
		r := []rune(w)
		for i := 0; i+1 < len(r); i++ {
			if unicode.Is(unicode.Han, r[i]) && unicode.Is(unicode.Han, r[i+1]) {
				pair := string(r[i : i+2])
				if !seen[pair] {
					seen[pair] = true
					if strings.Contains(d, pair) {
						score++
					}
				}
			}
		}
	}
	return score
}
