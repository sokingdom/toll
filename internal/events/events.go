// Package events defines toll's usage events and writes them as JSON lines.
//
// Events are written synchronously as they occur, with no in-memory queue:
// once Write returns nil the event has been handed to the operating system.
package events

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	TypeStarted   = "request.started"
	TypeCompleted = "request.completed"
)

// Usage sources recorded in completed events.
const (
	UsageProvider = "provider" // usage reported by the provider
	UsageMissing  = "missing"  // usage expected but not received
	UsageNone     = "none"     // usage not expected (unmetered endpoint, error response, rejection)
)

// Common holds the fields shared by both event types.
type Common struct {
	Type           string            `json:"type"`
	RequestID      string            `json:"request_id"`
	TS             string            `json:"ts"`
	Instance       string            `json:"instance"`
	User           *string           `json:"user"`
	UserSource     string            `json:"user_source"`
	KeyID          *string           `json:"key_id"`
	KeyHint        *string           `json:"key_hint"`
	KeyAlias       *string           `json:"key_alias"`
	Provider       string            `json:"provider"`
	Endpoint       string            `json:"endpoint"`
	RequestedModel *string           `json:"requested_model"`
	Tags           map[string]string `json:"tags"`
	Stream         bool              `json:"stream"`
	UsageInjected  bool              `json:"usage_injected"`
}

// Started is written just before the request is sent upstream.
type Started struct {
	Common
}

// Completed is written when the response ends, whatever the outcome. It
// repeats the common fields so that it can be aggregated on its own.
type Completed struct {
	Common
	Model            *string `json:"model"`
	Status           int     `json:"status"`
	Rejected         bool    `json:"rejected"`
	Error            string  `json:"error,omitempty"`
	Partial          bool    `json:"partial"`
	UsageSource      string  `json:"usage_source"`
	InputTokens      *int64  `json:"input_tokens"`
	CacheReadTokens  *int64  `json:"cache_read_tokens"`
	CacheWriteTokens *int64  `json:"cache_write_tokens"`
	OutputTokens     *int64  `json:"output_tokens"`
	ReasoningTokens  *int64  `json:"reasoning_tokens"`
	LatencyMS        int64   `json:"latency_ms"`
	TTFBMS           *int64  `json:"ttfb_ms"`
}

// Timestamp formats t the way events record time.
func Timestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// Writer persists events.
type Writer interface {
	Write(event any) error
	Close() error
}

// StreamWriter writes events to an io.Writer such as stdout.
type StreamWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func NewStreamWriter(w io.Writer) *StreamWriter { return &StreamWriter{w: w} }

func (s *StreamWriter) Write(event any) error {
	line, err := marshalLine(event)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.w.Write(line)
	return err
}

func (s *StreamWriter) Close() error { return nil }

// FileWriter appends events to time-rotated files in a directory.
type FileWriter struct {
	dir      string
	filename string // template with {instance} and {time}
	instance string
	rotate   time.Duration
	now      func() time.Time

	mu     sync.Mutex
	f      *os.File
	period time.Time
}

// NewFileWriter creates dir if needed and returns a writer that starts a new
// file every rotate period. Old files are never deleted by toll.
func NewFileWriter(dir, filename, instance string, rotate time.Duration) (*FileWriter, error) {
	if rotate <= 0 {
		return nil, fmt.Errorf("events: rotate must be positive")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &FileWriter{dir: dir, filename: filename, instance: instance, rotate: rotate, now: time.Now}, nil
}

func (fw *FileWriter) Write(event any) error {
	line, err := marshalLine(event)
	if err != nil {
		return err
	}
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if err := fw.ensureFile(); err != nil {
		return err
	}
	if _, err := fw.f.Write(line); err != nil {
		// Reopen on the next write in case the file was removed or the
		// error was transient.
		fw.f.Close()
		fw.f = nil
		return err
	}
	return nil
}

func (fw *FileWriter) ensureFile() error {
	period := fw.now().UTC().Truncate(fw.rotate)
	if fw.f != nil && period.Equal(fw.period) {
		return nil
	}
	if fw.f != nil {
		fw.f.Close()
		fw.f = nil
	}
	path := filepath.Join(fw.dir, fw.name(period))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	fw.f, fw.period = f, period
	return nil
}

func (fw *FileWriter) name(period time.Time) string {
	layout := "2006-01-02T15"
	if fw.rotate%time.Hour != 0 {
		layout = "2006-01-02T1504"
	}
	return strings.NewReplacer("{instance}", fw.instance, "{time}", period.Format(layout)).Replace(fw.filename)
}

func (fw *FileWriter) Close() error {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if fw.f == nil {
		return nil
	}
	err := fw.f.Close()
	fw.f = nil
	return err
}

func marshalLine(event any) ([]byte, error) {
	b, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
