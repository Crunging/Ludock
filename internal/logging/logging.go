// Package logging writes Ludock's redacted console log lines.
package logging

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

type Level int

const (
	LevelError Level = iota
	LevelWarn
	LevelInfo
	LevelDebug
)

var levelNames = [...]string{"error", "warn", "info", "debug"}

func (l Level) String() string { return levelNames[l] }

// ParseLevel reads LOG_LEVEL. An unsupported value falls back to info and is
// returned so startup can warn about it.
func ParseLevel(configured string) (Level, string) {
	value := strings.ToLower(strings.TrimSpace(configured))
	if value == "" {
		return LevelInfo, ""
	}
	for index, name := range levelNames {
		if name == value {
			return Level(index), ""
		}
	}
	return LevelInfo, configured
}

const (
	maxMessage      = 16384
	maxContextValue = 4096
)

// Field is one ordered context value. Values must be strings, numbers, bools,
// or nil.
type Field struct {
	Key   string
	Value any
}

func F(key string, value any) Field { return Field{key, value} }

type sink struct {
	mu     sync.Mutex
	level  Level
	stdout io.Writer
	stderr io.Writer
	now    func() time.Time
}

var std = newSink()

func newSink() *sink {
	return &sink{level: LevelInfo, stdout: os.Stdout, stderr: os.Stderr, now: time.Now}
}

func SetLevel(level Level) {
	std.mu.Lock()
	std.level = level
	std.mu.Unlock()
}

func CurrentLevel() Level {
	std.mu.Lock()
	defer std.mu.Unlock()
	return std.level
}

// SetOutput redirects console output, for tests.
func SetOutput(stdout, stderr io.Writer) {
	std.mu.Lock()
	std.stdout, std.stderr = stdout, stderr
	std.mu.Unlock()
}

type Logger struct{ component string }

func New(component string) *Logger { return &Logger{component} }

func (l *Logger) Error(message string, fields ...Field) {
	std.write(LevelError, l.component, message, fields)
}
func (l *Logger) Warn(message string, fields ...Field) {
	std.write(LevelWarn, l.component, message, fields)
}
func (l *Logger) Info(message string, fields ...Field) {
	std.write(LevelInfo, l.component, message, fields)
}
func (l *Logger) Debug(message string, fields ...Field) {
	std.write(LevelDebug, l.component, message, fields)
}

// Sanitize redacts and bounds a record the way the console line does.
func Sanitize(message string, fields []Field) (string, []Field) {
	redacted := Redact(message)
	if cut, truncated := truncate(redacted, maxMessage); truncated {
		redacted = cut + "… [truncated]"
	}
	var context []Field
	for _, field := range fields {
		value := field.Value
		if IsSensitiveKey(field.Key) {
			value = "[REDACTED]"
		} else if text, ok := value.(string); ok {
			value, _ = truncate(Redact(text), maxContextValue)
		}
		context = append(context, Field{field.Key, value})
	}
	return redacted, context
}

func (s *sink) write(level Level, component, message string, fields []Field) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if level > s.level {
		return
	}
	now := s.now()
	sanitized, context := Sanitize(message, fields)
	line := fmt.Sprintf("%s %s [%s] %s", now.UTC().Format("2006-01-02T15:04:05.000Z"),
		strings.ToUpper(level.String()), component, sanitized)
	if len(context) > 0 {
		line += " " + string(EncodeFields(context))
	}
	out := s.stdout
	if level <= LevelWarn {
		out = s.stderr
	}
	fmt.Fprintln(out, line)
}

func EncodeFields(fields []Field) []byte {
	var b strings.Builder
	b.WriteByte('{')
	for index, field := range fields {
		if index > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(field.Key)
		value, err := json.Marshal(field.Value)
		if err != nil {
			value = []byte("null")
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(value)
	}
	b.WriteByte('}')
	return []byte(b.String())
}
