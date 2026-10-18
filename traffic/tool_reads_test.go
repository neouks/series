package traffic

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	actool "github.com/Autumn-27/norma/tool"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTrafficToolPartsAndStableCursor(t *testing.T) {
	tr, _ := openTraffic(t)
	body := strings.Repeat("中文🙂\"\n", 20000) + "LAST"
	tr.record(newFlow("context.test", "POST", "/one", []byte("request"), []byte(body)))
	id := onlyExchangeID(t, tr)
	read := func(in any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(in)
		r, e := tr.getTool().Call(t.Context(), b, &actool.ToolContext{MaxOutputChars: 4000})
		if e != nil || r.IsError {
			t.Fatal(r.Flatten(), e)
		}
		if len(r.Flatten()) > 4000 || !utf8.ValidString(r.Flatten()) {
			t.Fatal("output overflow")
		}
		var v map[string]any
		if e = json.Unmarshal([]byte(r.Flatten()), &v); e != nil {
			t.Fatal(e)
		}
		return v
	}
	preview := read(map[string]any{"id": id})
	if len(preview["parts"].([]any)) != 4 {
		t.Fatal(preview)
	}
	var joined strings.Builder
	input := map[string]any{"id": id, "part": "response_body", "max_bytes": 8192}
	for i := 0; ; i++ {
		if i > 2000 {
			t.Fatal("non-progressing page")
		}
		out := read(input)
		row := out["parts"].([]any)[0].(map[string]any)
		joined.WriteString(row["text"].(string))
		next, ok := row["next"].(map[string]any)
		if !ok {
			break
		}
		input = next
	}
	if joined.String() != body {
		t.Fatalf("lossy continuation got %d want %d", joined.Len(), len(body))
	}
	read(map[string]any{"id": id, "part": "response_body", "max_bytes": 4})
	tr.record(newFlow("context.test", "GET", "/two", nil, []byte("second")))
	search := func(in map[string]any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(in)
		r, e := tr.searchTool().Call(t.Context(), b, nil)
		if e != nil || r.IsError {
			t.Fatal(r.Flatten(), e)
		}
		var out map[string]any
		json.Unmarshal([]byte(r.Flatten()), &out)
		return out
	}
	first := search(map[string]any{"host": "context.test", "limit": 1})
	if first["has_more"] != true {
		t.Fatal(first)
	}
	tr.record(newFlow("context.test", "GET", "/three", nil, []byte("late")))
	next := search(map[string]any{"host": "context.test", "limit": 1, "cursor": first["next_cursor"]})
	if next["has_more"] != false || len(next["items"].([]any)) != 1 {
		t.Fatal(next)
	}
	filtered := search(map[string]any{"host": "context.test", "method": "post", "status": 200, "contains": "/one"})
	if len(filtered["items"].([]any)) != 1 {
		t.Fatal(filtered)
	}
	r, _ := tr.searchTool().Call(t.Context(), json.RawMessage(`{"host":"context.test","since":2,"until":1}`), nil)
	if !r.IsError {
		t.Fatal("invalid range")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r, _ = tr.searchTool().Call(ctx, json.RawMessage(`{"host":"context.test"}`), nil)
	if !r.IsError {
		t.Fatal("query failure returned empty success")
	}
	for _, tool := range tr.Tools() {
		if tool.Name() == "traffic_blob" {
			t.Fatal("retired tool callable")
		}
	}
}

func TestTrafficBinaryContinuation(t *testing.T) {
	tr, _ := openTraffic(t)
	body := []byte{0x80, 0x81, 0x82, 0x83, 0x84, 0xff, 0xfe, 0xfd, 0xfc, 0xc3, 0x28}
	tr.record(newFlow("binary.test", "GET", "/binary", nil, body))
	id := onlyExchangeID(t, tr)
	var joined []byte
	offset := 0
	for i := 0; i < 10; i++ {
		raw, _ := json.Marshal(map[string]any{"id": id, "part": "response_body", "offset": offset, "max_bytes": 4})
		r, e := tr.getTool().Call(t.Context(), raw, nil)
		if e != nil || r.IsError {
			t.Fatal(r.Flatten(), e)
		}
		var out map[string]any
		json.Unmarshal([]byte(r.Flatten()), &out)
		row := out["parts"].([]any)[0].(map[string]any)
		data := []byte(row["text"].(string))
		if row["encoding"] == "hex" {
			data, e = hex.DecodeString(row["text"].(string))
			if e != nil {
				t.Fatal(e)
			}
		}
		joined = append(joined, data...)
		next, ok := row["next_offset"].(float64)
		if !ok {
			break
		}
		if int(next) <= offset {
			t.Fatal("no forward progress")
		}
		offset = int(next)
	}
	if !bytes.Equal(joined, body) {
		t.Fatalf("binary changed: %x != %x", joined, body)
	}
}
