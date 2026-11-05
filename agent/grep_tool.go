package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/runtimebudget"
)

const grepQueryTime = 2 * time.Minute
const grepPreviewBytes = 1 << 20
const grepPatternBytes = 200000

type admittedGrep struct {
	actool.CoreTool
	resolver *grepResolver
}

var sharedGrepResolver grepResolver

func newAdmittedGrep() actool.CoreTool {
	return &admittedGrep{CoreTool: actool.NewGrep(), resolver: &sharedGrepResolver}
}
func (t *admittedGrep) Description() string {
	return "Search local files with RE2 regex. Prefer Grep to Bash rg/grep. Filter by path/glob/type; output_mode is files_with_matches (default), content, or count. Page with head_limit (default 250; 0 all within budget) and offset. Use multiline=true for cross-line matches. Results use stable path order."
}
func (t *admittedGrep) Prompt() string {
	return t.CoreTool.Prompt() + " SERIES bounds each search to 120s, 32 MiB raw subprocess output and a 1 MiB rendered page. Omission/error markers mean the search or preview is incomplete; narrow the query or read the source before concluding absence."
}

type grepInput struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	Type       string `json:"type"`
	Mode       string `json:"output_mode"`
	IgnoreCase bool   `json:"-i"`
	LineNums   *bool  `json:"-n"`
	After      int    `json:"-A"`
	Before     int    `json:"-B"`
	Context    int    `json:"-C"`
	Multiline  bool   `json:"multiline"`
	Head       *int   `json:"head_limit"`
	Offset     int    `json:"offset"`
}
type grepAccessError struct{ error }

func grepAbort(ctx context.Context, err error) bool {
	var access *grepAccessError
	return ctx.Err() != nil || runtimebudget.StopsAutomaticReplay(err) || errors.As(err, &access)
}

func (t *admittedGrep) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, grepQueryTime)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return actool.Errorf("Grep not completed: " + err.Error()), nil
	}
	var in grepInput
	if err := json.Unmarshal(input, &in); err != nil {
		return actool.Result{}, err
	}
	if len(in.Pattern) > grepPatternBytes {
		return actool.Errorf("Grep pattern exceeds 200000 UTF-8 bytes; narrow the expression"), nil
	}
	if len(in.Glob) > 4096 || len(in.Type) > 4096 {
		return actool.Errorf("Grep glob/type exceeds 4096 UTF-8 bytes"), nil
	}
	flags := ""
	if in.IgnoreCase {
		flags += "i"
	}
	if in.Multiline {
		flags += "s"
	}
	pattern := in.Pattern
	if flags != "" {
		pattern = "(?" + flags + ")" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return actool.Errorf("Error: invalid regex: " + err.Error()), nil
	}
	if err := ctx.Err(); err != nil {
		return actool.Errorf("Grep not completed: " + err.Error()), nil
	}
	if in.Mode == "" {
		in.Mode = "files_with_matches"
	}
	in.Before, in.After = max(0, in.Before), max(0, in.After)
	if in.Context > 0 {
		in.Before, in.After = in.Context, in.Context
	}
	in.Offset = max(0, in.Offset)
	root := ""
	if tc != nil {
		root = tc.WorkingDir
	}
	if in.Path != "" {
		if filepath.IsAbs(in.Path) || root == "" {
			root = in.Path
		} else {
			root = filepath.Join(root, in.Path)
		}
	}
	if root == "" {
		root = "."
	}
	check := func(c context.Context) error {
		if err := recheckQueuedTool(c, "Grep", input, nil); err != nil {
			return &grepAccessError{err}
		}
		return nil
	}
	rg, err := t.resolver.resolve(ctx, ExternalProcessKey(ctx), check)
	if err != nil {
		return actool.Errorf("Grep not completed: " + err.Error()), nil
	}
	if rg != "" {
		page := newGrepPage(in)
		stderr := &grepText{limit: 4096}
		result, err := runtimebudget.RunProcessStream(ctx, ExternalProcessKey(ctx), grepQueryTime, check, func(c context.Context) (*exec.Cmd, func(), error) {
			dir, target := root, "."
			if stat, e := os.Stat(root); e == nil && !stat.IsDir() {
				dir, target = filepath.Dir(root), filepath.Base(root)
			}
			cmd := exec.CommandContext(c, rg, grepArgs(in, target)...)
			cmd.Dir = dir
			return cmd, nil, nil
		}, page, stderr)
		page.finish()
		if err == nil {
			return actool.Text(actool.Capture(tc, page.render(""))), nil
		}
		if grepAbort(ctx, err) {
			return actool.Errorf(actool.Capture(tc, page.render("Grep未完成："+err.Error()+"；不能据此判断未命中"))), nil
		}
		if result.Started && result.ExitCode == 1 {
			return actool.Text("No matches found"), nil
		}
		// Preserve the SDK's Go fallback on a genuine rg start/search failure,
		// never on cancellation, admission, authorization or output budget stops.
	}
	release, queued, err := runtimebudget.AcquireProcess(ctx, ExternalProcessKey(ctx))
	if err != nil {
		return actool.Errorf("Grep not completed: " + err.Error()), nil
	}
	defer release()
	if queued {
		if err = check(ctx); err != nil {
			return actool.Errorf(err.Error()), nil
		}
	}
	page := newGrepPage(in)
	warning, err := grepGo(ctx, root, in, re, page)
	if err != nil {
		return actool.Errorf(actool.Capture(tc, page.render("Grep未完成："+err.Error()+"；不能据此判断未命中"))), nil
	}
	return actool.Text(actool.Capture(tc, page.render(warning))), nil
}

