package traffic

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Compressible text well above the compression gate and below the inline
// cutoff, so the stored column must carry the prefix and shrink.
func TestInlineBodyCompressionRoundTrip(t *testing.T) {
	tr, _ := openTraffic(t)
	body := []byte(strings.Repeat("compressible-roundtrip-pattern,", 160)) // ~4.6KB
	tr.record(newFlow("inline.example.com", "POST", "/api", body, body))
	id := onlyExchangeID(t, tr)
	for _, column := range []string{"req_body", "resp_body"} {
		var stored []byte
		if err := tr.DB().QueryRow(`SELECT `+column+` FROM exchange_bodies WHERE id=?`, id).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(stored, zstdInlinePrefix) {
			t.Fatalf("%s 未按压缩格式存储", column)
		}
		if len(stored) >= len(body) {
			t.Fatalf("%s 压缩后 %d 字节不小于原文 %d", column, len(stored), len(body))
		}
	}
	req, resp, err := tr.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(req, "compressible-roundtrip-pattern") || !strings.Contains(resp, "compressible-roundtrip-pattern") {
		t.Fatal("Get() 未返回解压后的原文")
	}
}

// Bodies below the compression gate stay raw — a frame plus the prefix cannot
// pay for itself on tiny payloads.
func TestTinyBodyStaysRaw(t *testing.T) {
	tr, _ := openTraffic(t)
	tr.record(newFlow("tiny.example.com", "GET", "/ok", nil, []byte(`{"ok":true}`)))
	var stored []byte
	if err := tr.DB().QueryRow(`SELECT resp_body FROM exchange_bodies`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(stored, zstdInlinePrefix) {
		t.Fatal("小正文不应压缩存储")
	}
}

// A large compressible body spills to a ".zst" blob whose name is the sha256
// of the PLAINTEXT; Blob and BlobRange still serve plaintext.
func TestBlobCompressionRoundTrip(t *testing.T) {
	tr, dir := openTraffic(t)
	body := []byte(strings.Repeat(`{"key":"value","payload":"roundtrip"},`, 17000)) // ~510KB
	tr.record(newFlow("blob.example.com", "GET", "/big", nil, body, withRespType("application/json")))
	var hash string
	if err := tr.DB().QueryRow(`SELECT resp_blob FROM exchange_bodies WHERE resp_blob IS NOT NULL`).Scan(&hash); err != nil {
		t.Fatalf("未溢出为 blob: %v", err)
	}
	sum := sha256.Sum256(body)
	if hash != hex.EncodeToString(sum[:]) {
		t.Fatal("blob 文件名应为原文的 sha256")
	}
	stored := filepath.Join(dir, "_blobs", "sha256", hash[:2], hash+".zst")
	st, err := os.Stat(stored)
	if err != nil {
		t.Fatalf("压缩 blob 不存在: %v", err)
	}
	if st.Size() >= int64(len(body)) {
		t.Fatalf("blob 未压缩: %d >= %d", st.Size(), len(body))
	}

	rc, size, err := tr.Blob(hash)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if size != int64(len(body)) || !bytes.Equal(got, body) {
		t.Fatalf("Blob() 返回 %d 字节（应为 %d 且为原文）", size, len(body))
	}
	data, total, err := tr.BlobRange(hash, 100, 50)
	if err != nil {
		t.Fatal(err)
	}
	if total != int64(len(body)) || !bytes.Equal(data, body[100:150]) {
		t.Fatalf("BlobRange 分页错误: total=%d len=%d", total, len(data))
	}
	data, total, err = tr.BlobRange(hash, int64(len(body)-35), 50)
	if err != nil || total != int64(len(body)) || !bytes.Equal(data, body[len(body)-35:]) {
		t.Fatalf("BlobRange 尾页错误: total=%d len=%d err=%v", total, len(data), err)
	}
	if len(tr.blobCache) != 1 || tr.blobCache[0].hash != hash {
		t.Fatal("repeated pages did not reuse the bounded decoded cache")
	}
}

// Rows written before compression (no prefix inline, raw ".bin" blob) must keep
// reading exactly as before.
func TestLegacyRawBodiesReadable(t *testing.T) {
	tr, dir := openTraffic(t)
	rawBody := []byte(strings.Repeat("legacy-plaintext-body-", 100))
	if _, err := tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES('legacy-1',?, 'legacy.example.com','GET','/old','http://legacy.example.com/old',200,'application/json',0,?,'')`,
		time.Now().Unix(), len(rawBody)); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.DB().Exec(`INSERT INTO exchange_bodies(id,req_head,req_body,req_blob,resp_head,resp_body,resp_blob)
VALUES('legacy-1','','?','','',?,NULL)`, rawBody); err != nil {
		t.Fatal(err)
	}
	_, resp, err := tr.Get("legacy-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp, "legacy-plaintext-body-") {
		t.Fatal("旧内联正文读取失败")
	}

	blobBody := []byte(strings.Repeat("legacy-blob-body-", 40000)) // ~720KB raw .bin
	sum := sha256.Sum256(blobBody)
	hash := hex.EncodeToString(sum[:])
	blobDir := filepath.Join(dir, "_blobs", "sha256", hash[:2])
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobDir, hash+".bin"), blobBody, 0o644); err != nil {
		t.Fatal(err)
	}
	rc, size, err := tr.Blob(hash)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if size != int64(len(blobBody)) || !bytes.Equal(got, blobBody) {
		t.Fatal("旧 .bin blob 读取失败")
	}
}

// The static-asset filter: default list skips HTML/JS/CSS/images/fonts (by
// content-type and by URL extension for mislabeled servers), keeps API
// traffic, and honors both the toggle and a custom rule list.
func TestStaticSkipFilter(t *testing.T) {
	tr, _ := openTraffic(t)
	s := &sink{t: tr}
	recorded := func() (n int) {
		t.Helper()
		if err := tr.DB().QueryRow(`SELECT COUNT(*) FROM exchanges`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, tc := range []struct{ ct, path string }{
		{"text/html; charset=utf-8", "/index.html"},
		{"application/javascript", "/app.js"},
		{"text/css", "/styles/main.css"},
		{"image/png", "/logo.png?v=2"},
		{"font/woff2", "/fonts/x.woff2"},
		{"application/octet-stream", "/bundle.js?v=9"}, // 扩展名兜底
	} {
		s.Response(newFlow("static.example.com", "GET", tc.path, nil, []byte("payload"), withRespType(tc.ct)))
	}
	if n := recorded(); n != 0 {
		t.Fatalf("静态资源被录制：%d 行", n)
	}
	// Content-Type takes precedence even on API routes: HTML responses are
	// intentionally excluded by the default rule list.
	s.Response(newFlow("static.example.com", "GET", "/api/status", nil, []byte("<html/>"), withRespType("text/html")))
	if n := recorded(); n != 0 {
		t.Fatalf("HTML API response should be filtered: %d rows", n)
	}
	s.Response(newFlow("static.example.com", "GET", "/api/data", nil, []byte(`{"a":1}`)))
	if n := recorded(); n != 1 {
		t.Fatalf("API 响应未录制：%d 行", n)
	}

	tr.SetSkipStatic(false)
	s.Response(newFlow("static.example.com", "GET", "/index.html", nil, []byte("<html/>"), withRespType("text/html")))
	if n := recorded(); n != 2 {
		t.Fatalf("关闭过滤后静态资源仍未录制：%d 行", n)
	}

	tr.SetSkipStatic(true)
	tr.SetSkipRules([]string{".pdf"})
	s.Response(newFlow("static.example.com", "GET", "/page.html", nil, []byte("<html/>"), withRespType("text/html")))
	if n := recorded(); n != 3 {
		t.Fatalf("自定义清单不应再跳过 html：%d 行", n)
	}
	s.Response(newFlow("static.example.com", "GET", "/doc.pdf", nil, []byte("%PDF"), withRespType("application/pdf")))
	if n := recorded(); n != 3 {
		t.Fatalf("自定义清单未跳过 pdf：%d 行", n)
	}
}

// The retention sweeper's delete path removes only rows older than the cutoff.
func TestDeleteOlderThan(t *testing.T) {
	tr, _ := openTraffic(t)
	tr.record(newFlow("ttl.example.com", "GET", "/old", nil, []byte("old")))
	tr.record(newFlow("ttl.example.com", "GET", "/new", nil, []byte("new")))
	if _, err := tr.DB().Exec(`UPDATE exchanges SET ts=1000 WHERE url LIKE '%/old'`); err != nil {
		t.Fatal(err)
	}
	n, err := tr.DeleteOlderThan(time.Now().Add(-time.Hour).Unix())
	if err != nil || n != 1 {
		t.Fatalf("DeleteOlderThan=(%d,%v)，应为 (1,nil)", n, err)
	}
	var url string
	if err := tr.DB().QueryRow(`SELECT url FROM exchanges`).Scan(&url); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(url, "/new") {
		t.Fatalf("误删新流量：%s", url)
	}
}

func TestDeleteOlderThanBatches(t *testing.T) {
	tr, _ := openTraffic(t)
	tx, err := tr.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1201; i++ {
		if _, err := tx.Exec(`INSERT INTO exchanges(id,ts,host) VALUES(?,?,?)`, fmt.Sprintf("expired-%d", i), int64(1000), "old.example"); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO exchanges(id,ts,host) VALUES('current',?,?)`, time.Now().Unix(), "new.example"); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	n, err := tr.DeleteOlderThan(time.Now().Add(-time.Hour).Unix())
	if err != nil || n != 1201 {
		t.Fatalf("batched retention removed %d rows: %v", n, err)
	}
	var remaining int
	if err := tr.DB().QueryRow(`SELECT count(*) FROM exchanges WHERE id='current'`).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("current traffic was lost: count=%d err=%v", remaining, err)
	}
}

