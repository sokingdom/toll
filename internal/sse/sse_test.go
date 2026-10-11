package sse

import (
	"strings"
	"testing"
)

func TestRead(t *testing.T) {
	in := "event: a\r\ndata: one\r\n\r\n: comment\ndata:two\ndata: three\n\ndata: [DONE]"
	var got []string
	if err := Read(strings.NewReader(in), func(d []byte) { got = append(got, string(d)) }); err != nil {
		t.Fatal(err)
	}
	want := []string{"one", "two\nthree", "[DONE]"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestReadSkipsOversizedEvents(t *testing.T) {
	in := "data: " + strings.Repeat("x", MaxEventSize+10) + "\n\ndata: ok\n\n"
	var got []string
	if err := Read(strings.NewReader(in), func(d []byte) { got = append(got, string(d)) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "ok" {
		t.Fatalf("got %d events, first=%.10q", len(got), got)
	}
}
