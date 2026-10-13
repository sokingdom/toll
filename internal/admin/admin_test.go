package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sokingdom/toll/internal/config"
	"github.com/sokingdom/toll/internal/events"
	"github.com/sokingdom/toll/internal/metrics"
	"github.com/sokingdom/toll/internal/reqid"
	"github.com/sokingdom/toll/internal/store"
)

func setup(t *testing.T, started time.Time) (*httptest.Server, *store.Store) {
	t.Helper()
	cfg, err := config.Parse([]byte("server: { instance_id: i1, auth_token: s3cret }\nkey_aliases: { k_1: search }\nproviders: { vllm: { upstream: \"http://user:pw@vllm:8000\" } }"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "toll.db"), 31*24*time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(New(Options{
		Metrics: metrics.New(nil), Store: st, StartedAt: started,
		Config: func() *config.Config { return cfg },
	}))
	t.Cleanup(srv.Close)
	return srv, st
}

func get(t *testing.T, url string, out any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode == 200 {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s: %v: %s", url, err, b)
		}
	}
	return resp.StatusCode
}

func ptr[T any](v T) *T { return &v }

func record(st *store.Store, at time.Time, user, model string, status int, in, out int64) {
	c := events.Common{
		RequestID: reqid.Floor(at)[:10] + reqid.New()[10:], User: ptr(user),
		Provider: "openai", Endpoint: "chat", RequestedModel: ptr(model), KeyID: ptr("k_1"),
	}
	ev := events.Completed{Common: c, Status: status, UsageSource: events.UsageProvider, InputTokens: ptr(in), OutputTokens: ptr(out)}
	st.Completed(ev, at)
}

func TestUsageByDayAndHour(t *testing.T) {
	srv, st := setup(t, time.Now())
	seoul, _ := time.LoadLocation("Asia/Seoul")
	// 2026-10-12 23:30 and 2026-10-13 00:30 in Seoul fall on different
	// local days although both are 2026-10-12 in UTC.
	record(st, time.Date(2026, 10, 12, 23, 30, 0, 0, seoul), "u1", "gpt-4o", 200, 10, 5)
	record(st, time.Date(2026, 10, 13, 0, 30, 0, 0, seoul), "u1", "gpt-4o", 200, 20, 7)
	record(st, time.Date(2026, 10, 13, 0, 40, 0, 0, seoul), "u1", "claude", 500, 0, 0)
	record(st, time.Date(2026, 10, 13, 0, 40, 0, 0, seoul), "u2", "gpt-4o", 200, 99, 99)
	st.Flush()

	var week struct {
		Bucket string
		Total  struct{ Requests, Errors, InputTokens, OutputTokens int64 } `json:"total"`
		Models []struct{ Model string }
		Series []struct {
			Start    int64
			Requests int64
		}
	}
	if code := get(t, srv.URL+"/api/usage?user=u1&from=2026-10-12&to=2026-10-18&tz=Asia/Seoul", &week); code != 200 {
		t.Fatalf("status %d", code)
	}
	if week.Bucket != "day" || len(week.Series) != 7 || week.Series[0].Requests != 1 || week.Series[1].Requests != 2 {
		t.Fatalf("week %+v", week)
	}
	if week.Total.Requests != 3 || week.Total.Errors != 1 || len(week.Models) != 2 || week.Models[0].Model != "gpt-4o" {
		t.Fatalf("week totals %+v", week)
	}

	var day struct {
		Bucket string
		Series []struct{ Requests int64 }
	}
	get(t, srv.URL+"/api/usage?user=u1&from=2026-10-13&to=2026-10-13&tz=Asia/Seoul", &day)
	if day.Bucket != "hour" || len(day.Series) != 24 || day.Series[0].Requests != 2 {
		t.Fatalf("day %+v", day)
	}

	if code := get(t, srv.URL+"/api/usage?user=u1&from=2026-09-01&to=2026-10-13", nil); code != 400 {
		t.Fatalf("range over 31 days: %d", code)
	}
	if code := get(t, srv.URL+"/api/usage?from=2026-10-13&to=2026-10-13", nil); code != 400 {
		t.Fatalf("missing user: %d", code)
	}
}

func TestOpenSplitsInflightAndLost(t *testing.T) {
	started := time.Now()
	srv, st := setup(t, started)
	before := started.Add(-time.Hour)
	st.Started(events.Started{Common: events.Common{RequestID: reqid.Floor(before)[:10] + reqid.New()[10:], Provider: "openai"}}, before)
	st.Started(events.Started{Common: events.Common{RequestID: reqid.New(), Provider: "openai"}}, time.Now())
	st.Flush()

	var open struct {
		InFlight []store.Request `json:"in_flight"`
		Lost     []store.Request `json:"lost"`
	}
	get(t, srv.URL+"/api/open", &open)
	if len(open.InFlight) != 1 || len(open.Lost) != 1 {
		t.Fatalf("open %+v", open)
	}
}

func TestRequestsPaging(t *testing.T) {
	srv, st := setup(t, time.Now())
	for i := 0; i < 3; i++ {
		record(st, time.Now().Add(time.Duration(-i)*time.Minute), "u1", "m", 200, 1, 1)
	}
	st.Flush()
	var page struct {
		Requests []store.Request
		Next     string
	}
	get(t, srv.URL+"/api/requests?limit=2", &page)
	if len(page.Requests) != 2 || page.Next == "" || page.Requests[0].KeyAlias == nil || *page.Requests[0].KeyAlias != "search" {
		t.Fatalf("page 1 %+v", page)
	}
	get(t, srv.URL+"/api/requests?limit=2&before="+page.Next, &page)
	if len(page.Requests) != 1 || page.Next != "" {
		t.Fatalf("page 2 %+v", page)
	}
}

func TestConfigHidesSecrets(t *testing.T) {
	srv, _ := setup(t, time.Now())
	resp, err := http.Get(srv.URL + "/api/config")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(b), "s3cret") || strings.Contains(string(b), "pw@") {
		t.Fatalf("config leaks secrets: %s", b)
	}
}

func TestStaticAndHealth(t *testing.T) {
	srv, _ := setup(t, time.Now())
	for path, want := range map[string]string{
		"/healthz":            "ok",
		"/dashboard/":         "toll dashboard",
		"/dashboard/app.js":   "requestTable",
		"/dashboard/logo.png": "PNG",
		"/metrics":            "go_goroutines",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), want) {
			t.Errorf("%s: %d, missing %q", path, resp.StatusCode, want)
		}
	}
}

func TestDashboardDisabled(t *testing.T) {
	cfg, _ := config.Parse(nil)
	srv := httptest.NewServer(New(Options{Metrics: metrics.New(nil), Config: func() *config.Config { return cfg }}))
	defer srv.Close()
	if code := get(t, srv.URL+"/dashboard/", nil); code != 404 {
		t.Fatalf("dashboard served while disabled: %d", code)
	}
	if code := get(t, srv.URL+"/healthz", nil); code != 200 {
		t.Fatalf("healthz: %d", code)
	}
}
