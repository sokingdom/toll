package anthropic

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sokingdom/toll/internal/provider"
)

func TestInspect(t *testing.T) {
	body := `{"model":"claude-haiku-4-5","stream":true,"metadata":{"user_id":"u-42"}}`
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r.Header.Set("X-Api-Key", "sk-ant-xyz9")
	m := Adapter{}.Inspect(r, "/v1/messages", []byte(body))
	if m.Kind != kindMessages || m.Model != "claude-haiku-4-5" || !m.Stream || m.User != "u-42" || m.KeyHint != "…xyz9" {
		t.Fatalf("unexpected meta %+v", m)
	}
	r = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	if m := (Adapter{}).Inspect(r, "/v1/messages/count_tokens", []byte(body)); m.Metered() {
		t.Fatal("count_tokens must not be metered")
	}
}

func TestParseStream(t *testing.T) {
	body := `event: message_start
data: {"type":"message_start","message":{"model":"claude-haiku-4-5-20251001","usage":{"input_tokens":25,"cache_creation_input_tokens":100,"cache_read_input_tokens":200,"output_tokens":1}}}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"message_delta"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":15}}

event: message_stop
data: {"type":"message_stop"}

`
	res, err := Adapter{}.Parse(provider.Meta{Kind: kindMessages, Stream: true}, nil, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	want := provider.Usage{Input: 25, CacheWrite: 100, CacheRead: 200, Output: 15}
	if !res.HasUsage || res.Usage != want || res.Model != "claude-haiku-4-5-20251001" {
		t.Fatalf("got %+v", res)
	}
}

func TestParseNonStream(t *testing.T) {
	body := `{"id":"msg","model":"claude-sonnet-4-5","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":0}}`
	res, err := Adapter{}.Parse(provider.Meta{Kind: kindMessages}, nil, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if !res.HasUsage || res.Usage != (provider.Usage{Input: 10, Output: 3}) || res.Model != "claude-sonnet-4-5" {
		t.Fatalf("got %+v", res)
	}
}
