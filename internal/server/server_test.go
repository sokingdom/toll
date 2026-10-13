package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sokingdom/toll/internal/config"
	"github.com/sokingdom/toll/internal/events"
	"github.com/sokingdom/toll/internal/metrics"
)

// memWriter records events and can be told to fail.
type memWriter struct {
	mu     sync.Mutex
	events []map[string]any
	fail   bool
}

func (m *memWriter) Write(ev any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("disk full")
	}
	b, _ := json.Marshal(ev)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	m.events = append(m.events, out)
	return nil
}

func (m *memWriter) Close() error { return nil }

func (m *memWriter) all() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]map[string]any(nil), m.events...)
}

type upstreamCall struct {
	path, query string
	header      http.Header
	body        []byte
}

// memRecorder counts what the dashboard recorder receives.
type memRecorder struct {
	mu        sync.Mutex
	started   []string
	completed []events.Completed
}

func (m *memRecorder) Started(ev events.Started, _ time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = append(m.started, ev.RequestID)
}

func (m *memRecorder) Completed(ev events.Completed, _ time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.completed = append(m.completed, ev)
}

type harness struct {
	toll     *httptest.Server
	events   *memWriter
	recorder *memRecorder
	metrics  *metrics.Metrics
	calls    chan upstreamCall
	upstream *httptest.Server
}

