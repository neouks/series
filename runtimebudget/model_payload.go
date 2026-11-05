package runtimebudget

import (
	"encoding/json"
	"fmt"
	"github.com/Autumn-27/norma/llm"
	"reflect"
)

// Check the projected payload before the SDK allocates its wire JSON. This is a
// byte floor over the model-view text/parameters, not tokenization or an exact
// wire size (HTTP encoding is checked separately). Never inspect raw audit data.
func CheckModelInputBytes(req llm.CompletionRequest, limit int64) error {
	left := limit
	add := func(n int) error {
		left -= int64(n)
		if left < 0 {
			return &ModelByteLimitError{Phase: "input", Limit: limit}
		}
		return nil
	}
	for _, s := range req.System {
		if err := add(len(s)); err != nil {
			return err
		}
	}
	if err := add(len(req.Thinking)); err != nil {
		return err
	}
	for _, s := range req.Stop {
		if err := add(len(s)); err != nil {
			return err
		}
	}
	var block func(llm.ContentBlock, int) error
	block = func(b llm.ContentBlock, depth int) error {
		if depth > 10000 {
			return &LocalError{Cause: fmt.Errorf("消息内容嵌套过深或存在循环")}
		}
		if err := add(1); err != nil {
			return err
		}
		for _, s := range []string{b.Text, b.Thinking, b.Signature, b.Name, b.ID, b.ToolUseID, b.ThoughtSignature} {
			if err := add(len(s)); err != nil {
				return err
			}
		}
		if err := add(len(b.Input)); err != nil {
			return err
		}
		for _, child := range b.Content {
			if err := block(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if err := block(b, 0); err != nil {
				return err
			}
		}
	}
	for _, tool := range req.Tools {
		if err := add(len(tool.Name) + len(tool.Description)); err != nil {
			return err
		}
		if err := modelSchemaByteFloor(reflect.ValueOf(tool.InputSchema), add, 0); err != nil {
			return err
		}
	}
	return nil
}

// Normal schemas are JSON maps/slices/scalars. Walk them without marshaling a
// second potentially huge string. The depth guard matches Go JSON's 10K depth;
// malformed host schemas stop locally instead of exhausting a model retry.
func modelSchemaByteFloor(v reflect.Value, add func(int) error, depth int) error {
	if depth > 10000 {
		return &LocalError{Cause: fmt.Errorf("工具Schema嵌套过深或存在循环")}
	}
	if !v.IsValid() {
		return nil
	}
	if v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		return modelSchemaByteFloor(v.Elem(), add, depth+1)
	}
	switch v.Kind() {
	case reflect.String:
		return add(v.Len())
	case reflect.Slice, reflect.Array:
		if v.Type() == reflect.TypeOf(json.RawMessage{}) || v.Type() == reflect.TypeOf([]byte{}) {
			return add(v.Len())
		}
		if err := add(v.Len()); err != nil {
			return err
		}
		for i := 0; i < v.Len(); i++ {
			if err := modelSchemaByteFloor(v.Index(i), add, depth+1); err != nil {
				return err
			}
		}
	case reflect.Map:
		if err := add(v.Len()); err != nil {
			return err
		}
		it := v.MapRange()
		for it.Next() {
			if err := modelSchemaByteFloor(it.Key(), add, depth+1); err != nil {
				return err
			}
			if err := modelSchemaByteFloor(it.Value(), add, depth+1); err != nil {
				return err
			}
		}
	default:
		// Scalars and non-standard host schema values still receive the exact
		// final HTTP limit. Their already-resident Go object is not copied here.
		return add(1)
	}
	return nil
}
