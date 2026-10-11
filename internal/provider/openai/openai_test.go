package openai

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sokingdom/toll/internal/provider"
)

func inspect(t *testing.T, endpoint, body string) provider.Meta {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer sk-test-abcd1234")
	return Adapter{}.Inspect(r, endpoint, []byte(body))
}

func TestInspect(t *testing.T) {
	m := inspect(t, "/v1/chat/completions", `{"model":"gpt-4o-mini","stream":true,"user":"u1","safety_identifier":"s1"}`)
	if m.Kind != kindChat || m.Model != "gpt-4o-mini" || !m.Stream || m.User != "s1" || m.KeyHint != "…1234" {
		t.Fatalf("unexpected meta %+v", m)
	}
	m = inspect(t, "/v1/responses", `{"model":"gpt-5","user":"u1"}`)
	if m.Kind != kindResponses || m.User != "u1" {
		t.Fatalf("unexpected meta %+v", m)
	}
	if m := inspect(t, "/v1/files", `{}`); m.Metered() {
		t.Fatalf("files endpoint must not be metered: %+v", m)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	if m := (Adapter{}).Inspect(r, "/v1/models", nil); m.Metered() {
		t.Fatal("GET must not be metered")
	}
}

func TestPrepare(t *testing.T) {
	m := inspect(t, "/v1/chat/completions", `{"stream":true}`)
	out, modified := Adapter{}.Prepare(m, []byte(`{"stream":true}`))
	if !modified || string(out) != `{"stream":true,"stream_options":{"include_usage":true}}` {
		t.Fatalf("got %s %v", out, modified)
	}
	m = inspect(t, "/v1/chat/completions", `{"stream":false}`)
	if _, modified := (Adapter{}).Prepare(m, []byte(`{"stream":false}`)); modified {
		t.Fatal("non-streaming request must not be modified")
	}
	m = inspect(t, "/v1/responses", `{"stream":true}`)
	if _, modified := (Adapter{}).Prepare(m, []byte(`{"stream":true}`)); modified {
		t.Fatal("responses request must not be modified")
	}
}

func parse(t *testing.T, m provider.Meta, body string) provider.Result {
	t.Helper()
	res, err := Adapter{}.Parse(m, nil, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestParseChatStream(t *testing.T) {
	body := `data: {"id":"1","model":"gpt-4o-mini-2024-07-18","choices":[{"delta":{"content":"hi"}}],"usage":null}

data: {"id":"1","model":"gpt-4o-mini-2024-07-18","choices":[],"usage":{"prompt_tokens":812,"completion_tokens":240,"prompt_tokens_details":{"cached_tokens":512},"completion_tokens_details":{"reasoning_tokens":30}}}

data: [DONE]

`
	res := parse(t, provider.Meta{Kind: kindChat, Stream: true}, body)
	want := provider.Usage{Input: 300, CacheRead: 512, Output: 240, Reasoning: 30}
	if !res.HasUsage || res.Usage != want || res.Model != "gpt-4o-mini-2024-07-18" {
		t.Fatalf("got %+v", res)
	}
}

func TestParseChatStreamWithoutUsage(t *testing.T) {
	body := "data: {\"model\":\"m\",\"choices\":[{\"delta\":{}}]}\n\ndata: [DONE]\n\n"
	if res := parse(t, provider.Meta{Kind: kindChat, Stream: true}, body); res.HasUsage {
		t.Fatalf("unexpected usage %+v", res)
	}
}

func TestParseChatNonStream(t *testing.T) {
	body := `{"id":"x","model":"gpt-4o","choices":[{"message":{"content":"usage"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
	res := parse(t, provider.Meta{Kind: kindChat}, body)
	if !res.HasUsage || res.Usage != (provider.Usage{Input: 10, Output: 5}) || res.Model != "gpt-4o" {
		t.Fatalf("got %+v", res)
	}
}

func TestParseEmbeddings(t *testing.T) {
	body := `{"object":"list","data":[{"embedding":[0.1,0.2]}],"model":"text-embedding-3-small","usage":{"prompt_tokens":8,"total_tokens":8}}`
	res := parse(t, provider.Meta{Kind: kindEmbeddings}, body)
	if !res.HasUsage || res.Usage != (provider.Usage{Input: 8}) {
		t.Fatalf("got %+v", res)
	}
}

func TestParseResponsesStream(t *testing.T) {
	body := `event: response.created
data: {"type":"response.created","response":{"model":"gpt-5-2025-08-07","usage":null}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"\"response\""}

event: response.completed
data: {"type":"response.completed","response":{"model":"gpt-5-2025-08-07","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40},"output_tokens":50,"output_tokens_details":{"reasoning_tokens":20}}}}

`
	res := parse(t, provider.Meta{Kind: kindResponses, Stream: true}, body)
	want := provider.Usage{Input: 60, CacheRead: 40, Output: 50, Reasoning: 20}
	if !res.HasUsage || res.Usage != want || res.Model != "gpt-5-2025-08-07" {
		t.Fatalf("got %+v", res)
	}
}

func TestParseResponsesNonStream(t *testing.T) {
	body := `{"id":"r","model":"gpt-5","output":[],"usage":{"input_tokens":7,"output_tokens":3}}`
	res := parse(t, provider.Meta{Kind: kindResponses}, body)
	if !res.HasUsage || res.Usage != (provider.Usage{Input: 7, Output: 3}) {
		t.Fatalf("got %+v", res)
	}
}
