package server

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/neouks/series/db"
	"github.com/neouks/series/internal/logging"
	"github.com/sirupsen/logrus"
)

// LogLine is a captured backend log entry exposed by the /api/logs endpoints.
type LogLine struct {
	Seq    int64           `json:"seq"`
	DBID   int64           `json:"db_id,omitempty"`
	TS     time.Time       `json:"ts"`
	Level  string          `json:"level"`
	Tag    string          `json:"tag"`
	Text   string          `json:"text"`
	Fields json.RawMessage `json:"fields,omitempty"`
}

func logLineFromRow(row *db.DBLog) LogLine {
	return LogLine{DBID: row.ID, TS: row.CreatedAt.UTC(), Level: row.Level, Tag: row.Tag, Text: row.Text, Fields: row.Fields}
}

// logSinkT receives Logrus entries before terminal formatting. The same event
// feeds the ring, SSE subscribers and the bounded database write queue.
type logSinkT struct {
	mu   sync.Mutex
	ring []LogLine
	cap  int
	seq  int64
	subs map[chan LogLine]struct{}

	dbOnce sync.Once
	dbCh   chan LogLine
}

var logSink = &logSinkT{cap: 3000, subs: map[chan LogLine]struct{}{}}

// StartLogCapture configures logging before the database and HTTP server start.
func StartLogCapture() error {
	return logging.Configure(os.Stderr, os.Getenv("SERIES_LOG_FORMAT"), os.Getenv("SERIES_LOG_LEVEL"), logSink)
}

func (s *logSinkT) Levels() []logrus.Level { return logrus.AllLevels }

func (s *logSinkT) Fire(entry *logrus.Entry) error {
	entry.Time = entry.Time.UTC()
	tag, _ := entry.Data["component"].(string)
	level := entry.Level.String()
	switch entry.Level {
	case logrus.WarnLevel:
		level = "warn"
	case logrus.PanicLevel, logrus.FatalLevel:
		level = "error"
	case logrus.TraceLevel:
		level = "debug"
	}

	line := LogLine{TS: entry.Time, Level: level, Tag: tag, Text: entry.Message}
	if len(entry.Data) > 1 {
		fields := make(logging.Fields, len(entry.Data)-1)
		for key, value := range entry.Data {
			if key == "component" {
				continue
			}
			if err, ok := value.(error); ok {
				value = err.Error()
			}
			fields[key] = value
		}
		line.Fields, _ = json.Marshal(fields)
	}
	s.add(line)
	return nil
}

// SetDB restores recent history before HTTP starts, then persists new entries
// asynchronously. Capturing new messages never waits for the database.
func (s *logSinkT) SetDB(ctx context.Context, pg *db.DB) {
	if pg == nil {
		return
	}
	s.dbOnce.Do(func() {
		readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		rows, err := pg.RecentLogs(readCtx, 100)
		cancel()
		if err != nil && ctx.Err() == nil {
			logging.Component("logsink").WithError(err).Warn("读取日志历史失败")
		}

		ch := make(chan LogLine, 2000)
		s.mu.Lock()
		if len(rows) > 0 {
			restored := make([]LogLine, 0, len(rows)+len(s.ring))
			for _, row := range rows {
				restored = append(restored, logLineFromRow(row))
			}
			s.ring = append(restored, s.ring...)
			if len(s.ring) > s.cap {
				s.ring = s.ring[len(s.ring)-s.cap:]
			}
			// Restored history precedes the startup entries in SSE cursor order.
			for i := range s.ring {
				s.seq++
				s.ring[i].Seq = s.seq
			}
		}
		s.dbCh = ch
		s.mu.Unlock()
		go s.persist(ctx, pg, ch)
	})
}

func (s *logSinkT) persist(ctx context.Context, pg *db.DB, ch <-chan LogLine) {
	defer func() {
		s.mu.Lock()
		s.dbCh = nil
		s.mu.Unlock()
	}()

	var lastError time.Time
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case line := <-ch:
			writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			id, err := pg.InsertLog(writeCtx, db.DBLog{
				CreatedAt: line.TS, Level: line.Level, Tag: line.Tag, Text: line.Text, Fields: line.Fields,
			})
			cancel()
			if err != nil {
				if ctx.Err() == nil && time.Since(lastError) >= time.Minute {
					lastError = time.Now()
					logging.Component("logsink").WithError(err).Error("保存系统日志失败")
				}
				continue
			}
			s.mu.Lock()
			if len(s.ring) > 0 {
				i := line.Seq - s.ring[0].Seq
				if i >= 0 && i < int64(len(s.ring)) {
					s.ring[i].DBID = id
				}
			}
			s.mu.Unlock()
		}
	}
}

func (s *logSinkT) add(line LogLine) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.seq++
	line.Seq = s.seq
	s.ring = append(s.ring, line)
	if len(s.ring) > s.cap {
		s.ring = s.ring[len(s.ring)-s.cap:]
	}

	// Persistence diagnostics must never retry themselves through the same queue.
	if s.dbCh != nil && line.Tag != "logsink" {
		select {
		case s.dbCh <- line:
		default:
		}
	}
	for sub := range s.subs {
		select {
		case sub <- line:
		default:
		}
	}
}

// recent returns ring lines with Seq > since (capped to limit), and the latest seq.
func (s *logSinkT) recent(since int64, limit int) (lines []LogLine, cursor int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cursor = s.seq
	if limit <= 0 || limit > s.cap {
		limit = s.cap
	}
	for _, line := range s.ring {
		if line.Seq > since {
			lines = append(lines, line)
		}
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines, cursor
}

func (s *logSinkT) subscribe() (<-chan LogLine, func()) {
	ch := make(chan LogLine, 256)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subs, ch)
			close(ch)
			s.mu.Unlock()
		})
	}
}
