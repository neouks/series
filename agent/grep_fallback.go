package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const grepFileBytes = 8 << 20
const grepScanBytes = 64 << 20
const grepPathBytes = 4 << 20
const grepEntries = 20000
const grepMultiMatches = 65536

// These are the SDK's documented common file-type aliases. Native rg still
// decides its type definitions; unknown names retain extension-glob behavior.
var grepTypes = map[string][]string{
	"go": {"*.go"}, "py": {"*.py", "*.pyi"}, "python": {"*.py", "*.pyi"},
	"js": {"*.js", "*.jsx", "*.mjs", "*.cjs", "*.vue"}, "ts": {"*.ts", "*.tsx", "*.mts", "*.cts"},
	"rust": {"*.rs"}, "java": {"*.java"}, "kotlin": {"*.kt", "*.kts"}, "c": {"*.c", "*.h"},
	"cpp": {"*.cpp", "*.cc", "*.cxx", "*.hpp", "*.hh", "*.hxx", "*.h"}, "cs": {"*.cs"},
	"rb": {"*.rb"}, "ruby": {"*.rb"}, "php": {"*.php"}, "swift": {"*.swift"}, "sh": {"*.sh", "*.bash", "*.zsh"},
	"html": {"*.html", "*.htm"}, "css": {"*.css", "*.scss", "*.sass", "*.less"}, "json": {"*.json"},
	"yaml": {"*.yaml", "*.yml"}, "toml": {"*.toml"}, "xml": {"*.xml"}, "md": {"*.md", "*.markdown"},
	"markdown": {"*.md", "*.markdown"}, "sql": {"*.sql"}, "proto": {"*.proto"},
}

func grepGo(ctx context.Context, root string, in grepInput, re *regexp.Regexp, page *grepPage) (string, error) {
	stat, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	paths := []string{root}
	base := filepath.Dir(root)
	omitted := false
	if stat.IsDir() {
		base = root
		paths, omitted, err = grepFiles(ctx, root, in)
		if err != nil {
			return "", err
		}
	}
	remaining := int64(grepScanBytes)
	for _, path := range paths {
		if err = ctx.Err(); err != nil {
			return "", err
		}
		if remaining <= 1 {
			omitted = true
			break
		}
		data, partial, readErr := grepReadFile(ctx, path, min(int64(grepFileBytes), remaining-1))
		remaining -= int64(len(data))
		if partial {
			remaining--
		} // count the EOF/overflow probe in the query byte budget
		if readErr != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			omitted = true
			continue
		}
		if bytes.IndexByte(data[:min(8000, len(data))], 0) >= 0 {
			continue
		}
		if partial {
			omitted = true
			// A partial buffer must not create a false EOF/$ match. Multiline
			// queries require a complete file; line mode keeps complete lines only.
			if in.Multiline {
				continue
			}
			if n := bytes.LastIndexByte(data, '\n'); n >= 0 {
				data = data[:n+1]
			} else {
				continue
			}
		}
		rel, e := filepath.Rel(base, path)
		if e != nil {
			rel = path
		}
		cut, e := grepScanFile(ctx, data, rel, in, re, page)
		if e != nil {
			return "", e
		}
		omitted = omitted || cut
	}
	if omitted {
		return "Go回退搜索未覆盖全部内容：目录/路径预算、不可读/非普通文件、8 MiB单文件或64 MiB总扫描预算、多行匹配预算可能省略内容；请缩小path/glob/type或读取原文件，不能据此判断不存在", nil
	}
	return "", nil
}

