package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEnumInputErrorShowsAllowedValuesWithoutEchoingInput(t *testing.T) {
	schema := normalizeToolSchema(map[string]any{"type": "object", "properties": map[string]any{
		"types": map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"finding", "activity"}}},
	}})
	for _, value := range []string{"fact", "invalid-sensitive-input"} {
		input, _ := json.Marshal(map[string]any{"types": []string{value, "finding"}})
		err := validateBusinessInput(schema, input)
		if err == nil || !strings.Contains(err.Error(), `input.types[0]: unsupported value; allowed values: ["finding","activity"]`) || strings.Contains(err.Error(), value) {
			t.Fatalf("ambiguous or unsafe diagnostic: %v", err)
		}
	}
	if err := validateBusinessInput(schema, json.RawMessage(`{"types":["finding","activity"]}`)); err != nil {
		t.Fatal(err)
	}
}
