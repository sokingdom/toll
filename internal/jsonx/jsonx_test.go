package jsonx

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEnsureStreamUsage(t *testing.T) {
	cases := []struct {
		name, in, want string
		modified       bool
	}{
		{
			name:     "absent",
			in:       `{"model":"gpt-4o-mini","stream":true}`,
			want:     `{"model":"gpt-4o-mini","stream":true,"stream_options":{"include_usage":true}}`,
			modified: true,
		},
		{
			name:     "absent keeps formatting",
			in:       "{\n  \"stream\": true,\n  \"messages\": [ {\"role\": \"user\"} ]\n}\n",
			want:     "{\n  \"stream\": true,\n  \"messages\": [ {\"role\": \"user\"} ]\n,\"stream_options\":{\"include_usage\":true}}\n",
			modified: true,
		},
		{
			name:     "empty object",
			in:       `{}`,
			want:     `{"stream_options":{"include_usage":true}}`,
			modified: true,
		},
		{
			name:     "null options",
			in:       `{"stream":true,"stream_options":null,"n":1}`,
			want:     `{"stream":true,"stream_options":{"include_usage":true},"n":1}`,
			modified: true,
		},
		{
			name:     "options without include_usage",
			in:       `{"stream_options": {"x": 1}, "stream":true}`,
			want:     `{"stream_options": {"include_usage":true,"x": 1}, "stream":true}`,
			modified: true,
		},
		{
			name:     "empty options",
			in:       `{"stream_options":{ }}`,
			want:     `{"stream_options":{"include_usage":true }}`,
			modified: true,
		},
		{
			name:     "explicit false",
			in:       `{"stream_options":{"include_usage": false,"y":2}}`,
			want:     `{"stream_options":{"include_usage": true,"y":2}}`,
			modified: true,
		},
		{
			name: "already true",
			in:   `{"stream_options":{"include_usage":true}}`,
			want: `{"stream_options":{"include_usage":true}}`,
		},
		{
			name: "not an object",
			in:   `[1,2]`,
			want: `[1,2]`,
		},
		{
			name: "invalid options type",
			in:   `{"stream_options":"weird"}`,
			want: `{"stream_options":"weird"}`,
		},
		{
			name: "malformed",
			in:   `{"stream":`,
			want: `{"stream":`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, modified := EnsureStreamUsage([]byte(tc.in))
			if string(got) != tc.want || modified != tc.modified {
				t.Fatalf("got %q (modified=%v), want %q (modified=%v)", got, modified, tc.want, tc.modified)
			}
			if modified && !json.Valid(got) {
				t.Fatalf("result is not valid JSON: %s", got)
			}
		})
	}
}

func TestDecodeFields(t *testing.T) {
	big := strings.Repeat(`[0.1,0.2,{"a":[1,2,{"b":"c"}]}],`, 1000)
	in := `{"object":"list","data":[` + big + `[]],"model":"text-embedding-3-small","usage":{"prompt_tokens":8,"total_tokens":8}}`
	var model string
	var usage struct {
		PromptTokens int `json:"prompt_tokens"`
	}
	err := DecodeFields(strings.NewReader(in), map[string]any{"model": &model, "usage": &usage})
	if err != nil {
		t.Fatal(err)
	}
	if model != "text-embedding-3-small" || usage.PromptTokens != 8 {
		t.Fatalf("got model=%q usage=%+v", model, usage)
	}
	if err := DecodeFields(strings.NewReader(`[1]`), nil); err == nil {
		t.Fatal("expected error for non-object")
	}
}
