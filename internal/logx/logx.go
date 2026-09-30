// Package logx is a tiny logger that also keeps recent lines for the control panel.
package logx

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// Entry is one log line.
type Entry struct {
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
}

// Logger writes to an io.Writer and remembers the most recent entries.
type Logger struct {
	mu      sync.Mutex
	out     io.Writer
	max     int
	entries []Entry
	verbose bool
}

// New returns a logger that keeps up to max recent entries.
func New(out io.Writer, max int) *Logger {
	if max <= 0 {
		max = 200
	}
	return &Logger{out: out, max: max}
}

// Discard returns a logger that prints nothing.
func Discard() *Logger { return New(io.Discard, 50) }

// SetVerbose enables debug lines.
func (l *Logger) SetVerbose(v bool) {
	l.mu.Lock()
	l.verbose = v
	l.mu.Unlock()
}

func (l *Logger) logf(level, format string, args ...any) {
	if l == nil {
		return
	}
	e := Entry{Time: time.Now(), Level: level, Msg: fmt.Sprintf(format, args...)}
	l.mu.Lock()
	defer l.mu.Unlock()
	if level == "debug" && !l.verbose {
		return
	}
	l.entries = append(l.entries, e)
	if len(l.entries) > 2*l.max {
		l.entries = append([]Entry(nil), l.entries[len(l.entries)-l.max:]...)
	}
	if l.out != nil {
		fmt.Fprintf(l.out, "%s %-5s %s\n", e.Time.Format("15:04:05"), level, e.Msg)
	}
}

func (l *Logger) Debugf(format string, args ...any) { l.logf("debug", format, args...) }
func (l *Logger) Infof(format string, args ...any)  { l.logf("info", format, args...) }
func (l *Logger) Warnf(format string, args ...any)  { l.logf("warn", format, args...) }
func (l *Logger) Errorf(format string, args ...any) { l.logf("error", format, args...) }

// Recent returns up to n of the newest entries, oldest first.
func (l *Logger) Recent(n int) []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n <= 0 || n > len(l.entries) {
		n = len(l.entries)
	}
	if n > l.max {
		n = l.max
	}
	out := make([]Entry, n)
	copy(out, l.entries[len(l.entries)-n:])
	return out
}
