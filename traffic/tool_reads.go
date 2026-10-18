package traffic

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/llmrec"
)

func trafficReadTool(name, desc string, props map[string]any, required []string, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{Name: name, Description: desc, Schema: map[string]any{"type": "object", "properties": props, "required": required}, ReadOnly: func(json.RawMessage) bool { return true }, Concurrent: func(json.RawMessage) bool { return true }, Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
		return permission.Allowed()
	}, Run: func(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
		if tc != nil && tc.MaxOutputChars > 0 {
			ctx = context.WithValue(ctx, trafficBudgetKey{}, tc.MaxOutputChars)
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return run(ctx, in)
	}})
}

type trafficBudgetKey struct{}

func trafficBudget(ctx context.Context) int {
	if n, ok := ctx.Value(trafficBudgetKey{}).(int); ok && n > 0 {
		return n
	}
	return 30000
}
func trafficJSON(v any) (actool.Result, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return actool.Errorf(e.Error()), nil
	}
	return actool.Text(string(b)), nil
}
func decodeTrafficInput(raw json.RawMessage, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(out); e != nil {
		return e
	}
	var tail any
	if e := d.Decode(&tail); !errors.Is(e, io.EOF) {
		return fmt.Errorf("多余输入")
	}
	return nil
}

func (t *Traffic) allowToolHost(ctx context.Context, host string) error {
	ri := llmrec.RunInfoFrom(ctx)
	if ri.TaskID <= 0 {
		return nil
	}
	if t.assets == nil {
		return fmt.Errorf("任务资产校验不可用")
	}
	store := t.assets.WithReadContext(ctx)
	if (ri.AgentKey == "mainagent" && ri.IntentID == 0) || (ri.AgentKey == "worker" && ri.IntentID > 0) {
		return store.ValidateWorkerHosts(ri.TaskID, []string{host})
	}
	// Reporter and other explicitly bound roles retain strict approved-only
	// access. Tool binding decides availability; only trusted execution roles
	// above receive the existing pending-asset policy.
	return store.ValidateTaskHostsApproved(ri.TaskID, []string{host})
}

type trafficSearchInput struct {
	Host     string `json:"host"`
	Contains string `json:"contains"`
	Body     string `json:"body_contains"`
	Method   string `json:"method"`
	Status   int    `json:"status"`
	Since    int64  `json:"since"`
	Until    int64  `json:"until"`
	Limit    int    `json:"limit"`
	Cursor   string `json:"cursor"`
}
type trafficCursor struct {
	Signature string `json:"s"`
	Upper     int64  `json:"u"`
	TS        int64  `json:"t"`
	ID        string `json:"i"`
}

