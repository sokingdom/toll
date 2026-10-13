// Package admin serves toll's admin listener: the dashboard, its JSON API,
// Prometheus metrics and the health check. It is meant for operators and
// should not be reachable by the applications that call the proxy.
package admin

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
	_ "time/tzdata" // the dashboard groups usage by the viewer's time zone

	"github.com/sokingdom/toll/internal/config"
	"github.com/sokingdom/toll/internal/metrics"
	"github.com/sokingdom/toll/internal/store"
)

//go:embed web
var web embed.FS

type Options struct {
	Metrics *metrics.Metrics
	// Store is nil when the dashboard is disabled; metrics and health are
	// still served.
	Store     *store.Store
	Config    func() *config.Config
	StartedAt time.Time
	Logger    *slog.Logger
}

type handler struct {
	Options
	instance string
}

func New(o Options) http.Handler {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	h := &handler{Options: o, instance: o.Config().Server.InstanceID}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /metrics", o.Metrics.Handler())
	if o.Store == nil {
		return mux
	}

	static, _ := fs.Sub(web, "web")
	files := http.StripPrefix("/dashboard/", http.FileServerFS(static))
	mux.Handle("GET /dashboard/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Embedded files carry no modification time; revalidate so an
		// upgraded binary never serves a stale script from the browser cache.
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
	mux.Handle("GET /{$}", http.RedirectHandler("/dashboard/", http.StatusFound))
	mux.HandleFunc("GET /api/overview", h.overview)
	mux.HandleFunc("GET /api/requests", h.requests)
	mux.HandleFunc("GET /api/open", h.open)
	mux.HandleFunc("GET /api/usage", h.usage)
	mux.HandleFunc("GET /api/config", h.config)
	return mux
}

func (h *handler) overview(w http.ResponseWriter, r *http.Request) {
	summary, err := h.Metrics.Summary()
	if err != nil {
		h.fail(w, err)
		return
	}
	stats := h.Store.Stats(r.Context())
	writeJSON(w, map[string]any{
		"instance":   h.instance,
		"started_at": h.StartedAt.UnixMilli(),
		"now":        time.Now().UnixMilli(),
		"summary":    summary,
		"store":      stats,
	})
}

func (h *handler) requests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.Filter{
		User: q.Get("user"), Provider: q.Get("provider"), Model: q.Get("model"),
		Status: q.Get("status"), Before: q.Get("before"),
		Since: unixMilli(q.Get("since")), Until: unixMilli(q.Get("until")),
	}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	rows, err := h.Store.List(r.Context(), f)
	if err != nil {
		h.fail(w, err)
		return
	}
	next := ""
	if len(rows) == f.Limit {
		next = rows[len(rows)-1].RequestID
	}
	h.alias(rows)
	writeJSON(w, map[string]any{"requests": rows, "next": next})
}

func unixMilli(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(n)
}

// alias names keys with the current key_aliases, so renaming a key in the
// configuration also renames it in past requests.
func (h *handler) alias(rows []store.Request) {
	aliases := h.Config().KeyAliases
	for i, row := range rows {
		if row.KeyID != nil {
			if a, ok := aliases[*row.KeyID]; ok {
				rows[i].KeyAlias = &a
			}
		}
	}
}

// open lists requests without a completion. Those started by this process
// are in flight; any other was cut off by a crash or kill. The store belongs
// to one process at a time, so the start time is enough to tell them apart.
func (h *handler) open(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Store.Open(r.Context(), 200)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.alias(rows)
	inflight, lost := []store.Request{}, []store.Request{}
	since := h.StartedAt.UnixMilli()
	for _, row := range rows {
		if row.StartedAt >= since {
			inflight = append(inflight, row)
		} else {
			lost = append(lost, row)
		}
	}
	writeJSON(w, map[string]any{"in_flight": inflight, "lost": lost})
}

type usage struct {
	Requests   int64 `json:"requests"`
	Errors     int64 `json:"errors"`
	Input      int64 `json:"input_tokens"`
	CacheRead  int64 `json:"cache_read_tokens"`
	CacheWrite int64 `json:"cache_write_tokens"`
	Output     int64 `json:"output_tokens"`
	Reasoning  int64 `json:"reasoning_tokens"`
}

func (u *usage) add(row store.UsageRow) {
	u.Requests++
	if row.Status < 200 || row.Status > 299 {
		u.Errors++
	}
	u.Input += row.Tokens[0]
	u.CacheRead += row.Tokens[1]
	u.CacheWrite += row.Tokens[2]
	u.Output += row.Tokens[3]
	u.Reasoning += row.Tokens[4]
}

type modelUsage struct {
	Model string `json:"model"`
	usage
}

type bucketUsage struct {
	Start int64 `json:"start"` // unix ms
	usage
}

