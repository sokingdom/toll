package gemini

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sokingdom/toll/internal/provider"
)

func TestInspect(t *testing.T) {
	ep := "/v1beta/models/gemini-2.5-flash:streamGenerateContent"
	r := httptest.NewRequest(http.MethodPost, ep+"?alt=sse&key=AIzaQ1w2", nil)
	m := Adapter{}.Inspect(r, ep, nil)
	if m.Kind != kindGenerate || m.Model != "gemini-2.5-flash" || !m.Stream || m.KeyHint != "…Q1w2" {
		t.Fatalf("unexpected meta %+v", m)
	}
	ep = "/v1beta/models/gemini-2.5-flash:countTokens"
	if m := (Adapter{}).Inspect(httptest.NewRequest(http.MethodPost, ep, nil), ep, nil); m.Metered() {
		t.Fatal("countTokens must not be metered")
	}
}

const usageJSON = `"usageMetadata":{"promptTokenCount":120,"candidatesTokenCount":30,"cachedContentTokenCount":20,"thoughtsTokenCount":10,"toolUsePromptTokenCount":5}`

var wantUsage = provider.Usage{Input: 105, CacheRead: 20, Output: 40, Reasoning: 10}

func respFor(rawURL string) *http.Response {
	return &http.Response{Request: httptest.NewRequest(http.MethodPost, rawURL, nil)}
}

func TestParseSSE(t *testing.T) {
	body := `data: {"candidates":[],"usageMetadata":{"promptTokenCount":120},"modelVersion":"gemini-2.5-flash"}

data: {"candidates":[],` + usageJSON + `,"modelVersion":"gemini-2.5-flash"}

`
	res, err := Adapter{}.Parse(provider.Meta{Kind: kindGenerate, Stream: true}, respFor("/x?alt=sse"), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if !res.HasUsage || res.Usage != wantUsage || res.Model != "gemini-2.5-flash" {
		t.Fatalf("got %+v", res)
	}
}

func TestParseJSONArrayStream(t *testing.T) {
	body := `[{"candidates":[]},
{"candidates":[],` + usageJSON + `,"modelVersion":"gemini-2.5-pro"}]`
	res, err := Adapter{}.Parse(provider.Meta{Kind: kindGenerate, Stream: true}, respFor("/x"), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if !res.HasUsage || res.Usage != wantUsage || res.Model != "gemini-2.5-pro" {
		t.Fatalf("got %+v", res)
	}
}

func TestParseNonStream(t *testing.T) {
	body := `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}],` + usageJSON + `,"modelVersion":"gemini-2.5-flash"}`
	res, err := Adapter{}.Parse(provider.Meta{Kind: kindGenerate}, respFor("/x"), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if !res.HasUsage || res.Usage != wantUsage {
		t.Fatalf("got %+v", res)
	}
}
