package traffic

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTrafficToolReadsLegacyBlobAndMissingFile(t *testing.T) {
	tr, dir := openTraffic(t)
	body := strings.Repeat("中文原始流量", 20000)
	blob := tr.spill([]byte(body), "text/plain")
	if blob.hash == "" {
		t.Fatal("fixture did not spill")
	}
	const id = "legacy-read-review"
	rel := filepath.Join("legacy.test", "GET", id)
	if e := os.MkdirAll(filepath.Join(dir, rel), 0700); e != nil {
		t.Fatal(e)
	}
	name := filepath.Join(dir, rel, "response.http")
	raw := fmt.Sprintf("HTTP/1.1 200 OK\nContent-Type: text/plain\n\npreview\n…[truncated] @blob sha256:%s (len=%d)", blob.hash, len(body))
	if e := os.WriteFile(name, []byte(raw), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path) VALUES(?,1,'legacy.test','GET','/','http://legacy.test/',200,'text/plain',0,?,?)`, id, len(body), rel); e != nil {
		t.Fatal(e)
	}
	var joined strings.Builder
	for offset := int64(0); ; {
		page, total, e := tr.readTrafficPart(t.Context(), id, "response_body", offset, 4096)
		if e != nil {
			t.Fatal(e)
		}
		joined.Write(page)
		offset += int64(len(page))
		if offset >= total {
			break
		}
		if len(page) == 0 {
			t.Fatal("no progress")
		}
	}
	if joined.String() != body {
		t.Fatalf("legacy Blob unavailable: got %d want %d", joined.Len(), len(body))
	}
	if e := os.Remove(name); e != nil {
		t.Fatal(e)
	}
	if _, _, e := tr.readTrafficPart(t.Context(), id, "response_body", 0, 4096); e == nil {
		t.Fatal("missing body reported as empty success")
	}
}
