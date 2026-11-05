package runtimebudget

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// ModelDeadlines bounds queue residence separately from a logical model call.
// All values are seconds. A call includes SDK rate limiting and retries, but
// excludes its prior wait for a host execution slot.
type ModelDeadlines struct {
	QueueSeconds    int `json:"queue_seconds"`
	HeaderSeconds   int `json:"header_seconds"`
	ReadIdleSeconds int `json:"read_idle_seconds"`
	TotalSeconds    int `json:"total_seconds"`
}

func DefaultModelDeadlines() ModelDeadlines {
	return ModelDeadlines{QueueSeconds: 180, HeaderSeconds: 180, ReadIdleSeconds: 180, TotalSeconds: 900}
}

func (d ModelDeadlines) Validate() error {
	if d.QueueSeconds < 1 || d.QueueSeconds > 3600 || d.HeaderSeconds < 1 || d.HeaderSeconds > 3600 || d.ReadIdleSeconds < 1 || d.ReadIdleSeconds > 3600 || d.TotalSeconds < 1 || d.TotalSeconds > 7200 {
		return fmt.Errorf("模型排队、响应头及读取空闲时限须为1..3600秒，总时限须为1..7200秒")
	}
	if d.TotalSeconds < d.HeaderSeconds || d.TotalSeconds < d.ReadIdleSeconds {
		return fmt.Errorf("模型总时限不能小于响应头或读取空闲时限")
	}
	return nil
}

func (d *ModelDeadlines) UnmarshalJSON(raw []byte) error {
	type plain ModelDeadlines
	var next plain
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&next); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("模型时限必须是一个JSON对象")
	}
	if err := ModelDeadlines(next).Validate(); err != nil {
		return err
	}
	*d = ModelDeadlines(next)
	return nil
}

var modelDeadlines = struct {
	sync.RWMutex
	value ModelDeadlines
}{value: DefaultModelDeadlines()}

func ModelDeadlineSnapshot() ModelDeadlines {
	modelDeadlines.RLock()
	defer modelDeadlines.RUnlock()
	return modelDeadlines.value
}

func ConfigureModelDeadlines(d ModelDeadlines) error {
	if err := d.Validate(); err != nil {
		return err
	}
	modelDeadlines.Lock()
	defer modelDeadlines.Unlock()
	modelDeadlines.value = d
	return nil
}

// RequestTimeout is a local request-policy stop, including one after partial
// remote output. Unlike LocalError it does not imply that nothing was sent.
// Retrying a whole Worker/model request automatically could duplicate work.
type RequestTimeout struct {
	Phase string
	Limit time.Duration
}

func (e *RequestTimeout) Error() string {
	label := map[string]string{"queue": "排队", "headers": "等待响应头", "read_idle": "读取响应空闲", "total": "执行总时间"}[e.Phase]
	if label == "" {
		label = "执行"
	}
	return fmt.Sprintf("模型请求%s超过时限（%s），本次请求已停止", label, e.Limit)
}
func (*RequestTimeout) Unwrap() error            { return context.DeadlineExceeded }
func (*RequestTimeout) PreventModelReplay() bool { return true }

func (*RequestTimeout) Timeout() bool   { return true }
func (*RequestTimeout) Temporary() bool { return false }
