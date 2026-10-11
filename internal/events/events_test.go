package events

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileWriterRotates(t *testing.T) {
	dir := t.TempDir()
	fw, err := NewFileWriter(dir, "events-{instance}-{time}.jsonl", "i1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 12, 1, 59, 0, 0, time.UTC)
	fw.now = func() time.Time { return now }
	if err := fw.Write(Started{Common{Type: TypeStarted, RequestID: "a"}}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if err := fw.Write(Started{Common{Type: TypeStarted, RequestID: "b"}}); err != nil {
		t.Fatal(err)
	}
	fw.Close()

	for name, id := range map[string]string{
		"events-i1-2026-10-12T01.jsonl": "a",
		"events-i1-2026-10-12T02.jsonl": "b",
	} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var ev Started
		if err := json.Unmarshal(b, &ev); err != nil || ev.RequestID != id {
			t.Fatalf("%s: %s (%v)", name, b, err)
		}
	}
}

func TestFileWriterSubHourLayout(t *testing.T) {
	fw := &FileWriter{filename: "e-{time}.jsonl", rotate: 15 * time.Minute}
	got := fw.name(time.Date(2026, 10, 12, 1, 45, 0, 0, time.UTC))
	if got != "e-2026-10-12T0145.jsonl" {
		t.Fatal(got)
	}
}

func TestCompletedNullFields(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf)
	if err := sw.Write(Completed{Common: Common{Type: TypeCompleted}, UsageSource: UsageMissing}); err != nil {
		t.Fatal(err)
	}
	line := buf.String()
	for _, want := range []string{`"user":null`, `"input_tokens":null`, `"model":null`, `"ttfb_ms":null`} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %s in %s", want, line)
		}
	}
	if !strings.HasSuffix(line, "}\n") || strings.Contains(line, `"error"`) {
		t.Fatalf("unexpected line %s", line)
	}
}