// usage aggregates one user's requests over whole days [from, to] in the
// viewer's time zone: hourly for a single day, daily otherwise.
func (h *handler) usage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	user := q.Get("user")
	if user == "" {
		http.Error(w, "user is required", http.StatusBadRequest)
		return
	}
	loc, err := time.LoadLocation(q.Get("tz"))
	if err != nil {
		http.Error(w, "unknown time zone", http.StatusBadRequest)
		return
	}
	from, err1 := time.ParseInLocation(time.DateOnly, q.Get("from"), loc)
	to, err2 := time.ParseInLocation(time.DateOnly, q.Get("to"), loc)
	if err1 != nil || err2 != nil || to.Before(from) {
		http.Error(w, "from and to must be dates (YYYY-MM-DD), from <= to", http.StatusBadRequest)
		return
	}
	until := to.AddDate(0, 0, 1)
	days := 0
	for d := from; d.Before(until); d = d.AddDate(0, 0, 1) {
		days++
	}
	if days > 31 {
		http.Error(w, "the range can span at most 31 days", http.StatusBadRequest)
		return
	}

	hourly := days == 1
	bucketOf := func(t time.Time) time.Time {
		t = t.In(loc)
		if hourly {
			return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc)
		}
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	}
	var series []*bucketUsage
	index := map[int64]*bucketUsage{}
	for t := from; t.Before(until); {
		b := &bucketUsage{Start: t.UnixMilli()}
		series = append(series, b)
		index[b.Start] = b
		if hourly {
			t = t.Add(time.Hour)
		} else {
			t = t.AddDate(0, 0, 1)
		}
	}

	var total usage
	models := map[string]*modelUsage{}
	err = h.Store.ScanUser(r.Context(), user, from, until, func(row store.UsageRow) {
		total.add(row)
		m := models[row.Model]
		if m == nil {
			m = &modelUsage{Model: row.Model}
			models[row.Model] = m
		}
		m.add(row)
		if b := index[bucketOf(row.StartedAt).UnixMilli()]; b != nil {
			b.add(row)
		}
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	byModel := make([]*modelUsage, 0, len(models))
	for _, m := range models {
		byModel = append(byModel, m)
	}
	sort.Slice(byModel, func(i, j int) bool {
		a, b := byModel[i], byModel[j]
		return a.Input+a.Output > b.Input+b.Output
	})
	bucket := "day"
	if hourly {
		bucket = "hour"
	}
	writeJSON(w, map[string]any{
		"user": user, "from": q.Get("from"), "to": q.Get("to"), "tz": loc.String(),
		"since": from.UnixMilli(), "until": until.UnixMilli(), "bucket": bucket,
		"total": total, "models": byModel, "series": series,
	})
}

// config shows the running configuration without secrets.
func (h *handler) config(w http.ResponseWriter, r *http.Request) {
	c := h.Config()
	providers := map[string]map[string]string{}
	for name, p := range c.Providers {
		upstream := p.Upstream
		if u, err := url.Parse(upstream); err == nil && u.User != nil {
			u.User = url.User("redacted")
			upstream = u.String()
		}
		providers[name] = map[string]string{"upstream": upstream, "format": p.Format}
	}
	writeJSON(w, map[string]any{
		"server": map[string]any{
			"listen":                           c.Server.Listen,
			"admin_listen":                     c.Server.AdminAddr(),
			"instance_id":                      c.Server.InstanceID,
			"auth_token":                       c.Server.AuthToken != "",
			"max_request_body":                 int64(c.Server.MaxRequestBody),
			"shutdown_grace":                   c.Server.ShutdownGrace.String(),
			"read_header_timeout":              c.Server.ReadHeaderTimeout.String(),
			"upstream_response_header_timeout": c.Server.UpstreamResponseHeaderTimeout.String(),
		},
		"providers": providers,
		"metering": map[string]any{
			"require_user":        c.Metering.RequireUser,
			"inject_stream_usage": c.Metering.InjectsStreamUsage(),
			"metric_tags":         c.Metering.MetricTags,
		},
		"key_aliases": c.KeyAliases,
		"events": map[string]any{
			"output":         c.Events.Output,
			"dir":            c.Events.Dir,
			"filename":       c.Events.Filename,
			"rotate":         c.Events.Rotate.String(),
			"on_write_error": c.Events.OnWriteError,
		},
		"dashboard": map[string]any{
			"path":      c.Dashboard.Path,
			"retention": store.FormatDuration(time.Duration(c.Dashboard.Retention)),
		},
	})
}

func (h *handler) fail(w http.ResponseWriter, err error) {
	h.Logger.Error("dashboard query failed", "err", err)
	http.Error(w, "query failed", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