func grepArgs(in grepInput, target string) []string {
	// rg's parallel completion order is unstable across offset pages.
	args := []string{"--color", "never", "--sort", "path"}
	if in.IgnoreCase {
		args = append(args, "-i")
	}
	if in.Multiline {
		args = append(args, "-U", "--multiline-dotall")
	}
	switch in.Mode {
	case "files_with_matches":
		args = append(args, "-l")
	case "count":
		args = append(args, "-c")
	default:
		args = append(args, "--no-heading")
		if in.LineNums == nil || *in.LineNums {
			args = append(args, "-n")
		}
		if in.Before > 0 {
			args = append(args, "-B", strconv.Itoa(in.Before))
		}
		if in.After > 0 {
			args = append(args, "-A", strconv.Itoa(in.After))
		}
	}
	if in.Glob != "" {
		args = append(args, "--glob", in.Glob)
	}
	if in.Type != "" {
		if _, known := grepTypes[strings.ToLower(in.Type)]; known {
			args = append(args, "--type", in.Type)
		} else {
			args = append(args, "--glob", "*."+in.Type)
		}
	}
	return append(args, "-e", in.Pattern, "--", target)
}

// Page only the requested records while counting the bounded stdout stream.
// This avoids collecting all matches merely to return the first ten/250.
type grepPage struct {
	offset, limit, total, shown int
	line                        []byte
	text                        strings.Builder
	lineCut, cut                bool
}

func newGrepPage(in grepInput) *grepPage {
	limit := 250
	if in.Head != nil {
		limit = *in.Head
	}
	return &grepPage{offset: in.Offset, limit: limit}
}
func (p *grepPage) wants() bool {
	return p.total >= p.offset && (p.limit <= 0 || p.shown < p.limit) && p.text.Len() < grepPreviewBytes
}
func (p *grepPage) add(prefix string, body []byte, truncated bool) {
	keep := p.wants()
	p.total++
	if !keep {
		return
	}
	p.shown++
	left := grepPreviewBytes - p.text.Len()
	if len(body) > grepPreviewBytes {
		body = body[:grepPreviewBytes]
		truncated = true
	}
	bodyText := string(body)
	if truncated && len(bodyText) > 0 {
		start := len(bodyText) - 1
		for start > 0 && len(bodyText)-start < utf8.UTFMax && !utf8.RuneStart(bodyText[start]) {
			start--
		}
		if !utf8.FullRuneInString(bodyText[start:]) {
			bodyText = bodyText[:start]
		}
	}
	for _, part := range []string{prefix, bodyText, "\n"} {
		if len(part) > left {
			part = grepRuneHead(part, left)
			p.cut = true
		}
		p.text.WriteString(part)
		left = grepPreviewBytes - p.text.Len()
	}
	p.cut = p.cut || truncated
}
func (p *grepPage) Write(data []byte) (int, error) {
	n := len(data)
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		take := len(data)
		if i >= 0 {
			take = i
		}
		space := grepPreviewBytes - len(p.line)
		copied := min(space, take)
		p.line = append(p.line, data[:copied]...)
		p.lineCut = p.lineCut || copied < take
		if i < 0 {
			break
		}
		p.add("", p.line, p.lineCut)
		p.line = p.line[:0]
		p.lineCut = false
		data = data[i+1:]
	}
	return n, nil
}
func (p *grepPage) finish() {
	if len(p.line) > 0 || p.lineCut {
		p.add("", p.line, p.lineCut)
		p.line = nil
		p.lineCut = false
	}
}
func (p *grepPage) render(warning string) string {
	if p.total == 0 && warning == "" {
		return "No matches found"
	}
	text := p.text.String()
	if more := max(0, p.total-p.offset-p.shown); more > 0 {
		text += fmt.Sprintf("... [%d more result(s) — page with offset=%d, or head_limit=0 for all] ...\n", more, p.offset+p.shown)
	}
	if p.cut {
		text += "\n[Grep预览已截断；用offset读取其他条目，超长单行须回读原文件；不能把预览作为完整证据]\n"
	}
	if warning != "" {
		text += "\n[" + warning + "]\n"
	}
	return text
}
func grepRuneHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	n = max(0, n)
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

