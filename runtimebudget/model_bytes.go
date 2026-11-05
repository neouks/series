package runtimebudget

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

type modelBytePolicyKey struct{}

func WithModelByteLimits(ctx context.Context, limits ModelByteLimits) context.Context {
	if previous, ok := ctx.Value(modelBytePolicyKey{}).(ModelByteLimits); ok && previous == limits {
		return ctx
	}
	return context.WithValue(ctx, modelBytePolicyKey{}, limits)
}

func ModelBytePolicy(ctx context.Context) ModelByteLimits {
	if limits, ok := ctx.Value(modelBytePolicyKey{}).(ModelByteLimits); ok && limits.Validate() == nil {
		return limits
	}
	return ModelByteSnapshot()
}

const MiB int64 = 1024 * 1024

// ModelByteLimits is independent of tokens and time. RequestMiB bounds the
// projected UTF-8 input payload and final HTTP JSON; ResponseMiB bounds bodies
// read across all HTTP attempts in one logical call, including SSE framing.
type ModelByteLimits struct {
	RequestMiB  int `json:"request_mib"`
	ResponseMiB int `json:"response_mib"`
}

func DefaultModelByteLimits() ModelByteLimits {
	return ModelByteLimits{RequestMiB: 16, ResponseMiB: 32}
}

func (b ModelByteLimits) Validate() error {
	if b.RequestMiB < 1 || b.RequestMiB > 256 || b.ResponseMiB < 1 || b.ResponseMiB > 256 {
		return fmt.Errorf("模型请求及累计响应上限须为1..256 MiB")
	}
	return nil
}

func (b *ModelByteLimits) UnmarshalJSON(raw []byte) error {
	type plain ModelByteLimits
	var next plain
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&next); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("模型字节限制必须是一个JSON对象")
	}
	if err := ModelByteLimits(next).Validate(); err != nil {
		return err
	}
	*b = ModelByteLimits(next)
	return nil
}

var modelByteLimits = struct {
	sync.RWMutex
	value ModelByteLimits
}{value: DefaultModelByteLimits()}

func ModelByteSnapshot() ModelByteLimits {
	modelByteLimits.RLock()
	defer modelByteLimits.RUnlock()
	return modelByteLimits.value
}

func ConfigureModelByteLimits(limits ModelByteLimits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	modelByteLimits.Lock()
	defer modelByteLimits.Unlock()
	modelByteLimits.value = limits
	return nil
}

type ModelByteLimitError struct {
	Phase string
	Limit int64
}

func (e *ModelByteLimitError) Error() string {
	return fmt.Sprintf("模型请求资源上限已触发：phase=%s，limit=%d字节；本轮已停止，响应可能不完整，请减少单次输入/输出或调整系统字节限制", e.Phase, e.Limit)
}

func (*ModelByteLimitError) PreventModelReplay() bool { return true }
