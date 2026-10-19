// Package log implements a tiny leveled logger. All diagnostics go to the
// configured writer (stderr in the CLI); the serial data path never logs.
package log

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Level is the severity of a log message.
type Level int

const (
	LevelError Level = iota
	LevelWarn
	LevelInfo
	LevelDebug
	LevelTrace
)

// String implements fmt.Stringer.
func (l Level) String() string {
	switch l {
	case LevelError:
		return "error"
	case LevelWarn:
		return "warn"
	case LevelInfo:
		return "info"
	case LevelDebug:
		return "debug"
	case LevelTrace:
		return "trace"
	default:
		return "unknown"
	}
}

// ParseLevel converts a textual level (error/warn/info/debug/trace) into a
// Level. It accepts uppercase input and the common "warning" spelling.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error", "err":
		return LevelError, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "info":
		return LevelInfo, nil
	case "debug":
		return LevelDebug, nil
	case "trace":
		return LevelTrace, nil
	default:
		return LevelInfo, fmt.Errorf("未知日志级别 %q（可选：error, warn, info, debug, trace）", s)
	}
}

// Logger is a concurrency-safe leveled logger.
type Logger struct {
	mu    sync.Mutex
	w     io.Writer
	level Level
	now   func() time.Time
}

// New returns a logger writing to w at the given minimum level.
// A nil writer is treated as io.Discard.
func New(w io.Writer, level Level) *Logger {
	if w == nil {
		w = io.Discard
	}
	return &Logger{w: w, level: level, now: time.Now}
}

// Discard returns a logger that drops every message.
func Discard() *Logger { return New(nil, LevelError) }

// Level reports the minimum enabled level.
func (l *Logger) Level() Level {
	if l == nil {
		return LevelError
	}
	return l.level
}

// Enabled reports whether level would be emitted.
func (l *Logger) Enabled(level Level) bool {
	return l != nil && level <= l.level
}

func (l *Logger) logf(level Level, format string, args ...any) {
	if l == nil || level > l.level {
		return
	}
	msg := fmt.Sprintf(format, args...)
	ts := l.now().Format("15:04:05.000")
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.w, "%s [%s] %s", ts, level, msg)
}

func (l *Logger) Errorf(format string, args ...any) { l.logf(LevelError, format, args...) }
func (l *Logger) Warnf(format string, args ...any)  { l.logf(LevelWarn, format, args...) }
func (l *Logger) Infof(format string, args ...any)  { l.logf(LevelInfo, format, args...) }
func (l *Logger) Debugf(format string, args ...any) { l.logf(LevelDebug, format, args...) }
func (l *Logger) Tracef(format string, args ...any) { l.logf(LevelTrace, format, args...) }
