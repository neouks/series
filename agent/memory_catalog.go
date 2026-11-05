package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Autumn-27/norma/memory"
)

// This catalog stores only file identity and bounded frontmatter, not memory
// bodies. It is disposable, bounded across stores, and rebuilt incrementally
// after restart. Explicit RecallMemory queries keep their original semantics.
const memoryCatalogFiles = 4096
const memoryCatalogRefresh = 64
const memoryHeaderBytes = 8192

type memoryCatalogEntry struct {
	info              os.FileInfo
	version           string
	name, description string
	kind              memory.Type
	body              int64
	unavailable       bool
	limited           bool
}

type memoryPreviewCatalog struct {
	gate    chan struct{}
	entries map[string]memoryCatalogEntry
}

var memoryCatalogs = struct {
	sync.Mutex
	items map[string]*memoryPreviewCatalog
	order []string
}{items: make(map[string]*memoryPreviewCatalog)}

func memoryCatalogFor(root string) *memoryPreviewCatalog {
	memoryCatalogs.Lock()
	defer memoryCatalogs.Unlock()
	if c := memoryCatalogs.items[root]; c != nil {
		for i, name := range memoryCatalogs.order {
			if name == root {
				memoryCatalogs.order = append(memoryCatalogs.order[:i], memoryCatalogs.order[i+1:]...)
				break
			}
		}
		memoryCatalogs.order = append(memoryCatalogs.order, root)
		return c
	}
	if len(memoryCatalogs.order) >= 16 {
		delete(memoryCatalogs.items, memoryCatalogs.order[0])
		memoryCatalogs.order = memoryCatalogs.order[1:]
	}
	c := &memoryPreviewCatalog{gate: make(chan struct{}, 1), entries: make(map[string]memoryCatalogEntry)}
	memoryCatalogs.items[root] = c
	memoryCatalogs.order = append(memoryCatalogs.order, root)
	return c
}

// Include the OS change timestamp when available, so replacing content and
// restoring mtime/size does not serve stale metadata. No platform-specific SDK
// dependency or raw text is kept in the version.
func memoryFileVersion(info os.FileInfo) string {
	change := ""
	v := reflect.ValueOf(info.Sys())
	if v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	if v.IsValid() && v.Kind() == reflect.Struct {
		for _, name := range []string{"Ctim", "Ctimespec", "ChangeTime"} {
			if f := v.FieldByName(name); f.IsValid() && f.CanInterface() {
				change = fmt.Sprint(f.Interface())
				break
			}
		}
	}
	return fmt.Sprintf("%d:%d:%s", info.Size(), info.ModTime().UnixNano(), change)
}

func openMemoryFile(root, name string) (*os.File, os.FileInfo, error) {
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) || !strings.HasSuffix(name, ".md") || name == "MEMORY.md" {
		return nil, nil, fmt.Errorf("记忆文件名无效")
	}
	path := filepath.Join(root, name)
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("记忆不是普通文件；不读取链接或特殊文件")
	}
	// Reuse the project regular-file opener: Unix nonblocking open prevents a
	// FIFO replacement between Lstat/Open from hanging before descriptor checks.
	f, err := openGrepRegular(path)
	if err != nil {
		return nil, nil, err
	}
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) || memoryFileVersion(before) != memoryFileVersion(after) {
		f.Close()
		return nil, nil, fmt.Errorf("记忆在读取时变化，请重试")
	}
	return f, after, nil
}

func memoryMetadata(root, name string) (memoryCatalogEntry, int, error) {
	f, info, err := openMemoryFile(root, name)
	if err != nil {
		return memoryCatalogEntry{}, 0, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, memoryHeaderBytes+1))
	if err != nil {
		return memoryCatalogEntry{}, len(raw), err
	}
	entry := memoryCatalogEntry{info: info, version: memoryFileVersion(info), name: strings.TrimSuffix(name, ".md")}
	text := string(raw)
	if strings.HasPrefix(text, "---\n") || strings.HasPrefix(text, "---\r\n") {
		reader := bufio.NewReader(strings.NewReader(text))
		var offset int64
		for lineNo := 0; ; lineNo++ {
			line, err := reader.ReadString('\n')
			offset += int64(len(line))
			if offset > memoryHeaderBytes {
				return memoryCatalogEntry{}, len(raw), fmt.Errorf("记忆头部超过8KiB")
			}
			if lineNo > 0 && strings.TrimSpace(line) == "---" {
				entry.body = offset
				break
			}
			if lineNo > 0 {
				key, value, _ := strings.Cut(line, ":")
				value = strings.TrimSpace(value)
				switch strings.TrimSpace(key) {
				case "name":
					if value != "" {
						entry.limited = entry.limited || utf8.RuneCountInString(value) > 256
						entry.name = firstLine(value, 256)
					}
				case "description":
					entry.limited = entry.limited || utf8.RuneCountInString(value) > 512
					entry.description = firstLine(value, 512)
				case "type":
					entry.kind = memory.Type(firstLine(value, 32))
				}
			}
			if err != nil || offset > memoryHeaderBytes {
				return memoryCatalogEntry{}, len(raw), fmt.Errorf("记忆头部缺失结束标记或超过8KiB")
			}
		}
	}
	return entry, len(raw), nil
}

type memoryPreviewSelection struct {
	items                        []memory.Memory
	files                        []string
	versions                     []string
	indexed, pending, unreadable int
	more                         bool
	headerBytes                  int
}

