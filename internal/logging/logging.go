package logging

import (
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

type Fields = logrus.Fields

// Keep application capture separate from dependency loggers, which may emit
// network or protocol details that do not belong in the system log history.
var logger = logrus.New()

func Component(name string) *logrus.Entry {
	return logger.WithField("component", name)
}

// Configure sets up the application logger and redirects existing stdlib logs.
// Call once at startup, before starting services.
func Configure(out io.Writer, format, minimumLevel string, hook logrus.Hook) error {
	var formatter logrus.Formatter
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "json":
		formatter = &logrus.JSONFormatter{TimestampFormat: time.RFC3339Nano}
	case "text":
		formatter = &logrus.TextFormatter{DisableColors: true, FullTimestamp: true, TimestampFormat: time.RFC3339Nano}
	default:
		return fmt.Errorf("SERIES_LOG_FORMAT must be json or text")
	}
	level := logrus.InfoLevel
	if value := strings.TrimSpace(minimumLevel); value != "" {
		parsed, err := logrus.ParseLevel(value)
		if err != nil || parsed < logrus.ErrorLevel || parsed > logrus.DebugLevel {
			return fmt.Errorf("SERIES_LOG_LEVEL must be debug, info, warn or error")
		}
		level = parsed
	}
	logger.SetFormatter(formatter)
	logger.SetOutput(out)
	logger.SetLevel(level)
	logger.AddHook(hook)
	log.SetFlags(0)
	log.SetOutput(legacyWriter{})
	return nil
}

// legacyWriter keeps the historical level inference for callers that still
// use log.Printf. Structured entries supply their own level and component.
type legacyWriter struct{}

func (legacyWriter) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	if msg == "" {
		return len(p), nil
	}
	level := legacyLevel(msg)
	component := "app"
	if rest, ok := strings.CutPrefix(msg, "["); ok {
		if tag, body, found := strings.Cut(rest, "]"); found && tag != "" {
			component = tag
			msg = strings.TrimSpace(body)
		}
	}
	Component(component).Log(level, msg)
	return len(p), nil
}

func legacyLevel(msg string) logrus.Level {
	lower := strings.ToLower(msg)
	for _, word := range []string{"fatal", "panic", "error", "err:", "失败", "丢弃", "拒绝", "✕", "不可达"} {
		if strings.Contains(lower, word) {
			return logrus.ErrorLevel
		}
	}
	for _, word := range []string{"warn", "disabled", "禁用", "skip", "stopped", "⚠", "重试"} {
		if strings.Contains(lower, word) {
			return logrus.WarnLevel
		}
	}
	return logrus.InfoLevel
}