func newHarness(t *testing.T, yaml string, handler http.HandlerFunc) *harness {
	t.Helper()
	h := &harness{events: &memWriter{}, recorder: &memRecorder{}, calls: make(chan upstreamCall, 10)}
	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.calls <- upstreamCall{path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone(), body: body}
		r.Body = io.NopCloser(bytes.NewReader(body))
		handler(w, r)
	}))
	t.Cleanup(h.upstream.Close)

	yaml = strings.ReplaceAll(yaml, "UPSTREAM", h.upstream.URL)
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Server.InstanceID = "test-1"
	h.metrics = metrics.New(cfg.Metering.MetricTags)
	srv, err := New(Options{
		Config:   cfg,
		Events:   h.events,
		Metrics:  h.metrics,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Recorder: h.recorder,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.toll = httptest.NewServer(srv)
	t.Cleanup(h.toll.Close)
	return h
}

func (h *harness) post(t *testing.T, path, body string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.toll.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// waitEvents waits for n events; completed events are written after the
// response body has been fully sent.
func (h *harness) waitEvents(t *testing.T, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if evs := h.events.all(); len(evs) >= n {
			return evs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected %d events, got %v", n, h.events.all())
	return nil
}

const openaiStream = "data: {\"model\":\"gpt-4o-mini-2024-07-18\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"model\":\"gpt-4o-mini-2024-07-18\",\"choices\":[],\"usage\":{\"prompt_tokens\":812,\"completion_tokens\":240,\"prompt_tokens_details\":{\"cached_tokens\":12}}}\n\n" +
	"data: [DONE]\n\n"

func TestOpenAIStreamingEndToEnd(t *testing.T) {
	h := newHarness(t, "providers: { openai: { upstream: UPSTREAM } }\nkey_aliases: {}\n", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, openaiStream)
	})
	reqBody := `{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, body := h.post(t, "/openai/v1/chat/completions", reqBody, map[string]string{
		"Authorization": "Bearer sk-test-1234abcd",
		"X-Toll-User":   "user_8f3a21",
		"X-Toll-Tags":   "feature=chat, platform=ios",
	})
	if resp.StatusCode != 200 || body != openaiStream {
		t.Fatalf("response changed: %d %q", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Toll-Request-Id") == "" {
		t.Fatal("missing X-Toll-Request-Id")
	}

	call := <-h.calls
	if call.path != "/v1/chat/completions" {
		t.Fatalf("upstream path %q", call.path)
	}
	want := `{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"hi"}],"stream_options":{"include_usage":true}}`
	if string(call.body) != want {
		t.Fatalf("upstream body %s", call.body)
	}
	if call.header.Get("X-Toll-User") != "" || call.header.Get("X-Toll-Tags") != "" {
		t.Fatal("X-Toll headers leaked upstream")
	}
	if call.header.Get("Authorization") != "Bearer sk-test-1234abcd" {
		t.Fatal("provider key not passed through")
	}

	evs := h.waitEvents(t, 2)
	started, completed := evs[0], evs[1]
	if started["type"] != events.TypeStarted || completed["type"] != events.TypeCompleted {
		t.Fatalf("event types %v %v", started["type"], completed["type"])
	}
	if started["request_id"] != completed["request_id"] || started["request_id"] != resp.Header.Get("X-Toll-Request-Id") {
		t.Fatal("request ids do not match")
	}
	checks := map[string]any{
		"user": "user_8f3a21", "user_source": "header", "provider": "openai",
		"endpoint": "/v1/chat/completions", "requested_model": "gpt-4o-mini",
		"model": "gpt-4o-mini-2024-07-18", "usage_injected": true, "usage_source": "provider",
		"input_tokens": 800.0, "cache_read_tokens": 12.0, "output_tokens": 240.0,
		"status": 200.0, "partial": false, "rejected": false, "key_hint": "…abcd", "instance": "test-1",
	}
	for k, v := range checks {
		if completed[k] != v {
			t.Errorf("completed[%s] = %v, want %v", k, completed[k], v)
		}
	}
	if tags, _ := completed["tags"].(map[string]any); tags["feature"] != "chat" || tags["platform"] != "ios" {
		t.Errorf("tags %v", completed["tags"])
	}
}

func TestAnthropicBodyUserAndGzip(t *testing.T) {
	h := newHarness(t, "providers: { anthropic: { upstream: UPSTREAM } }", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") || strings.Contains(r.Header.Get("Accept-Encoding"), "br") {
			t.Errorf("accept-encoding not filtered: %q", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = io.WriteString(gz, `{"model":"claude-haiku-4-5","usage":{"input_tokens":10,"output_tokens":3,"cache_creation_input_tokens":4}}`)
		_ = gz.Close()
	})
	req, _ := http.NewRequest(http.MethodPost, h.toll.URL+"/anthropic/v1/messages",
		strings.NewReader(`{"model":"claude-haiku-4-5","metadata":{"user_id":"u-body"}}`))
	req.Header.Set("X-Api-Key", "sk-ant-zzzz")
	req.Header.Set("Accept-Encoding", "br, gzip")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("response encoding changed")
	}
	c := h.waitEvents(t, 2)[1]
	if c["user"] != "u-body" || c["user_source"] != "body" || c["input_tokens"] != 10.0 || c["cache_write_tokens"] != 4.0 || c["usage_injected"] != false {
		t.Fatalf("completed %v", c)
	}
}

func TestGeminiQueryKeyAndSSE(t *testing.T) {
	h := newHarness(t, "providers: { gemini: { upstream: UPSTREAM } }", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"usageMetadata\":{\"promptTokenCount\":9,\"candidatesTokenCount\":2},\"modelVersion\":\"gemini-2.5-flash\"}\n\n")
	})
	h.post(t, "/gemini/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse&key=AIzaKEY9", `{"contents":[]}`, map[string]string{"X-Toll-User": "u1"})
	call := <-h.calls
	if call.path != "/v1beta/models/gemini-2.5-flash:streamGenerateContent" || !strings.Contains(call.query, "key=AIzaKEY9") {
		t.Fatalf("upstream call %+v", call)
	}
	c := h.waitEvents(t, 2)[1]
	if c["input_tokens"] != 9.0 || c["output_tokens"] != 2.0 || c["key_hint"] != "…KEY9" || c["requested_model"] != "gemini-2.5-flash" {
		t.Fatalf("completed %v", c)
	}
	if strings.Contains(c["endpoint"].(string), "key=") {
		t.Fatal("api key leaked into endpoint")
	}
}

func TestUnmeteredAndErrorResponses(t *testing.T) {
	h := newHarness(t, "providers: { openai: { upstream: UPSTREAM } }", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[]}`)
	})
	resp, _ := h.post(t, "/openai/v1/chat/completions", `{"model":"gpt-4o","stream":true}`, nil)
	if resp.StatusCode != 429 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	c := h.waitEvents(t, 2)[1]
	if c["status"] != 429.0 || c["usage_source"] != events.UsageNone || c["input_tokens"] != nil {
		t.Fatalf("completed %v", c)
	}
	h.post(t, "/openai/v1/files", `{}`, nil)
	c = h.waitEvents(t, 4)[3]
	if c["usage_source"] != events.UsageNone || c["status"] != 200.0 {
		t.Fatalf("completed %v", c)
	}
}