func TestRetentionRejectsUnsafeRange(t *testing.T) {
	tr, _ := openTraffic(t)
	tr.SetRetention(30)
	for _, invalid := range []int64{-1, 106752, 3651} {
		tr.SetRetention(invalid)
		if got := tr.RetentionDays(); got != 30 {
			t.Fatalf("unsafe retention %d replaced configured value with %d", invalid, got)
		}
	}
}

// Archives stay plaintext (their blob checksum is the sha256 of the content),
// while restoring into a compressed hot store keeps everything readable and
// searchable.
func TestArchiveRoundTripWithCompression(t *testing.T) {
	tr, _ := openTraffic(t)
	const host = "arch.example.com"
	small := []byte(strings.Repeat("archive-small-roundtrip-marker-", 150))    // ~4.3KB → 压缩内联
	big := []byte(strings.Repeat(`{"archive":"big-roundtrip-marker"}`, 18000)) // ~576KB → 压缩 blob
	tr.record(newFlow(host, "GET", "/small", nil, small))
	tr.record(newFlow(host, "GET", "/big", nil, big, withRespType("application/json")))

	stage := t.TempDir()
	if n, err := tr.ExportHosts([]string{host}, stage); err != nil || n != 2 {
		t.Fatalf("ExportHosts=(%d,%v)", n, err)
	}
	// The archived blob must be plaintext (checksum contract of ImportArchive).
	entries, err := os.ReadDir(filepath.Join(stage, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(stage, "blobs", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.HasPrefix(data, zstdInlinePrefix) {
			t.Fatal("归档 blob 不应携带内联压缩前缀")
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != strings.TrimSuffix(e.Name(), ".bin") {
			t.Fatal("归档 blob 校验失败（应为原文 sha256）")
		}
	}

	other, otherDir := openTraffic(t)
	imported, err := other.ImportArchive(stage)
	if err != nil || imported != 2 {
		t.Fatalf("ImportArchive=(%d,%v)", imported, err)
	}
	var ids []string
	rows, err := other.DB().Query(`SELECT id FROM exchanges ORDER BY url`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		_, resp, err := other.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(resp, "roundtrip-marker") {
			t.Fatalf("导入后 %s 正文不可读:\n%s", id, resp[:120])
		}
	}
	// FTS was rebuilt from plaintext: the marker is searchable after import.
	// (The hyphenated marker must be quoted — FTS5 parses barewords at '-'.)
	if other.fts {
		var n int
		if err := other.DB().QueryRow(`SELECT count(*) FROM ex_fts WHERE ex_fts MATCH '"archive-small-roundtrip-marker"'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatal("导入后的全文索引搜不到正文（未用明文重建）")
		}
	}
	// The imported big body landed as a compressed blob in the new store.
	sum := sha256.Sum256(big)
	hash := hex.EncodeToString(sum[:])
	if _, err := os.Stat(filepath.Join(otherDir, "_blobs", "sha256", hash[:2], hash+".zst")); err != nil {
		t.Fatalf("导入后压缩 blob 不存在: %v", err)
	}
}
