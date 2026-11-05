package agent

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// These bounds protect the public SDK's uint16 PTY size conversion and lazy
// terminal allocation. They are NOT process admission or an exit notification.
const (
	ptyMaxRows       = 256
	ptyMaxCols       = 512
	ptyMaxCells      = 65536
	ptyMaxWaitMillis = 120000
	ptyMaxInputBytes = 1 << 20
	ptyMaxRegexBytes = 4096
	ptyMaxWatchBytes = 32768
	ptyRingBytes     = 256 << 10
)

func isInteractiveTool(name string) bool {
	switch name {
	case "shell_open", "shell_send", "shell_read", "shell_close", "shell_list":
		return true
	}
	return false
}

func interactiveInputSchema(name string, schema map[string]any) map[string]any {
	if !isInteractiveTool(name) {
		return schema
	}
	// Do not mutate the SDK schema or a user-configured decorator's schema.
	out := normalizeToolSchema(schema)
	props, _ := out["properties"].(map[string]any)
	limit := func(field, key string, value int, note string) {
		if prop, ok := props[field].(map[string]any); ok {
			// A narrower configured limit is authoritative too.
			if prior, ok := prop[key].(float64); !ok || prior > float64(value) {
				prop[key] = float64(value)
			}
			if note != "" {
				desc, _ := prop["description"].(string)
				prop["description"] = desc + " " + note
			}
		}
	}
	itemLimit := func(field string, value int) {
		if prop, ok := props[field].(map[string]any); ok {
			if item, ok := prop["items"].(map[string]any); ok {
				if prior, ok := item["maxLength"].(float64); !ok || prior > float64(value) {
					item["maxLength"] = float64(value)
				}
			}
		}
	}
	limit("session_id", "maxLength", 128, "UTF-8最多128字节。")
	switch name {
	case "shell_open":
		limit("rows", "maximum", ptyMaxRows, "有效rows×cols不超过65536。")
		limit("cols", "maximum", ptyMaxCols, "")
		limit("quiet_ms", "maximum", ptyMaxWaitMillis, "")
		limit("startup_grace_ms", "maximum", ptyMaxWaitMillis, "")
		limit("command", "maxLength", ptyMaxInputBytes, "UTF-8最多1MiB。")
		limit("prompt_regex", "maxLength", ptyMaxRegexBytes, "UTF-8最多4096字节。")
		limit("watch", "maxItems", 32, "UTF-8合计不超过32768字节，每项最多4096字节。")
		itemLimit("watch", ptyMaxRegexBytes)
	case "shell_send":
		limit("quiet_ms", "maximum", ptyMaxWaitMillis, "")
		limit("timeout_ms", "maximum", ptyMaxWaitMillis, "")
		limit("text", "maxLength", ptyMaxInputBytes, "与paste合计UTF-8最多1MiB。")
		limit("paste", "maxLength", ptyMaxInputBytes, "与text共用额度。")
		limit("prompt_regex", "maxLength", ptyMaxRegexBytes, "UTF-8最多4096字节。")
		limit("keys", "maxItems", 1024, "")
		limit("hex", "maxItems", 4096, "")
		itemLimit("keys", 64)
		itemLimit("hex", 32)
	case "shell_read":
		limit("max_bytes", "maximum", ptyRingBytes, "drain仍只读取SDK保留的环形缓冲，omitted表示旧输出已丢失。")
		limit("max_lines", "maximum", ptyRingBytes, "")
	}
	return out
}

func validateInteractiveInput(name string, schema map[string]any, input json.RawMessage) error {
	if !isInteractiveTool(name) {
		return nil
	}
	return validateInteractiveBoundedInput(name, interactiveInputSchema(name, schema), input)
}

func validateInteractiveBoundedInput(name string, bounded map[string]any, input json.RawMessage) error {
	if err := validateBusinessInput(bounded, input); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(input, &fields); err != nil {
		return err
	}
	props, _ := bounded["properties"].(map[string]any)
	texts := map[string]string{}
	arrays := map[string][]string{}
	checkLength := func(field, value string, prop map[string]any) error {
		if n, ok := prop["maxLength"].(float64); ok && float64(utf8.RuneCountInString(value)) > n {
			return fmt.Errorf("%s: maxLength must be <= %g", field, n)
		}
		return nil
	}
	for key, raw := range fields {
		prop, _ := props[key].(map[string]any)
		if prop["type"] == "string" {
			var value string
			if json.Unmarshal(raw, &value) != nil {
				return fmt.Errorf("%s: expected a string", key)
			}
			if err := checkLength(key, value, prop); err != nil {
				return err
			}
			texts[key] = value
		}
		if item, ok := prop["items"].(map[string]any); ok && item["type"] == "string" {
			var values []string
			if json.Unmarshal(raw, &values) != nil {
				return fmt.Errorf("%s: expected a string array", key)
			}
			for _, value := range values {
				if err := checkLength(key+" entry", value, item); err != nil {
					return err
				}
			}
			arrays[key] = values
		}
	}
	if len(texts["session_id"]) > 128 {
		return fmt.Errorf("session_id exceeds 128 UTF-8 bytes")
	}
	integer := func(key string) (int64, error) {
		var n int64
		if raw := fields[key]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &n); err != nil {
				return 0, fmt.Errorf("%s: expected a signed 64-bit integer", key)
			}
		}
		return n, nil
	}
	// Exact integer parsing before the SDK's int/duration conversion also rejects
	// numbers outside int64, rather than turning an ignored decode error into 0.
	for _, key := range []string{"rows", "cols", "quiet_ms", "startup_grace_ms", "timeout_ms", "max_bytes", "max_lines", "since_cursor"} {
		if props[key] == nil {
			continue
		}
		if _, err := integer(key); err != nil {
			return err
		}
	}
	if name == "shell_open" {
		rows, _ := integer("rows")
		cols, _ := integer("cols")
		if rows <= 0 {
			rows = 24
		}
		if cols <= 0 {
			cols = 80
		}
		if rows > ptyMaxRows || cols > ptyMaxCols || rows*cols > ptyMaxCells {
			return fmt.Errorf("PTY grid exceeds resource limit: rows<=256, cols<=512, effective rows*cols<=65536")
		}
	}
	text := func(key string) string { return texts[key] }
	if (name == "shell_open" && len(text("command")) > ptyMaxInputBytes) ||
		(name == "shell_send" && len(text("text"))+len(text("paste")) > ptyMaxInputBytes) {
		return fmt.Errorf("PTY command or combined text/paste exceeds 1 MiB UTF-8 bytes")
	}
	if (name == "shell_open" || name == "shell_send") && len(text("prompt_regex")) > ptyMaxRegexBytes {
		return fmt.Errorf("prompt_regex exceeds 4096 UTF-8 bytes")
	}
	if name == "shell_send" {
		for field, maxBytes := range map[string]int{"keys": 64, "hex": 32} {
			for _, value := range arrays[field] {
				if len(value) > maxBytes {
					return fmt.Errorf("%s entry exceeds %d UTF-8 bytes", field, maxBytes)
				}
			}
		}
	}
	if name != "shell_open" {
		return nil
	}
	total := 0
	for _, pattern := range arrays["watch"] {
		if len(pattern) > ptyMaxRegexBytes {
			return fmt.Errorf("watch pattern exceeds 4096 UTF-8 bytes")
		}
		total += len(pattern)
	}
	if total > ptyMaxWatchBytes {
		return fmt.Errorf("watch patterns exceed 32768 UTF-8 bytes in total")
	}
	return nil
}
