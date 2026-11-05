package runtimebudget

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// IsContextCapacityRejection deliberately excludes generic "too long" errors:
// a long URL, password, output, or ordinary rate limit is not a context window.
// The pinned SDK exposes HTTP failures as strings, not typed API errors.
func IsContextCapacityRejection(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	if len(s) > 16384 {
		s = s[:16384]
	}
	s = strings.ToLower(s)
	if status := capacityAnyStatus.FindStringSubmatch(s); len(status) == 2 && status[1] != "400" && status[1] != "413" && status[1] != "422" {
		return false
	}
	for _, marker := range []string{
		"context_length_exceeded", "context length exceeded", "maximum context length",
		"context window exceeded", "prompt is too long", "input is too long for",
		"exceeds the context window", "exceeds the model's context", "exceeds the model context",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return capacityStatus413.MatchString(s)
}

var capacityStatus413 = regexp.MustCompile(`\b(?:status|http)\s+(?:code\s+)?413\b`)
var capacityAnyStatus = regexp.MustCompile(`\b(?:status|http)\s+(?:code\s+)?([0-9]{3})\b`)
var capacityWindows = []*regexp.Regexp{
	regexp.MustCompile(`(?i)maximum context length is\s+([0-9][0-9,]*)\s+tokens`),
	regexp.MustCompile(`(?i)context (?:window|length) (?:of|is)\s+([0-9][0-9,]*)\s+tokens`),
	regexp.MustCompile(`(?i)(?:prompt|input) is too long:\s*[0-9][0-9,]*\s*tokens\s*>\s*([0-9][0-9,]*)\s*(?:maximum|tokens)`),
}

// RejectedContextWindow accepts only an explicit window in a known rejection.
// It must not confuse requested max_tokens, token rate limits, or input size
// with the endpoint's actual capacity. Zero means unknown, never unlimited.
func RejectedContextWindow(err error) int {
	if !IsContextCapacityRejection(err) {
		return 0
	}
	text := err.Error()
	if len(text) > 16384 {
		text = text[:16384]
	}
	for _, pattern := range capacityWindows {
		if match := pattern.FindStringSubmatch(text); len(match) == 2 {
			n, parseErr := strconv.Atoi(strings.ReplaceAll(match[1], ",", ""))
			if parseErr == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// CapacityError is a sanitized remote rejection (or its identical-request
// receipt). The wording remains recognizable by the official compactor, which
// may retry a genuinely smaller view. Project-level replay/failover must stop.
// No request text, API key, or raw response is retained here.
type CapacityError struct {
	ConfiguredWindow int
	ReportedWindow   int
	Repeated         bool
}

func (e *CapacityError) Error() string {
	return fmt.Sprintf("模型上下文超限（context length exceeded）：配置窗口=%d token，服务端明确返回窗口=%d token（0表示未知），相同已拒绝请求=%t；可压缩后重试或调整模型配置，不能重放整个Worker", e.ConfiguredWindow, e.ReportedWindow, e.Repeated)
}

func (*CapacityError) PreventModelReplay() bool { return true }

// InvalidOutputBudget cannot be fixed by removing old conversation messages.
// Do not label it a prompt overflow or ask the SDK to summarize repeatedly.
type InvalidOutputBudget struct{ Window, Output int }

func (e *InvalidOutputBudget) Error() string {
	return fmt.Sprintf("模型输出预算无效：输出上限%d token不能达到或超过已配置上下文窗口%d token；请调整模型配置", e.Output, e.Window)
}

func (*InvalidOutputBudget) PreventModelReplay() bool { return true }

// MeasuredCapacityError uses the endpoint's own completed input accounting for
// an identical prompt, never an estimate or a cached count for a changed prompt.
// The wording lets the official compactor try a smaller request; the host must
// not restart the Worker or switch profiles to repeat already performed work.
type MeasuredCapacityError struct {
	Window, Input, Output int
}

func (e *MeasuredCapacityError) Error() string {
	return fmt.Sprintf("模型上下文超限（context length exceeded）：相同输入的服务端实测用量=%d token，输出预留=%d token，有效窗口=%d token；请压缩可恢复历史或调整模型配置，必要约束未被删除", e.Input, e.Output, e.Window)
}

func (*MeasuredCapacityError) PreventModelReplay() bool { return true }