func memoryQueryTerms(query string) []string {
	var terms []string
	seen := make(map[string]bool)
	add := func(s string) {
		if len(terms) < 64 && utf8.RuneCountInString(s) >= 2 && !seen[s] {
			terms = append(terms, s)
			seen[s] = true
		}
	}
	for _, word := range strings.FieldsFunc(strings.ToLower(firstLine(query, 2000)), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' }) {
		runes := []rune(word)
		add(word)
		// Chinese text has no word spaces. Literal Han bigrams recover relevant
		// metadata without pretending to provide semantic or synonym search.
		for i := 0; i+1 < len(runes) && len(terms) < 64; i++ {
			if unicode.Is(unicode.Han, runes[i]) && unicode.Is(unicode.Han, runes[i+1]) {
				add(string(runes[i : i+2]))
			}
		}
	}
	return terms
}

func selectMemoryPreviews(ctx context.Context, store *memory.Store, query string) (out memoryPreviewSelection, err error) {
	root, err := filepath.Abs(store.Dir)
	if err != nil {
		return out, err
	}
	c := memoryCatalogFor(root)
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		return out, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	dir, err := os.Open(root)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	rows, readErr := dir.ReadDir(memoryCatalogFiles + 1)
	dir.Close()
	if readErr != nil && readErr != io.EOF {
		return out, readErr
	}
	if len(rows) > memoryCatalogFiles {
		out.more = true
		rows = rows[:memoryCatalogFiles]
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name() < rows[j].Name() })
	fresh := make(map[string]memoryCatalogEntry, min(len(rows), memoryCatalogFiles))
	refreshed := 0
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if row.Name() == "MEMORY.md" || !strings.HasSuffix(row.Name(), ".md") || row.Type()&os.ModeSymlink != 0 || row.IsDir() {
			continue
		}
		info, err := row.Info()
		if err != nil || !info.Mode().IsRegular() {
			out.unreadable++
			continue
		}
		entry, exists := c.entries[row.Name()]
		if !exists || entry.version != memoryFileVersion(info) || !os.SameFile(entry.info, info) {
			if refreshed >= memoryCatalogRefresh {
				out.pending++
				continue
			}
			refreshed++
			var n int
			entry, n, err = memoryMetadata(root, row.Name())
			out.headerBytes += n
			if err != nil {
				entry = memoryCatalogEntry{info: info, version: memoryFileVersion(info), unavailable: true}
			}
		}
		fresh[row.Name()] = entry
		out.more = out.more || entry.limited
		if entry.unavailable {
			out.unreadable++
		} else {
			out.indexed++
		}
	}
	c.entries = fresh // Deleted, replaced and unreadable files cannot survive in the view.
	type candidate struct {
		file  string
		entry memoryCatalogEntry
		score int
	}
	var candidates []candidate
	terms := memoryQueryTerms(query)
	for name, entry := range fresh {
		if entry.unavailable {
			continue
		}
		score := 0
		nameText := strings.ToLower(entry.name + " " + name)
		descriptionText := strings.ToLower(entry.description)
		for _, term := range terms {
			if strings.Contains(nameText, term) {
				score += 3
			}
			if strings.Contains(descriptionText, term) {
				score += 2
			}
		}
		if score > 0 {
			candidates = append(candidates, candidate{name, entry, score})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		if !candidates[i].entry.info.ModTime().Equal(candidates[j].entry.info.ModTime()) {
			return candidates[i].entry.info.ModTime().After(candidates[j].entry.info.ModTime())
		}
		return candidates[i].file < candidates[j].file
	})
	if len(candidates) > 3 {
		candidates = candidates[:3]
		out.more = true
	}
	for _, row := range candidates {
		f, info, err := openMemoryFile(root, row.file)
		if err != nil {
			out.unreadable++
			continue
		}
		if memoryFileVersion(info) != row.entry.version || !os.SameFile(info, row.entry.info) {
			f.Close()
			out.unreadable++
			continue
		}
		_, err = f.Seek(row.entry.body, io.SeekStart)
		if err != nil {
			f.Close()
			out.unreadable++
			continue
		}
		preview, _, _, err := readMemoryRunes(ctx, f, 500)
		after, statErr := f.Stat()
		f.Close()
		if err != nil || statErr != nil || memoryFileVersion(after) != row.entry.version {
			out.unreadable++
			continue
		}
		out.items = append(out.items, memory.Memory{Name: row.entry.name, Description: row.entry.description, Type: row.entry.kind, Content: preview, ModTime: info.ModTime()})
		out.files = append(out.files, row.file)
		out.versions = append(out.versions, row.entry.version)
	}
	out.more = out.more || out.pending > 0 || out.unreadable > 0
	return out, nil
}

// Offset is bytes at a UTF-8 boundary. Returning consumed bytes makes explicit
// continuation seekable without rescanning an arbitrarily long memory prefix.
func readMemoryRunes(ctx context.Context, r io.Reader, limit int) (string, int64, bool, error) {
	reader := bufio.NewReaderSize(r, 4096)
	var b strings.Builder
	var consumed int64
	for i := 0; i < limit; i++ {
		if i%128 == 0 {
			if err := ctx.Err(); err != nil {
				return "", 0, false, err
			}
		}
		runeValue, n, err := reader.ReadRune()
		if err == io.EOF {
			return b.String(), consumed, false, nil
		}
		if err != nil {
			return "", 0, false, err
		}
		if runeValue == utf8.RuneError && n == 1 {
			return "", 0, false, fmt.Errorf("记忆正文不是有效UTF-8，或offset不是字符边界")
		}
		b.WriteRune(runeValue)
		consumed += int64(n)
	}
	_, err := reader.Peek(1)
	return b.String(), consumed, err != io.EOF, errUnlessEOF(err)
}

func errUnlessEOF(err error) error {
	if err == io.EOF {
		return nil
	}
	return err
}

func memoryPreviewContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, time.Second)
}