func (t *Traffic) searchTool() actool.CoreTool {
	str := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	integer := func(d string) map[string]any { return map[string]any{"type": "integer", "description": d} }
	return trafficReadTool("traffic_search", TrafficSearchDescription, map[string]any{"host": str("主机、主机:端口或URL"), "contains": str("URL字面子串"), "body_contains": str("正文关键词，至少3字符"), "method": str("HTTP方法"), "status": integer("HTTP状态码"), "since": integer("起始Unix时间"), "until": integer("结束Unix时间"), "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 10, "default": 3}, "cursor": str("原条件下的 next_cursor")}, []string{"host"}, func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
		var a trafficSearchInput
		if e := decodeTrafficInput(raw, &a); e != nil {
			return actool.Errorf(e.Error()), nil
		}
		host, port, e := normalizeSearchHost(a.Host)
		if e != nil {
			return actool.Errorf(e.Error()), nil
		}
		if e = t.allowToolHost(ctx, host); e != nil {
			return actool.Errorf(e.Error()), nil
		}
		if a.Limit == 0 {
			a.Limit = 3
		}
		if a.Limit < 1 || a.Limit > 10 || a.Since < 0 || a.Until < 0 || (a.Until > 0 && a.Since > a.Until) || (a.Status != 0 && (a.Status < 100 || a.Status > 599)) {
			return actool.Errorf("无效筛选或分页范围"), nil
		}
		a.Limit = min(a.Limit, max(1, (trafficBudget(ctx)-1024)/7000))
		normalized := a
		normalized.Cursor = ""
		normalized.Limit = 0
		b, _ := json.Marshal(normalized)
		signature := sha256.Sum256(b)
		c := trafficCursor{Signature: hex.EncodeToString(signature[:12])}
		if a.Cursor != "" {
			b, e := base64.RawURLEncoding.DecodeString(a.Cursor)
			var old trafficCursor
			if e != nil || json.Unmarshal(b, &old) != nil || old.Signature != c.Signature || old.ID == "" || old.Upper < 0 {
				return actool.Errorf("游标无效或筛选条件已变化"), nil
			}
			c = old
		} else {
			if e = t.db.QueryRowContext(ctx, `SELECT COALESCE(max(rowid),0) FROM exchanges WHERE host=?`, host).Scan(&c.Upper); e != nil {
				return actool.Errorf(e.Error()), nil
			}
		}
		where := `host=? AND rowid<=?`
		args := []any{host, c.Upper}
		if port != "" {
			authority := net.JoinHostPort(host, port)
			where += ` AND (url LIKE ? OR url LIKE ? OR url LIKE ?)`
			args = append(args, "%://"+authority+"/%", "%://"+authority+"?%", "%://"+authority)
		}
		if a.Contains != "" {
			where += ` AND (instr(lower(url),lower(?))>0 OR instr(lower(url_template),lower(?))>0)`
			args = append(args, a.Contains, a.Contains)
		}
		if a.Body != "" {
			cond, arg, ok := t.ftsFilter(a.Body)
			if !ok {
				return actool.Errorf("正文搜索需要全文索引且至少3个字符"), nil
			}
			where += " AND " + cond
			args = append(args, arg)
		}
		if a.Method != "" {
			where += ` AND upper(method)=upper(?)`
			args = append(args, a.Method)
		}
		if a.Status != 0 {
			where += ` AND status=?`
			args = append(args, a.Status)
		}
		if a.Since > 0 {
			where += ` AND ts>=?`
			args = append(args, a.Since)
		}
		if a.Until > 0 {
			where += ` AND ts<=?`
			args = append(args, a.Until)
		}
		if c.ID != "" {
			where += ` AND (ts<? OR (ts=? AND id<?))`
			args = append(args, c.TS, c.TS, c.ID)
		}
		args = append(args, a.Limit+1)
		rows, e := t.db.QueryContext(ctx, `SELECT id,ts,method,substr(url,1,1024),status,resp_len,length(url)>1024 FROM exchanges WHERE `+where+` ORDER BY ts DESC,id DESC LIMIT ?`, args...)
		if e != nil {
			return actool.Errorf(e.Error()), nil
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var id, method, url string
			var ts int64
			var status, length int
			var truncated bool
			if e = rows.Scan(&id, &ts, &method, &url, &status, &length, &truncated); e != nil {
				return actool.Errorf(e.Error()), nil
			}
			items = append(items, map[string]any{"id": id, "ts": ts, "method": method, "url": url, "status": status, "resp_len": length, "url_truncated": truncated})
		}
		if e = rows.Err(); e != nil {
			return actool.Errorf(e.Error()), nil
		}
		more := len(items) > a.Limit
		if more {
			items = items[:a.Limit]
		}
		out := map[string]any{"target_host": host, "items": items, "has_more": more, "body_index_max_bytes": maxIndexBody, "binary_body_indexed": false}
		if more {
			last := items[len(items)-1]
			c.TS = last["ts"].(int64)
			c.ID = last["id"].(string)
			b, _ := json.Marshal(c)
			out["next_cursor"] = base64.RawURLEncoding.EncodeToString(b)
		}
		return boundedTrafficJSON(ctx, out)
	})
}

var trafficParts = []string{"request_headers", "request_body", "response_headers", "response_body"}