func TestUsageMissingWhenInjectionDisabled(t *testing.T) {
	h := newHarness(t, "providers: { openai: { upstream: UPSTREAM } }\nmetering: { inject_stream_usage: false }", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "data: {\"model\":\"m\",\"choices\":[]}\n\ndata: [DONE]\n\n")
	})
	h.post(t, "/openai/v1/chat/completions", `{"stream":true}`, nil)
	if call := <-h.calls; string(call.body) != `{"stream":true}` {
		t.Fatalf("body modified: %s", call.body)
	}
	c := h.waitEvents(t, 2)[1]
	if c["usage_source"] != events.UsageMissing || c["usage_injected"] != false {
		t.Fatalf("completed %v", c)
	}
}

func TestRejections(t *testing.T) {
	h := newHarness(t, `
server: { auth_token: s3cret, max_request_body: 64 }
providers: { openai: { upstream: UPSTREAM } }
metering: { require_user: true }
`, func(w http.ResponseWriter, r *http.Request) { t.Error("upstream must not be called") })

	resp, body := h.post(t, "/openai/v1/chat/completions", `{}`, nil)
	if resp.StatusCode != 401 || !strings.Contains(body, "unauthorized") {
		t.Fatalf("auth: %d %s", resp.StatusCode, body)
	}
	auth := map[string]string{"X-Toll-Auth": "s3cret"}
	if resp, _ := h.post(t, "/nope/v1", `{}`, auth); resp.StatusCode != 404 {
		t.Fatalf("route: %d", resp.StatusCode)
	}
	if resp, body := h.post(t, "/openai/v1/chat/completions", `{}`, auth); resp.StatusCode != 400 || !strings.Contains(body, "user_required") {
		t.Fatalf("require_user: %d %s", resp.StatusCode, body)
	}
	if resp, _ := h.post(t, "/openai/v1/chat/completions", strings.Repeat("x", 100), auth); resp.StatusCode != 413 {
		t.Fatalf("size: %d", resp.StatusCode)
	}
	evs := h.waitEvents(t, 2)
	if len(evs) != 2 {
		t.Fatalf("401/404 must not produce events: %v", evs)
	}
	for _, ev := range evs {
		if ev["type"] != events.TypeCompleted || ev["rejected"] != true {
			t.Fatalf("rejection event %v", ev)
		}
	}
	if evs[0]["error"] != "user_required" || evs[1]["error"] != "body_too_large" {
		t.Fatalf("errors %v %v", evs[0]["error"], evs[1]["error"])
	}
}

