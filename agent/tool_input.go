package agent

import (
	"encoding/json"
	"fmt"
	"reflect"

	actool "github.com/Autumn-27/norma/tool"
)

// Business contracts live at the SERIES tool boundary. In particular the SDK's
// basic type checker does not enforce our ID, batch and response-budget limits.
// JSON normalization also makes code schemas ([]string/int) and stored schemas
// ([]any/float64) behave identically.
func normalizeToolSchema(schema map[string]any) map[string]any {
	b, _ := json.Marshal(schema)
	var normalized map[string]any
	_ = json.Unmarshal(b, &normalized)
	return normalized
}

func validateBusinessInput(schema map[string]any, input json.RawMessage) error {
	if err := actool.ValidateInput(schema, input); err != nil {
		return err
	}
	var value any
	if err := json.Unmarshal(input, &value); err != nil {
		return err
	}
	return validateBusinessBounds(schema, value, "input")
}

func validateBusinessBounds(schema map[string]any, value any, path string) error {
	if values, ok := schema["enum"].([]any); ok {
		found := false
		for _, allowed := range values {
			found = found || reflect.DeepEqual(allowed, value)
		}
		if !found {
			// Report the schema's choices, not the rejected input (which may
			// contain secrets). Role-filtered enums need actionable diagnostics.
			choices, err := json.Marshal(values)
			if err == nil && len(choices) <= 512 {
				return fmt.Errorf("%s: unsupported value; allowed values: %s", path, choices)
			}
			return fmt.Errorf("%s: unsupported value; see the parameter's enum for allowed values", path)
		}
	}
	check := func(n float64, low, high string) error {
		if min, ok := schema[low].(float64); ok && n < min {
			return fmt.Errorf("%s: %s must be >= %g", path, low, min)
		}
		if max, ok := schema[high].(float64); ok && n > max {
			return fmt.Errorf("%s: %s must be <= %g", path, high, max)
		}
		return nil
	}
	switch v := value.(type) {
	case float64:
		return check(v, "minimum", "maximum")
	case []any:
		if err := check(float64(len(v)), "minItems", "maxItems"); err != nil {
			return err
		}
		item, _ := schema["items"].(map[string]any)
		for i, child := range v {
			if err := validateBusinessBounds(item, child, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		for key, child := range v {
			property, _ := props[key].(map[string]any)
			if err := validateBusinessBounds(property, child, path+"."+key); err != nil {
				return err
			}
		}
	}
	return nil
}

type inputPreparer interface {
	PrepareInput(json.RawMessage) (json.RawMessage, error)
}

func PrepareToolInput(tool actool.CoreTool, input json.RawMessage) (json.RawMessage, error) {
	if p, ok := tool.(inputPreparer); ok {
		return p.PrepareInput(input)
	}
	return input, nil
}