func (t *Traffic) getTool() actool.CoreTool {
	return trafficReadTool("traffic_get", "读取流量的请求/响应头与正文。默认有界预览；用 part、next_offset 续读，Blob 由服务端解析。offset/max_bytes 按字节计。", map[string]any{"id": map[string]any{"type": "string"}, "part": map[string]any{"type": "string", "enum": trafficParts}, "offset": map[string]any{"type": "integer", "minimum": 0}, "max_bytes": map[string]any{"type": "integer", "minimum": 4, "maximum": 8192, "default": 8192}}, []string{"id"}, func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
		var a struct {
			ID     string `json:"id"`
			Part   string `json:"part"`
			Offset int64  `json:"offset"`
			Max    int64  `json:"max_bytes"`
		}
		if e := decodeTrafficInput(raw, &a); e != nil {
			return actool.Errorf(e.Error()), nil
		}
		if a.Max == 0 {
			a.Max = 8192
		}
		if a.ID == "" || a.Offset < 0 || a.Max < 4 || a.Max > 8192 || (a.Part != "" && !slices.Contains(trafficParts, a.Part)) || (a.Part == "" && a.Offset != 0) {
			return actool.Errorf("无效流量ID、部分或续读范围"), nil
		}
		var host string
		if e := t.db.QueryRowContext(ctx, `SELECT host FROM exchanges WHERE id=?`, a.ID).Scan(&host); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return actool.Errorf("流量不可用"), nil
			}
			return actool.Errorf("流量查询失败: " + e.Error()), nil
		}
		if e := t.allowToolHost(ctx, host); e != nil {
			return actool.Errorf(e.Error()), nil
		}
		parts := trafficParts
		if a.Part != "" {
			parts = []string{a.Part}
		}
		// JSON escaping is bounded by six bytes per input byte. Reserve metadata
		// before reading; continuation addresses cannot be lost to Capture.
		perPart := min(a.Max/int64(len(parts)), int64((trafficBudget(ctx)-512*len(parts))/6/len(parts)))
		if perPart < 4 {
			return actool.Errorf("输出预算过小；请指定单个 part，max_bytes 至少4，或提高工具输出上限"), nil
		}
		out := []map[string]any{}
		for _, part := range parts {
			data, total, e := t.readTrafficPart(ctx, a.ID, part, a.Offset, perPart)
			if e != nil {
				return actool.Errorf(e.Error()), nil
			}
			row := map[string]any{"part": part, "offset": a.Offset, "total_bytes": total, "truncated": a.Offset+int64(len(data)) < total}
			if isBinaryBody("", data) || !utf8.Valid(data) {
				row["encoding"] = "hex"
				row["text"] = hex.EncodeToString(data)
			} else {
				row["text"] = string(data)
			}
			if a.Offset+int64(len(data)) < total {
				row["next_offset"] = a.Offset + int64(len(data))
				row["next"] = map[string]any{"id": a.ID, "part": part, "offset": a.Offset + int64(len(data)), "max_bytes": a.Max}
			}
			out = append(out, row)
		}
		return boundedTrafficJSON(ctx, map[string]any{"id": a.ID, "target_host": host, "parts": out})
	})
}
func (t *Traffic) readTrafficPart(ctx context.Context, id, part string, offset, limit int64) ([]byte, int64, error) {
	side := "req"
	if strings.HasPrefix(part, "response") {
		side = "resp"
	}
	var head string
	var body []byte
	var blob sql.NullString
	var e error
	if strings.HasSuffix(part, "headers") {
		e = t.db.QueryRowContext(ctx, `SELECT `+side+`_head FROM exchange_bodies WHERE id=?`, id).Scan(&head)
	} else {
		e = t.db.QueryRowContext(ctx, `SELECT `+side+`_body,`+side+`_blob FROM exchange_bodies WHERE id=?`, id).Scan(&body, &blob)
	}
	if errors.Is(e, sql.ErrNoRows) {
		return t.readLegacyTrafficPart(ctx, id, part, offset, limit)
	} else if e == nil {
		body = decompressInline(body)
	}
	if e != nil {
		return nil, 0, e
	}
	var data []byte
	var total int64
	if strings.HasSuffix(part, "headers") {
		data = []byte(head)
	} else if blob.Valid && blob.String != "" {
		data, total, e = t.BlobRange(blob.String, offset, limit+4)
		if e != nil {
			return nil, 0, e
		}
		return utf8ByteWindow(data, limit), total, ctx.Err()
	} else {
		data = body
	}
	total = int64(len(data))
	if offset >= total {
		return []byte{}, total, nil
	}
	return utf8ByteWindow(data[offset:], limit), total, ctx.Err()
}