func TestWriteErrorPolicy(t *testing.T) {
	for _, tc := range []struct {
		policy string
		status int
	}{{"fail_open", 200}, {"fail_closed", 503}} {
		t.Run(tc.policy, func(t *testing.T) {
			h := newHarness(t, "providers: { openai: { upstream: UPSTREAM } }\nevents: { on_write_error: "+tc.policy+" }",
				func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{}`) })
			h.events.fail = true
			if resp, _ := h.post(t, "/openai/v1/chat/completions", `{}`, nil); resp.StatusCode != tc.status {
				t.Fatalf("status %d, want %d", resp.StatusCode, tc.status)
			}
		})
	}
}

func TestClientDisconnectIsPartial(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, "providers: { anthropic: { upstream: UPSTREAM } }", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude\",\"usage\":{\"input_tokens\":50,\"output_tokens\":1}}}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.toll.URL+"/anthropic/v1/messages", strings.NewReader(`{"model":"claude","stream":true}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	_, _ = resp.Body.Read(buf)
	cancel()
	resp.Body.Close()

	c := h.waitEvents(t, 2)[1]
	if c["partial"] != true || c["usage_source"] != events.UsageProvider || c["input_tokens"] != 50.0 {
		t.Fatalf("completed %v", c)
	}
}

func TestUpstreamUnreachable(t *testing.T) {
	h := newHarness(t, "providers: { openai: { upstream: http://127.0.0.1:1 } }", nil)
	resp, body := h.post(t, "/openai/v1/chat/completions", `{}`, nil)
	if resp.StatusCode != 502 || !strings.Contains(body, "upstream_unreachable") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	c := h.waitEvents(t, 2)[1]
	if c["status"] != 502.0 || c["partial"] != false || c["rejected"] != false {
		t.Fatalf("completed %v", c)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	h := newHarness(t, "providers: { openai: { upstream: UPSTREAM } }\nmetering: { metric_tags: [feature] }\nkey_aliases: { k_placeholder: x }", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"model":"gpt-4o","usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	})
	h.post(t, "/openai/v1/chat/completions", `{"model":"gpt-4o"}`, map[string]string{"Authorization": "Bearer sk-abc", "X-Toll-Tags": "feature=summary"})
	h.waitEvents(t, 2)
	rec := httptest.NewRecorder()
	h.metrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	text := rec.Body.String()
	for _, want := range []string{
		`toll_tokens_total{key="k_`,
		`model="gpt-4o",provider="openai",tag_feature="summary",type="input"} 5`,
		`toll_requests_total{`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	if strings.Contains(text, "user") && strings.Contains(text, `user="`) {
		t.Error("user must never be a metric label")
	}
}

func TestMetricsSummary(t *testing.T) {
	h := newHarness(t, "providers: { openai: { upstream: UPSTREAM } }", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"model":"gpt-4o","usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	})
	h.post(t, "/openai/v1/chat/completions", `{"model":"gpt-4o"}`, nil)
	h.post(t, "/openai/v1/chat/completions", `{"model":"gpt-4o"}`, nil)
	h.waitEvents(t, 4)
	s, err := h.metrics.Summary()
	if err != nil || len(s.Rows) != 1 {
		t.Fatalf("summary %+v, %v", s, err)
	}
	r := s.Rows[0]
	if r.Requests != 2 || r.Errors != 0 || r.Tokens["input"] != 10 || r.Tokens["output"] != 4 || s.Status["200"] != 2 {
		t.Fatalf("row %+v status %v", r, s.Status)
	}
}

func TestRecorderGetsEveryRequest(t *testing.T) {
	h := newHarness(t, "providers: { openai: { upstream: UPSTREAM } }\nmetering: { require_user: true }", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"model":"gpt-4o","usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	})
	h.post(t, "/openai/v1/chat/completions", `{"model":"gpt-4o"}`, map[string]string{"X-Toll-User": "u1"})
	h.post(t, "/openai/v1/chat/completions", `{"model":"gpt-4o"}`, nil) // rejected: no user
	h.waitEvents(t, 3)
	h.recorder.mu.Lock()
	defer h.recorder.mu.Unlock()
	if len(h.recorder.started) != 1 || len(h.recorder.completed) != 2 {
		t.Fatalf("started %v completed %d", h.recorder.started, len(h.recorder.completed))
	}
	if !h.recorder.completed[1].Rejected || *h.recorder.completed[0].User != "u1" {
		t.Fatalf("completed %+v", h.recorder.completed)
	}
}
