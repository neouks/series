package agent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	actool "github.com/Autumn-27/norma/tool"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var memorySearchCache = struct {
	sync.Mutex
	values map[string]string
	order  []string
}{values: map[string]string{}}

type memorySearchCursor struct {
	Signature string `json:"s"`
	File      int    `json:"f"`
	Offset    int64  `json:"o"`
}

// Search body chunks with a byte/work budget instead of SDK Relevant's full
// body scan. No model/index service is used; callers explicitly continue work.
func (t *catalogRecallTool) search(ctx context.Context, query, cursor string) (actool.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if utf8.RuneCountInString(query) > 200 {
		return actool.Errorf("query最多200字符"), nil
	}
	root, err := filepath.Abs(t.store.Dir)
	if err != nil {
		return actool.Errorf("记忆目录不可用"), nil
	}
	dir, err := os.Open(root)
	if os.IsNotExist(err) {
		return contextJSON(ctx, map[string]any{"items": []any{}, "has_more": false, "scope": "memory"})
	}
	if err != nil {
		return actool.Errorf("记忆目录不可读"), nil
	}
	entries, err := dir.ReadDir(memoryCatalogFiles + 1)
	dir.Close()
	if err != nil && err != io.EOF {
		return actool.Errorf("记忆目录读取失败"), nil
	}
	limited := len(entries) > memoryCatalogFiles
	if limited {
		entries = entries[:memoryCatalogFiles]
	}
	names := []string{}
	for _, e := range entries {
		if e.Name() != "MEMORY.md" && strings.HasSuffix(e.Name(), ".md") && e.Type()&os.ModeSymlink == 0 && !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	versions := map[string]string{}
	h := sha256.New()
	h.Write([]byte(root))
	h.Write([]byte{0})
	h.Write([]byte(query))
	for _, entry := range entries {
		name := entry.Name()
		if err = ctx.Err(); err != nil {
			return actool.Result{}, err
		}
		if !strings.HasSuffix(name, ".md") || name == "MEMORY.md" || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			continue
		}
		info, e := entry.Info()
		if e != nil || !info.Mode().IsRegular() {
			versions[name] = "unavailable"
		} else {
			versions[name] = memoryFileVersion(info)
		}
	}
	for _, name := range names {
		fmt.Fprintf(h, "%d:%s:%s;", len(name), name, versions[name])
	}

	signature := hex.EncodeToString(h.Sum(nil))
	if cursor == "" {
		memorySearchCache.Lock()
		cached := memorySearchCache.values[signature]
		memorySearchCache.Unlock()
		if cached != "" && len(cached) <= readOutputBudget(ctx) {
			var result map[string]any
			if json.Unmarshal([]byte(cached), &result) == nil {
				result["cache_hit"] = true
				result["scanned_bytes"] = 0
				return contextJSON(ctx, result)
			}
		}
	}
	position := memorySearchCursor{Signature: signature}
	if cursor != "" {
		raw, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil || json.Unmarshal(raw, &position) != nil || position.Signature != signature || position.File < 0 || position.File >= len(names) || position.Offset < 0 {
			return actool.Errorf("经验查询游标或源版本已变化，请重新查询"), nil
		}
	}
	items := []any{}
	scanned := 0
	files := 0
	unavailable := 0
	for position.File < len(names) && scanned < 256<<10 && files < 16 && len(items) < 5 {
		if err = ctx.Err(); err != nil {
			return actool.Result{}, err
		}
		name := names[position.File]
		meta, _, e := memoryMetadata(root, name)
		if e != nil {
			unavailable++
			position.File++
			position.Offset = 0
			continue
		}
		f, info, e := openMemoryFile(root, name)
		if e != nil {
			unavailable++
			position.File++
			position.Offset = 0
			continue
		}
		if memoryFileVersion(info) != versions[name] || position.Offset > info.Size()-meta.body {
			f.Close()
			return actool.Errorf("经验源在搜索期间变化，请重试"), nil
		}
		if _, e = f.Seek(meta.body+position.Offset, io.SeekStart); e != nil {
			f.Close()
			return actool.Errorf("记忆定位失败"), nil
		}
		// 16K runes <=64KiB. Overlap ensures a <=200-rune keyword spanning
		// chunks is found, including Chinese; offsets remain original UTF-8 bytes.
		runes := min(16000, ((256<<10)-scanned)/4)
		if runes <= 200 {
			f.Close()
			break
		}
		text, consumed, more, e := readMemoryRunes(ctx, f, runes)
		after, statErr := f.Stat()
		f.Close()
		files++
		scanned += int(consumed)
		if e != nil {
			return actool.Result{}, e
		}
		if statErr != nil || memoryFileVersion(after) != versions[name] {
			return actool.Errorf("经验源在搜索期间变化，请重试"), nil
		}
		lower := strings.ToLower(text)
		at := strings.Index(lower, strings.ToLower(query))
		if at >= 0 {
			at = len(string([]rune(text)[:utf8.RuneCountInString(lower[:at])]))
		}
		metadataMatch := position.Offset == 0 && strings.Contains(strings.ToLower(meta.name+" "+meta.description), strings.ToLower(query))
		if at >= 0 || metadataMatch {
			start := 0
			if at >= 0 {
				start = at
				for i := 0; i < 60 && start > 0; i++ {
					_, n := utf8.DecodeLastRuneInString(text[:start])
					start -= n
				}
			}
			preview := string([]rune(text[start:])[:min(240, len([]rune(text[start:])))])
			items = append(items, map[string]any{"name": meta.name, "file": name, "snippet": preview, "read": map[string]any{"name": name, "version": meta.version, "offset": position.Offset + int64(start), "max_chars": 4000}})
		}
		if at >= 0 || metadataMatch {
			position.File++
			position.Offset = 0
			continue
		}
		if !more {
			position.File++
			position.Offset = 0
		} else {
			overlap := []rune(text)
			tail := string(overlap[max(0, len(overlap)-200):])
			position.Offset += consumed - int64(len(tail))
		}
	}
	out := map[string]any{"items": items, "has_more": position.File < len(names), "scan_incomplete": position.File < len(names) || limited || unavailable > 0, "directory_limited": limited, "unavailable_files": unavailable, "scanned_bytes": scanned, "note": "历史经验不是当前目标证据；空页且scan_incomplete=true不表示无匹配。目录最多4096项，超限需按已知name读取。"}
	if position.File < len(names) {
		raw, _ := json.Marshal(position)
		out["next"] = map[string]any{"query": query, "cursor": base64.RawURLEncoding.EncodeToString(raw)}
	}
	result, err := contextJSON(ctx, out)
	if cursor == "" && err == nil && !result.IsError && out["has_more"] == false && !limited && unavailable == 0 {
		raw, _ := json.Marshal(out)
		if len(raw) <= 16<<10 {
			memorySearchCache.Lock()
			if _, ok := memorySearchCache.values[signature]; !ok {
				if len(memorySearchCache.order) >= 64 {
					delete(memorySearchCache.values, memorySearchCache.order[0])
					memorySearchCache.order = memorySearchCache.order[1:]
				}
				memorySearchCache.order = append(memorySearchCache.order, signature)
			}
			memorySearchCache.values[signature] = string(raw)
			memorySearchCache.Unlock()
		}
	}
	return result, err
}
