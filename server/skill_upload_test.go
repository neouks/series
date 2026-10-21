package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/text/encoding/simplifiedchinese"
)

func TestValidSkillName(t *testing.T) {
	ok := []string{"web-recon", "a", "nuclei2", "中文技能", "端口扫描-x", "日本語スキル"}
	bad := []string{
		"", "Web-Recon", "-lead", "trail-", "dou--ble", "has space", "中文 技能",
		"dot.name", "a/b", `a\b`, "..", ".", "中文/技能", "sk\x00ill", "中‮文",
		strings.Repeat("a", 65), "1abc", "中文技能!",
	}
	for _, n := range ok {
		if !validSkillName(n) {
			t.Errorf("validSkillName(%q) = false, want true", n)
		}
	}
	for _, n := range bad {
		if validSkillName(n) {
			t.Errorf("validSkillName(%q) = true, want false", n)
		}
	}
}

func TestSkillRelPath(t *testing.T) {
	ok := map[string]string{
		"SKILL.md":           "SKILL.md",
		"scripts/run.py":     "scripts/run.py",
		"参考/中文说明.md":         "参考/中文说明.md",
		"references/a b.txt": "references/a b.txt",
		"./SKILL.md":         "SKILL.md",
		"assets/图片-1_v2.png": "assets/图片-1_v2.png",
	}
	for in, want := range ok {
		got, msg := skillRelPath(in)
		if msg != "" || got != want {
			t.Errorf("skillRelPath(%q) = (%q, %q), want (%q, \"\")", in, got, msg, want)
		}
	}
	bad := []string{
		"", "../etc/passwd", "a/../../b", "/abs/path", "a//b", `..\..\x`,
		`scripts\run.sh`, `\\server\share\x`, "C:/abs/x", "C:\\abs\\x",
		"%2e%2e/x", "a\x00b", "中文 名.md", "中‮文.md", "a#b.md", "a?b.md",
		"a:b.md", string([]byte{0xd6, 0xd0}) + ".md", // 裸 GBK 字节：非法 UTF-8
		strings.Repeat("a", maxSkillPathLen+1),
	}
	for _, in := range bad {
		if got, msg := skillRelPath(in); msg == "" {
			t.Errorf("skillRelPath(%q) = (%q, \"\"), want rejection", in, got)
		}
	}
}

