package traffic

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	mproxy "github.com/lqqyt2423/go-mitmproxy/proxy"
)

func brotliEncode(t *testing.T, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := brotli.NewWriter(&buf)
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipEncode(t *testing.T, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func withContentEncoding(enc string) flowOpt {
	return func(f *mproxy.Flow) { f.Response.Header.Set("Content-Encoding", enc) }
}

func TestDecodeHTTPBodyBrotli(t *testing.T) {
	plain := []byte(`{"token":"abc123","note":"内网测试账号"}`)
	encoded := brotliEncode(t, plain)
	head := "HTTP 200\nContent-Encoding: br\nContent-Type: application/json;charset=UTF-8\n"
	if got := decodeHTTPBody(head, encoded); !bytes.Equal(got, plain) {
		t.Fatalf("brotli decode = %q, want %q", got, plain)
	}
}

func TestDecodeHTTPBodyStackedEncodings(t *testing.T) {
	plain := []byte(`{"ok":true,"note":"嵌套编码"}`)
	// Header order is the order transforms were applied: gzip first, then br.
	encoded := brotliEncode(t, gzipEncode(t, plain))
	head := "HTTP 200\nContent-Encoding: gzip, br\n"
	if got := decodeHTTPBody(head, encoded); !bytes.Equal(got, plain) {
		t.Fatalf("stacked decode = %q, want %q", got, plain)
	}
}

func TestDecodeHTTPBodyUnsupportedEncodingFallsBack(t *testing.T) {
	body := []byte("raw bytes")
	head := "HTTP 200\nContent-Encoding: x-unknown\n"
	if got := decodeHTTPBody(head, body); !bytes.Equal(got, body) {
		t.Fatalf("unsupported encoding changed body: %q", got)
	}
}

// TestRecordAndGetDecodesBrotliResponse covers both sides of the fix: the UI
// read path must show plaintext, and the full-text index must be built from the
// decoded body rather than the Brotli frame.
func TestRecordAndGetDecodesBrotliResponse(t *testing.T) {
	tr, _ := openTraffic(t)
	plain := []byte(`{"token":"abc123","note":"内网测试账号"}`)
	encoded := brotliEncode(t, plain)

	tr.record(newFlow("api.example.com", "POST", "/v1/login", []byte(`{}`), encoded,
		withRespType("application/json;charset=UTF-8"), withContentEncoding("br")))

	id := onlyExchangeID(t, tr)
	_, resp, err := tr.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !strings.Contains(resp, `"token":"abc123"`) || !strings.Contains(resp, "内网测试账号") {
		t.Fatalf("Get returned undecoded response: %q", resp)
	}

	rows, total, err := tr.Page(PageQuery{Body: "abc123", RespMin: -1, RespMax: -1}, 0, 10)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("decoded body not searchable: total=%d rows=%d", total, len(rows))
	}

	data, _, err := tr.readTrafficPart(t.Context(), id, "response_body", 0, 4096)
	if err != nil {
		t.Fatalf("readTrafficPart: %v", err)
	}
	if !strings.Contains(string(data), `"token":"abc123"`) {
		t.Fatalf("agent read returned undecoded response: %q", data)
	}
}