var legacyBodyPointer = regexp.MustCompile(`(?:^|\n)(?:…\[truncated\] )?@blob sha256:([0-9a-f]{64}) \(len=[0-9]+\)\s*$`)

// Legacy captures retain their on-disk HTTP envelopes. Read only the selected
// side and requested body window; missing files are errors, never empty success.
func (t *Traffic) readLegacyTrafficPart(ctx context.Context, id, part string, offset, limit int64) ([]byte, int64, error) {
	var rel string
	if err := t.db.QueryRowContext(ctx, `SELECT path FROM exchanges WHERE id=?`, id).Scan(&rel); err != nil {
		return nil, 0, err
	}
	if !filepath.IsLocal(rel) {
		return nil, 0, fmt.Errorf("流量原始文件路径不可用")
	}
	name := "request.http"
	if strings.HasPrefix(part, "response") {
		name = "response.http"
	}
	f, err := os.Open(filepath.Join(t.dir, rel, name))
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	const maxHead = 1 << 20
	r := bufio.NewReader(io.LimitReader(f, maxHead+1))
	var head strings.Builder
	var bodyStart int64
	for {
		line, readErr := r.ReadString('\n')
		bodyStart += int64(len(line))
		if bodyStart > maxHead {
			return nil, 0, fmt.Errorf("HTTP 头超过读取上限")
		}
		if line == "\n" || line == "\r\n" {
			break
		}
		head.WriteString(line)
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, 0, readErr
		}
	}
	if strings.HasSuffix(part, "headers") {
		data := []byte(head.String())
		if offset >= int64(len(data)) {
			return []byte{}, int64(len(data)), ctx.Err()
		}
		return utf8ByteWindow(data[offset:], limit), int64(len(data)), ctx.Err()
	}
	total := stat.Size() - bodyStart
	tail := make([]byte, min(total, 256))
	if _, err = f.ReadAt(tail, stat.Size()-int64(len(tail))); err != nil && err != io.EOF {
		return nil, 0, err
	}
	if match := legacyBodyPointer.FindSubmatch(tail); match != nil {
		data, size, err := t.BlobRange(string(match[1]), offset, limit+4)
		if err != nil {
			return nil, 0, err
		}
		return utf8ByteWindow(data, limit), size, ctx.Err()
	}
	if offset >= total {
		return []byte{}, total, ctx.Err()
	}
	data := make([]byte, min(total-offset, limit+4))
	n, err := f.ReadAt(data, bodyStart+offset)
	if err != nil && err != io.EOF {
		return nil, 0, err
	}
	return utf8ByteWindow(data[:n], limit), total, ctx.Err()
}
func utf8ByteWindow(data []byte, limit int64) []byte {
	end := min(int64(len(data)), limit)
	if end < int64(len(data)) && !isBinaryBody("", data) {
		candidate := end
		for candidate > 0 && !utf8.RuneStart(data[candidate]) {
			candidate--
		}
		if candidate > 0 && utf8.Valid(data[:candidate]) {
			end = candidate
		}
	}

	return data[:end]
}

// SeedToolMetas constructs metadata without opening a traffic store.
func SeedToolMetas() []actool.CoreTool { return (&Traffic{}).Tools() }

func boundedTrafficJSON(ctx context.Context, v any) (actool.Result, error) {
	r, e := trafficJSON(v)
	if e != nil || r.IsError {
		return r, e
	}
	if len(r.Flatten()) > trafficBudget(ctx) {
		return actool.Errorf("结果元数据超过输出上限；请减少 limit 或指定单个 part"), nil
	}
	return r, nil
}