type grepText struct {
	buf   bytes.Buffer
	limit int
	cut   bool
}

func (w *grepText) Write(p []byte) (int, error) {
	n := min(max(0, w.limit-w.buf.Len()), len(p))
	w.buf.Write(p[:n])
	w.cut = w.cut || n < len(p)
	return len(p), nil
}

// Keep one configuration/result, not an unbounded PATH-keyed cache. Canceled
// setup is retryable; actual failed installation is attempted once per PATH.
type grepResolver struct {
	mu           sync.Mutex
	flight       chan struct{}
	config, path string
	attempted    bool
}

func (r *grepResolver) resolve(ctx context.Context, key string, check func(context.Context) error) (string, error) {
	if os.Getenv("NORMA_DISABLE_RIPGREP") != "" {
		return "", nil
	}
	if p, err := exec.LookPath("rg"); err == nil {
		return p, nil
	}
	config := os.Getenv("PATH")
	for {
		r.mu.Lock()
		if r.flight != nil {
			ch := r.flight
			r.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-ch:
				continue
			}
		}
		if r.attempted && r.config == config {
			p := r.path
			r.mu.Unlock()
			if p == "" {
				return "", nil
			}
			if stat, e := os.Stat(p); e == nil && !stat.IsDir() {
				return p, nil
			}
			r.mu.Lock()
			r.attempted = false
			r.mu.Unlock()
			continue
		}
		if os.Getenv("NORMA_RIPGREP_NO_INSTALL") != "" {
			r.mu.Unlock()
			return "", nil
		}
		r.flight = make(chan struct{})
		ch := r.flight
		r.mu.Unlock()
		p, err := installGrep(ctx, key, check)
		r.mu.Lock()
		if err == nil {
			r.config, r.path, r.attempted = config, p, true
		}
		r.flight = nil
		close(ch)
		r.mu.Unlock()
		return p, err
	}
}
func installGrep(ctx context.Context, key string, check func(context.Context) error) (string, error) {
	npm, err := exec.LookPath("npm")
	if err != nil {
		return "", nil
	}
	run := func(args []string, stdout io.Writer) (runtimebudget.ProcessResult, error) {
		return runtimebudget.RunProcessStream(ctx, key, grepQueryTime, check, func(c context.Context) (*exec.Cmd, func(), error) {
			return exec.CommandContext(c, npm, args...), nil, nil
		}, stdout, io.Discard)
	}
	_, err = run([]string{"install", "-g", "@vscode/ripgrep"}, io.Discard)
	if err != nil {
		if grepAbort(ctx, err) {
			return "", err
		}
		return "", nil
	}
	if p, e := exec.LookPath("rg"); e == nil {
		return p, nil
	}
	root := &grepText{limit: 65536}
	_, err = run([]string{"root", "-g"}, root)
	if err != nil {
		if grepAbort(ctx, err) {
			return "", err
		}
		return "", nil
	}
	if root.cut {
		return "", nil
	}
	name := "rg"
	if runtime.GOOS == "windows" {
		name = "rg.exe"
	}
	p := filepath.Join(strings.TrimSpace(root.buf.String()), "@vscode", "ripgrep", "bin", name)
	if stat, e := os.Stat(p); e == nil && !stat.IsDir() {
		p, e = filepath.Abs(p)
		return p, e
	}
	return "", nil
}
