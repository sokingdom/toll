// Package server implements toll's proxy listener: routing, the metering
// proxy, and event emission.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sokingdom/toll/internal/config"
	"github.com/sokingdom/toll/internal/events"
	"github.com/sokingdom/toll/internal/metrics"
	"github.com/sokingdom/toll/internal/provider"
	"github.com/sokingdom/toll/internal/provider/anthropic"
	"github.com/sokingdom/toll/internal/provider/gemini"
	"github.com/sokingdom/toll/internal/provider/openai"
	"github.com/sokingdom/toll/internal/reqid"
)

var adapters = map[string]provider.Adapter{
	config.FormatOpenAI:    openai.Adapter{},
	config.FormatAnthropic: anthropic.Adapter{},
	config.FormatGemini:    gemini.Adapter{},
}

type Options struct {
	Config  *config.Config
	Events  events.Writer
	Metrics *metrics.Metrics
	Logger  *slog.Logger
	// Recorder receives a copy of every event for the dashboard. Optional.
	Recorder Recorder
	// Transport overrides the upstream transport, mainly for tests.
	Transport http.RoundTripper
}

// Recorder keeps a copy of events, such as the dashboard store.
type Recorder interface {
	Started(ev events.Started, at time.Time)
	Completed(ev events.Completed, startedAt time.Time)
}

type nopRecorder struct{}

func (nopRecorder) Started(events.Started, time.Time)     {}
func (nopRecorder) Completed(events.Completed, time.Time) {}

// Server is an http.Handler. Its reloadable settings live in an atomically
// swapped snapshot, so a reload never affects requests already in flight.
type Server struct {
	state     atomic.Pointer[state]
	events    events.Writer
	recorder  Recorder
	metrics   *metrics.Metrics
	log       *slog.Logger
	transport http.RoundTripper
	instance  string

	inflight     sync.WaitGroup
	lastWriteLog atomic.Int64
}

type state struct {
	routes      map[string]*route
	aliases     map[string]string
	requireUser bool
	inject      bool
	failClosed  bool
	authToken   string
	maxBody     int64
}

type route struct {
	name     string
	upstream *url.URL
	adapter  provider.Adapter
}

func New(o Options) (*Server, error) {
	s := &Server{
		events:    o.Events,
		recorder:  o.Recorder,
		metrics:   o.Metrics,
		log:       o.Logger,
		transport: o.Transport,
		instance:  o.Config.Server.InstanceID,
	}
	if s.transport == nil {
		s.transport = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          1024,
			MaxIdleConnsPerHost:   256,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: o.Config.Server.UpstreamResponseHeaderTimeout,
		}
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.recorder == nil {
		s.recorder = nopRecorder{}
	}
	if err := s.Reload(o.Config); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload applies the reloadable parts of c: providers, key aliases, metering
// switches, the write-error policy, the auth token and the body size limit.
func (s *Server) Reload(c *config.Config) error {
	st := &state{
		routes:      make(map[string]*route, len(c.Providers)),
		aliases:     c.KeyAliases,
		requireUser: c.Metering.RequireUser,
		inject:      c.Metering.InjectsStreamUsage(),
		failClosed:  c.Events.OnWriteError == config.FailClosed,
		authToken:   c.Server.AuthToken,
		maxBody:     int64(c.Server.MaxRequestBody),
	}
	for name, p := range c.Providers {
		u, err := url.Parse(p.Upstream)
		if err != nil {
			return fmt.Errorf("providers.%s: %w", name, err)
		}
		a, ok := adapters[p.Format]
		if !ok {
			return fmt.Errorf("providers.%s: unknown format %q", name, p.Format)
		}
		st.routes[name] = &route{name: name, upstream: u, adapter: a}
	}
	s.state.Store(st)
	return nil
}

// Wait blocks until all proxied requests have finished.
func (s *Server) Wait() { s.inflight.Wait() }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.inflight.Add(1)
	defer s.inflight.Done()

	st := s.state.Load()
	id := reqid.New()
	w.Header().Set("X-Toll-Request-Id", id)

	if st.authToken != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Toll-Auth")), []byte(st.authToken)) != 1 {
		writeTollError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid X-Toll-Auth header")
		return
	}
	name, rest, rawRest := splitRoute(r.URL)
	rt := st.routes[name]
	if rt == nil {
		writeTollError(w, http.StatusNotFound, "unknown_route", fmt.Sprintf("no provider is configured for /%s", name))
		return
	}
	x := &exchange{s: s, st: st, rt: rt, w: w, r: r, id: id, start: time.Now()}
	x.serve(rest, rawRest)
}

// splitRoute splits "/openai/v1/chat/completions" into the route name and
// the upstream path, keeping the original escaping of the path.
func splitRoute(u *url.URL) (name, rest, rawRest string) {
	name, after, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	_, rawAfter, _ := strings.Cut(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	return name, "/" + after, "/" + rawAfter
}

func writeTollError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"toll_error": map[string]string{"code": code, "message": message},
	})
}

// writeEvent persists an event and records failures. Errors are logged at
// most every ten seconds so that a full disk does not flood stderr.
func (s *Server) writeEvent(eventType string, ev any) error {
	err := s.events.Write(ev)
	if err != nil {
		s.metrics.WriteError(eventType)
		now := time.Now().Unix()
		if last := s.lastWriteLog.Load(); now-last >= 10 && s.lastWriteLog.CompareAndSwap(last, now) {
			s.log.Error("event write failed", "type", eventType, "err", err)
		}
	}
	return err
}
