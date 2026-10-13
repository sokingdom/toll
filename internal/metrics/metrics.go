// Package metrics exposes toll's Prometheus metrics. Per-user data is
// deliberately absent: it lives in events, not in label sets.
package metrics

import (
	"net/http"
	"sort"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/sokingdom/toll/internal/provider"
)

type Metrics struct {
	tagKeys  []string
	registry *prometheus.Registry

	requests     *prometheus.CounterVec
	inflight     *prometheus.GaugeVec
	tokens       *prometheus.CounterVec
	duration     *prometheus.HistogramVec
	ttfb         *prometheus.HistogramVec
	usageMissing *prometheus.CounterVec
	parseErrors  *prometheus.CounterVec
	injected     *prometheus.CounterVec
	writeErrors  *prometheus.CounterVec
}

// New registers all metrics. tagKeys are the X-Toll-Tags keys promoted to
// labels; they are fixed for the lifetime of the process.
func New(tagKeys []string) *Metrics {
	tagLabels := make([]string, len(tagKeys))
	for i, k := range tagKeys {
		tagLabels[i] = "tag_" + k
	}
	with := func(base ...string) []string { return append(base, tagLabels...) }
	latencyBuckets := []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300}

	m := &Metrics{
		tagKeys:  tagKeys,
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toll_requests_total", Help: "Requests handled, by outcome.",
		}, with("provider", "model", "key", "status")),
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "toll_inflight_requests", Help: "Requests currently being proxied.",
		}, []string{"provider"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toll_tokens_total", Help: "Tokens reported by providers, by type.",
		}, with("provider", "model", "key", "type")),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "toll_request_duration_seconds", Help: "Time from request receipt to response end.", Buckets: latencyBuckets,
		}, []string{"provider", "model"}),
		ttfb: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "toll_time_to_first_byte_seconds", Help: "Time from request receipt to the first response body byte.", Buckets: latencyBuckets,
		}, []string{"provider", "model"}),
		usageMissing: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toll_usage_missing_total", Help: "Successful metered responses without usage.",
		}, []string{"provider", "key"}),
		parseErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toll_usage_parse_errors_total", Help: "Responses whose usage could not be parsed.",
		}, []string{"provider"}),
		injected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toll_stream_usage_injected_total", Help: "Requests modified to include stream usage.",
		}, []string{"provider", "key"}),
		writeErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toll_event_write_errors_total", Help: "Events that could not be written.",
		}, []string{"type"}),
	}
	m.registry.MustRegister(
		m.requests, m.inflight, m.tokens, m.duration, m.ttfb,
		m.usageMissing, m.parseErrors, m.injected, m.writeErrors,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Request describes one finished request for metric purposes.
type Request struct {
	Provider, Model, Key string
	Status               int
	Tags                 map[string]string
	Metered              bool // a successful, metered response
	Result               provider.Result
	ParseErr             bool
	Injected             bool
	DurationSeconds      float64
	TTFBSeconds          float64 // negative when no body byte was seen
	Rejected             bool
}

func (m *Metrics) Observe(r Request) {
	tagValues := make([]string, len(m.tagKeys))
	for i, k := range m.tagKeys {
		tagValues[i] = r.Tags[k]
	}
	with := func(base ...string) []string { return append(base, tagValues...) }

	m.requests.WithLabelValues(with(r.Provider, r.Model, r.Key, strconv.Itoa(r.Status))...).Inc()
	if r.Rejected {
		return
	}
	m.duration.WithLabelValues(r.Provider, r.Model).Observe(r.DurationSeconds)
	if r.TTFBSeconds >= 0 {
		m.ttfb.WithLabelValues(r.Provider, r.Model).Observe(r.TTFBSeconds)
	}
	if r.Injected {
		m.injected.WithLabelValues(r.Provider, r.Key).Inc()
	}
	if r.ParseErr {
		m.parseErrors.WithLabelValues(r.Provider).Inc()
	}
	if r.Result.HasUsage {
		u := r.Result.Usage
		for typ, n := range map[string]int64{
			"input": u.Input, "cache_read": u.CacheRead, "cache_write": u.CacheWrite,
			"output": u.Output, "reasoning": u.Reasoning,
		} {
			if n > 0 {
				m.tokens.WithLabelValues(with(r.Provider, r.Model, r.Key, typ)...).Add(float64(n))
			}
		}
	} else if r.Metered {
		m.usageMissing.WithLabelValues(r.Provider, r.Key).Inc()
	}
}

func (m *Metrics) Inflight(provider string, delta float64) {
	m.inflight.WithLabelValues(provider).Add(delta)
}

func (m *Metrics) WriteError(eventType string) {
	m.writeErrors.WithLabelValues(eventType).Inc()
}

// Summary is a snapshot of the request and token counters since the process
// started, for the dashboard. Tag labels are summed away.
type Summary struct {
	Rows         []SummaryRow     `json:"rows"`
	Status       map[string]int64 `json:"status"`
	Inflight     int64            `json:"inflight"`
	UsageMissing int64            `json:"usage_missing"`
	WriteErrors  int64            `json:"write_errors"`
}

type SummaryRow struct {
	Provider string           `json:"provider"`
	Model    string           `json:"model"`
	Key      string           `json:"key"`
	Requests int64            `json:"requests"`
	Errors   int64            `json:"errors"`
	Tokens   map[string]int64 `json:"tokens"`
}

func (m *Metrics) Summary() (Summary, error) {
	families, err := m.registry.Gather()
	if err != nil {
		return Summary{}, err
	}
	s := Summary{Status: map[string]int64{}}
	rows := map[[3]string]*SummaryRow{}
	row := func(labels map[string]string) *SummaryRow {
		k := [3]string{labels["provider"], labels["model"], labels["key"]}
		if rows[k] == nil {
			rows[k] = &SummaryRow{Provider: k[0], Model: k[1], Key: k[2], Tokens: map[string]int64{}}
		}
		return rows[k]
	}
	for _, f := range families {
		for _, metric := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range metric.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			switch f.GetName() {
			case "toll_requests_total":
				n := int64(metric.GetCounter().GetValue())
				r := row(labels)
				r.Requests += n
				s.Status[labels["status"]] += n
				if code, _ := strconv.Atoi(labels["status"]); code < 200 || code > 299 {
					r.Errors += n
				}
			case "toll_tokens_total":
				row(labels).Tokens[labels["type"]] += int64(metric.GetCounter().GetValue())
			case "toll_inflight_requests":
				s.Inflight += int64(metric.GetGauge().GetValue())
			case "toll_usage_missing_total":
				s.UsageMissing += int64(metric.GetCounter().GetValue())
			case "toll_event_write_errors_total":
				s.WriteErrors += int64(metric.GetCounter().GetValue())
			}
		}
	}
	s.Rows = make([]SummaryRow, 0, len(rows))
	for _, r := range rows {
		s.Rows = append(s.Rows, *r)
	}
	sort.Slice(s.Rows, func(i, j int) bool {
		a, b := s.Rows[i], s.Rows[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.Key < b.Key
	})
	return s, nil
}