// TestWalkSkillFilesSlashPaths is the Windows regression guard: filepath.Rel returns
// "scripts\run.sh" there, the client echoes that back, and skillRelPath refuses
// backslashes — so files in subdirectories were listed but could not be read or saved.
func TestWalkSkillFilesSlashPaths(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %q: %v", rel, err)
		}
	}
	mustWrite("SKILL.md", "---\nname: demo\n---\n")
	mustWrite("scripts/run.sh", "#!/bin/sh\necho hi\n")
	mustWrite("参考/中文说明.md", "x")

	entries, err := walkSkillFiles(root)
	if err != nil {
		t.Fatalf("walkSkillFiles: %v", err)
	}
	got := make(map[string]bool, len(entries))
	for _, e := range entries {
		if strings.Contains(e, `\`) {
			t.Errorf("entry %q uses a backslash; skill paths must be / separated", e)
		}
		got[e] = true
	}
	for _, want := range []string{"SKILL.md", "scripts/", "scripts/run.sh", "参考/", "参考/中文说明.md"} {
		if !got[want] {
			t.Errorf("walkSkillFiles missing %q (got %v)", want, entries)
		}
	}
	// Whatever the list endpoint returns must pass the check the read/write
	// endpoints apply — that round trip is exactly what broke on Windows.
	for _, e := range entries {
		if strings.HasSuffix(e, "/") {
			continue
		}
		if _, msg := skillRelPath(e); msg != "" {
			t.Errorf("skillRelPath(%q) rejected a path the server itself listed: %s", e, msg)
		}
	}
}

// TestSkillFileRoundTripNested drives list → save → read for a nested file through
// the real handlers, using the path the list returned.
func TestSkillFileRoundTripNested(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "demo", "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "demo", "SKILL.md"), []byte("---\nname: demo\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "demo", "scripts", "run.sh"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{skillDir: dir}

	listReq := httptest.NewRequest(http.MethodGet, "/api/skills/demo/files", nil)
	listReq.SetPathValue("name", "demo")
	listRR := httptest.NewRecorder()
	s.fsListFiles(listRR, listReq)
	if listRR.Code != 200 {
		t.Fatalf("list status = %d, body = %s", listRR.Code, listRR.Body)
	}
	var listed struct{ Files []string }
	if err := json.Unmarshal(listRR.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	// The list is the only path source the UI has, so the rest of the test uses
	// exactly what it returned.
	rel := ""
	for _, f := range listed.Files {
		if strings.Contains(f, `\`) {
			t.Fatalf("listed file %q uses a backslash; must be / separated", f)
		}
		if f == "scripts/run.sh" {
			rel = f
		}
	}
	if rel == "" {
		t.Fatalf("list did not report scripts/run.sh (got %v)", listed.Files)
	}
	writeReq := httptest.NewRequest(http.MethodPut, "/api/skills/demo/files/"+rel, strings.NewReader(`{"content":"echo hi"}`))
	writeReq.SetPathValue("name", "demo")
	writeReq.SetPathValue("file", rel)
	writeRR := httptest.NewRecorder()
	s.fsWriteFile(writeRR, writeReq)
	if writeRR.Code != 200 {
		t.Fatalf("save status = %d, body = %s", writeRR.Code, writeRR.Body)
	}

	readReq := httptest.NewRequest(http.MethodGet, "/api/skills/demo/files/"+rel, nil)
	readReq.SetPathValue("name", "demo")
	readReq.SetPathValue("file", rel)
	readRR := httptest.NewRecorder()
	s.fsReadFile(readRR, readReq)
	if readRR.Code != 200 {
		t.Fatalf("read status = %d, body = %s", readRR.Code, readRR.Body)
	}
	var out struct{ Content, File string }
	if err := json.Unmarshal(readRR.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode read: %v", err)
	}
	if out.Content != "echo hi" {
		t.Errorf("content = %q, want %q", out.Content, "echo hi")
	}
	// The path echoed back must be the slash form the client already holds.
	for _, f := range listed.Files {
		if strings.Contains(f, `\`) {
			t.Errorf("listed file %q uses a backslash", f)
		}
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// zipFile describes one entry for buildZip.
type zipFile struct {
	name    string
	body    string
	method  uint16
	nonUTF8 bool // write the name bytes as-is (GBK 包)
}

func buildZip(t *testing.T, files ...zipFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	zw.RegisterCompressor(zipMethodZstd, zstd.ZipCompressor())
	// Deflate64 没有纯 Go 编码器；这里原样写入，只是为了给条目打上 method 9 的标记 ——
	// 断言的是「方法不支持时给什么提示」，不会真去解压它。
	zw.RegisterCompressor(zipMethodDeflate64, func(w io.Writer) (io.WriteCloser, error) {
		return nopWriteCloser{w}, nil
	})
	for _, f := range files {
		h := &zip.FileHeader{Name: f.name, Method: f.method, NonUTF8: f.nonUTF8}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatalf("CreateHeader(%q): %v", f.name, err)
		}
		if _, err := io.WriteString(w, f.body); err != nil {
			t.Fatalf("write %q: %v", f.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// uploadZip posts raw zip bytes to fsUploadSkill and returns the response.
func uploadZip(t *testing.T, skillDir string, filename string, data []byte) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/skills/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	(&Server{skillDir: skillDir}).fsUploadSkill(rr, req)

	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr, out
}

const zhSkillMD = "---\nname: 中文技能\ndescription: 测试\n---\n正文\n"

// A zstd-compressed archive (WinZip 的可选压缩方式) used to blow up with
// "zip: unsupported compression"; it now installs like any Deflate archive.
func TestUploadSkillZstdAndChineseNames(t *testing.T) {
	dir := t.TempDir()
	data := buildZip(t,
		zipFile{name: "中文技能/SKILL.md", body: zhSkillMD, method: zipMethodZstd},
		zipFile{name: "中文技能/参考/说明 文档.md", body: "参考", method: zipMethodZstd},
		zipFile{name: "中文技能/scripts/run.py", body: "print(1)", method: zip.Deflate},
	)
	rr, out := uploadZip(t, dir, "中文技能.zip", data)
	if rr.Code != 201 {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
	}
	if out["name"] != "中文技能" {
		t.Fatalf("name = %v, want 中文技能", out["name"])
	}
	for _, rel := range []string{"SKILL.md", "参考/说明 文档.md", "scripts/run.py"} {
		if _, err := os.Stat(filepath.Join(dir, "中文技能", rel)); err != nil {
			t.Errorf("missing extracted file %q: %v", rel, err)
		}
	}
}

// GBK-named entries (7-Zip / 资源管理器 on Chinese Windows) must be decoded rather
// than rejected as invalid UTF-8 paths.
func TestUploadSkillGBKNames(t *testing.T) {
	gbk := func(s string) string {
		b, err := simplifiedchinese.GBK.NewEncoder().String(s)
		if err != nil {
			t.Fatalf("gbk encode %q: %v", s, err)
		}
		return b
	}
	dir := t.TempDir()
	data := buildZip(t,
		zipFile{name: gbk("中文技能/SKILL.md"), body: zhSkillMD, method: zip.Deflate, nonUTF8: true},
		zipFile{name: gbk("中文技能/参考资料.md"), body: "内容", method: zip.Deflate, nonUTF8: true},
	)
	rr, out := uploadZip(t, dir, "skill.zip", data)
	if rr.Code != 201 {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
	}
	if out["name"] != "中文技能" {
		t.Fatalf("name = %v, want 中文技能", out["name"])
	}
	if _, err := os.Stat(filepath.Join(dir, "中文技能", "参考资料.md")); err != nil {
		t.Errorf("GBK-named entry not extracted: %v", err)
	}
}

// An archive we genuinely cannot decode should name the method in Chinese instead of
// surfacing "zip: unsupported compression algorithm".
func TestUploadSkillUnsupportedMethod(t *testing.T) {
	data := buildZip(t,
		zipFile{name: "demo/SKILL.md", body: "---\nname: demo\n---\n", method: zipMethodDeflate64},
	)
	rr, out := uploadZip(t, t.TempDir(), "demo.zip", data)
	if rr.Code != 400 {
		t.Fatalf("status = %d, want 400 (body %s)", rr.Code, rr.Body)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "Deflate64") || !strings.Contains(msg, "不支持的压缩方式") {
		t.Fatalf("error = %q, want a Chinese message naming Deflate64", msg)
	}
}

func TestUploadSkillEncrypted(t *testing.T) {
	data := buildZip(t, zipFile{name: "demo/SKILL.md", body: "---\nname: demo\n---\n", method: zip.Deflate})
	// flip the "encrypted" general-purpose flag bit in the local file header
	// (offset 6) and in the central directory copy (offset 8).
	local := bytes.Index(data, []byte("PK\x03\x04"))
	central := bytes.Index(data, []byte("PK\x01\x02"))
	if local < 0 || central < 0 {
		t.Fatal("could not locate zip headers")
	}
	data[local+6] |= 1
	data[central+8] |= 1

	rr, out := uploadZip(t, t.TempDir(), "demo.zip", data)
	if rr.Code != 400 {
		t.Fatalf("status = %d, want 400 (body %s)", rr.Code, rr.Body)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "已加密") {
		t.Fatalf("error = %q, want 加密 hint", msg)
	}
}

// Zip-slip must still be refused now that the path check accepts Unicode.
func TestUploadSkillRejectsTraversal(t *testing.T) {
	data := buildZip(t,
		zipFile{name: "demo/SKILL.md", body: "---\nname: demo\n---\n", method: zip.Deflate},
		zipFile{name: "demo/../../evil.sh", body: "rm -rf /", method: zip.Deflate},
	)
	dir := t.TempDir()
	rr, out := uploadZip(t, dir, "demo.zip", data)
	if rr.Code != 400 {
		t.Fatalf("status = %d, want 400 (body %s)", rr.Code, rr.Body)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "非法路径") {
		t.Fatalf("error = %q, want 非法路径", msg)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("upload left files behind: %v", entries)
	}
}

func TestSkillNameFromFrontmatterQuoted(t *testing.T) {
	got := skillNameFromFrontmatter([]byte("---\nname: \"中文技能\"\ndescription: x\n---\n"))
	if got != "中文技能" {
		t.Fatalf("name = %q, want 中文技能", got)
	}
}