// Read directory entries in bounded batches. filepath.WalkDir would load an
// entire very wide directory before our callback could stop it.
func grepFiles(ctx context.Context, root string, in grepInput) ([]string, bool, error) {
	match, err := grepGlobMatcher(in.Glob)
	if err != nil {
		return nil, false, err
	}
	types := grepTypes[strings.ToLower(in.Type)]
	if in.Type != "" && len(types) == 0 {
		types = []string{"*." + in.Type}
	}
	stack := []string{root}
	var paths []string
	entries, pathBytes := 0, len(root)
	omitted := false
	for len(stack) > 0 {
		if err = ctx.Err(); err != nil {
			return nil, false, err
		}
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if grepSkipDir(filepath.Base(dir)) {
			continue
		}
		f, e := os.Open(dir)
		if e != nil {
			omitted = true
			continue
		}
		stop := false
		for {
			if err = ctx.Err(); err != nil {
				f.Close()
				return nil, false, err
			}
			batch, readErr := f.ReadDir(256)
			for _, entry := range batch {
				entries++
				path := filepath.Join(dir, entry.Name())
				pathBytes += len(path)
				if entries > grepEntries || pathBytes > grepPathBytes {
					omitted, stop = true, true
					break
				}
				if entry.IsDir() {
					if !grepSkipDir(entry.Name()) {
						stack = append(stack, path)
					}
					continue
				}
				rel, e := filepath.Rel(root, path)
				if e != nil {
					rel = path
				}
				if !match(filepath.ToSlash(rel)) {
					continue
				}
				if len(types) > 0 {
					ok := false
					for _, glob := range types {
						yes, _ := filepath.Match(glob, entry.Name())
						ok = ok || yes
					}
					if !ok {
						continue
					}
				}
				paths = append(paths, path)
			}
			if stop || readErr == io.EOF {
				break
			}
			if readErr != nil {
				omitted = true
				break
			}
		}
		f.Close()
		if stop {
			break
		}
	}
	sort.Strings(paths)
	return paths, omitted, nil
}
func grepSkipDir(name string) bool {
	return name == ".git" || name == "node_modules" || name == "vendor"
}

func grepReadFile(ctx context.Context, path string, limit int64) ([]byte, bool, error) {
	f, err := openGrepRegular(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !stat.Mode().IsRegular() {
		return nil, false, fmt.Errorf("not a regular file")
	}
	data := make([]byte, 0, int(min(max(0, stat.Size()), limit+1)))
	buf := make([]byte, 32*1024)
	for int64(len(data)) <= limit {
		if err = ctx.Err(); err != nil {
			return nil, false, err
		}
		n, e := f.Read(buf[:min(int64(len(buf)), limit+1-int64(len(data)))])
		data = append(data, buf[:n]...)
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, false, e
		}
	}
	partial := int64(len(data)) > limit
	if partial {
		data = data[:limit]
	}
	return data, partial, nil
}

// Store one bit per source line, not string headers/maps for every line. Match
// and context passes are linear; multiline no longer does matches × all lines.
func grepScanFile(ctx context.Context, data []byte, path string, in grepInput, re *regexp.Regexp, page *grepPage) (bool, error) {
	if len(data) == 0 {
		return false, nil
	}
	if !in.Multiline && in.Mode != "content" {
		count := 0
		stop := errors.New("file matched")
		err := grepEachLine(ctx, data, func(_ int, start, end int) error {
			if re.Match(data[start:end]) {
				count++
				if in.Mode != "count" {
					return stop
				}
			}
			return nil
		})
		if err != nil && !errors.Is(err, stop) {
			return false, err
		}
		if count > 0 {
			prefix := path
			if in.Mode == "count" {
				prefix = fmt.Sprintf("%s: %d", path, count)
			}
			page.add(prefix, nil, false)
		}
		return false, nil
	}
	lines := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		lines++
	}
	hits := make([]uint64, (lines+63)/64)
	set := func(i int) { hits[i/64] |= uint64(1) << uint(i%64) }
	hit := func(i int) int {
		if i < 0 || i >= lines {
			return 0
		}
		if hits[i/64]&(uint64(1)<<uint(i%64)) != 0 {
			return 1
		}
		return 0
	}
	var spans [][]int
	cut := false
	if in.Multiline {
		spans = re.FindAllIndex(data, grepMultiMatches+1)
		if len(spans) > grepMultiMatches {
			spans = spans[:grepMultiMatches]
			cut = true
		}
	}
	count, span := 0, 0
	err := grepEachLine(ctx, data, func(i, start, end int) error {
		matches := false
		if in.Multiline {
			for span < len(spans) && spans[span][1] <= start {
				span++
			}
			matches = span < len(spans) && spans[span][0] <= end && spans[span][1] > start
		} else {
			matches = re.Match(data[start:end])
		}
		if matches {
			set(i)
			count++
		}
		return nil
	})
	if err != nil {
		return cut, err
	}
	if count == 0 {
		return cut, nil
	}
	if in.Mode != "content" {
		prefix := path
		if in.Mode == "count" {
			prefix = fmt.Sprintf("%s: %d", path, count)
		}
		page.add(prefix, nil, false)
		return cut, nil
	}
	before, after := min(in.Before, lines), min(in.After, lines)
	window := 0
	for i := 0; i <= before && i < lines; i++ {
		window += hit(i)
	}
	previous := -2
	err = grepEachLine(ctx, data, func(i, start, end int) error {
		if i > 0 {
			window -= hit(i - after - 1)
			if i+before < lines {
				window += hit(i + before)
			}
		}
		if window == 0 {
			return nil
		}
		if (before > 0 || after > 0) && previous >= 0 && i != previous+1 {
			page.add("--", nil, false)
		}
		if page.wants() {
			sep := "-"
			if hit(i) > 0 {
				sep = ":"
			}
			prefix := path + sep
			if in.LineNums == nil || *in.LineNums {
				prefix = fmt.Sprintf("%s%s%d%s", path, sep, i+1, sep)
			}
			page.add(prefix, data[start:end], false)
		} else {
			page.add("", nil, false)
		}
		previous = i
		return nil
	})
	return cut, err
}
func grepEachLine(ctx context.Context, data []byte, visit func(int, int, int) error) error {
	start, line := 0, 0
	for start < len(data) {
		if line%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		end := bytes.IndexByte(data[start:], '\n')
		if end < 0 {
			end = len(data)
		} else {
			end += start
		}
		if err := visit(line, start, end); err != nil {
			return err
		}
		line++
		start = end + 1
	}
	return nil
}

// Match declared ** paths and brace alternatives in the Go backend too. This
// closes the SDK fallback's basename-only glob gap, without a new dependency.
func grepGlobMatcher(pattern string) (func(string) bool, error) {
	if pattern == "" {
		return func(string) bool { return true }, nil
	}
	if len(pattern) > 4096 {
		return nil, fmt.Errorf("glob exceeds 4096 bytes")
	}
	negative := strings.HasPrefix(pattern, "!")
	if negative {
		pattern = pattern[1:]
	}
	pattern = strings.TrimPrefix(filepath.ToSlash(pattern), "/")
	var parse func(string, int) (string, int, error)
	parse = func(s string, depth int) (string, int, error) {
		if depth > 16 {
			return "", 0, fmt.Errorf("glob nesting too deep")
		}
		var out strings.Builder
		for i := 0; i < len(s); i++ {
			switch s[i] {
			case ',', '}':
				if depth > 0 {
					return out.String(), i, nil
				}
				out.WriteString(regexp.QuoteMeta(s[i : i+1]))
			case '{':
				out.WriteString("(?:")
				j := i + 1
				for {
					part, n, e := parse(s[j:], depth+1)
					if e != nil {
						return "", 0, e
					}
					out.WriteString(part)
					j += n
					if j >= len(s) {
						return "", 0, fmt.Errorf("unclosed glob brace")
					}
					if s[j] == '}' {
						break
					}
					out.WriteByte('|')
					j++
				}
				out.WriteByte(')')
				i = j
			case '*':
				if i+1 < len(s) && s[i+1] == '*' {
					i++
					if i+1 < len(s) && s[i+1] == '/' {
						i++
						out.WriteString("(?:.*/)?")
					} else {
						out.WriteString(".*")
					}
				} else {
					out.WriteString("[^/]*")
				}
			case '?':
				out.WriteString("[^/]")
			case '[':
				j := i + 1
				if j < len(s) && (s[j] == '!' || s[j] == '^') {
					j++
				}
				if j < len(s) && s[j] == ']' {
					j++
				}
				for j < len(s) && s[j] != ']' {
					j++
				}
				if j >= len(s) {
					return "", 0, fmt.Errorf("unclosed glob character class")
				}
				part := s[i : j+1]
				if len(part) > 1 && part[1] == '!' {
					part = "[^" + part[2:]
				}
				out.WriteString(part)
				i = j
			case '\\':
				if i+1 >= len(s) {
					return "", 0, fmt.Errorf("trailing glob escape")
				}
				i++
				out.WriteString(regexp.QuoteMeta(s[i : i+1]))
			default:
				out.WriteString(regexp.QuoteMeta(s[i : i+1]))
			}
		}
		return out.String(), len(s), nil
	}
	text, _, err := parse(pattern, 0)
	if err != nil {
		return nil, err
	}
	re, err := regexp.Compile("^(?:" + text + ")$")
	if err != nil {
		return nil, err
	}
	basename := !strings.Contains(pattern, "/")
	return func(path string) bool {
		if basename {
			path = filepath.Base(path)
		}
		return re.MatchString(path) != negative
	}, nil
}
