package server

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/sokingdom/toll/internal/events"
	"github.com/sokingdom/toll/internal/metrics"
	"github.com/sokingdom/toll/internal/provider"
)

// statusClientClosed is recorded when the caller disconnects before the
// upstream responds (nginx's convention; never sent on the wire).
const statusClientClosed = 499

// exchange is the life of one proxied request.
type exchange struct {
	s     *Server
	st    *state
	rt    *route
	w     http.ResponseWriter
	r     *http.Request
	id    string
	start time.Time

	meta     provider.Meta
	common   events.Common
	keyLabel string
	tags     map[string]string

	// Set while proxying. All fields below are touched only by the handler
	// goroutine, except parsed which is a channel.
	status      int
	gotResponse bool
	metered     bool
	eof         bool
	firstByte   time.Time
	parsed      chan parseOutcome
}

type parseOutcome struct {
	result provider.Result
	err    error
}

func (x *exchange) serve(rest, rawRest string) {
	body, err := readBody(x.w, x.r, x.st.maxBody)
	var tooLarge *http.MaxBytesError
	if err != nil && !errors.As(err, &tooLarge) {
		return // the caller went away while sending the body
	}
	x.meta = x.rt.adapter.Inspect(x.r, rest, body)
	x.buildCommon()

	if tooLarge != nil {
		x.reject(http.StatusRequestEntityTooLarge, "body_too_large", fmt.Sprintf("request body exceeds %d bytes", x.st.maxBody))
		return
	}
	if x.st.requireUser && x.common.User == nil {
		x.reject(http.StatusBadRequest, "user_required", "user id is required: set X-Toll-User header or the provider user field")
		return
	}

	out := body
	if x.st.inject {
		out, x.common.UsageInjected = x.rt.adapter.Prepare(x.meta, body)
	}

	started := events.Started{Common: x.common}
	started.Type, started.TS = events.TypeStarted, events.Timestamp(time.Now())
	if err := x.s.writeEvent(events.TypeStarted, started); err != nil && x.st.failClosed {
		x.reject(http.StatusServiceUnavailable, "event_write_failed", "toll could not record the request")
		return
	}

	x.s.metrics.Inflight(x.rt.name, 1)
	defer x.s.metrics.Inflight(x.rt.name, -1)

	if len(out) > 0 {
		x.r.Body = io.NopCloser(bytes.NewReader(out))
	} else {
		x.r.Body = http.NoBody
	}
	x.r.ContentLength = int64(len(out))
	x.r.TransferEncoding = nil

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Path, pr.Out.URL.RawPath = rest, rawRest
			pr.SetURL(x.rt.upstream)
			stripTollHeaders(pr.Out.Header)
			filterAcceptEncoding(pr.Out.Header)
		},
		Transport:      x.s.transport,
		FlushInterval:  -1,
		ModifyResponse: x.modifyResponse,
		ErrorHandler:   x.proxyError,
		ErrorLog:       slog.NewLogLogger(x.s.log.Handler(), slog.LevelDebug),
	}
	// ReverseProxy aborts the handler with panic(http.ErrAbortHandler) when
	// copying the response fails, e.g. on client disconnect. The deferred
	// finish still records the request before the panic continues.
	defer x.finish()
	proxy.ServeHTTP(x.w, x.r)
}

func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	return io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
}

func (x *exchange) buildCommon() {
	user, source := x.r.Header.Get("X-Toll-User"), "header"
	if user == "" {
		user, source = x.meta.User, "body"
	}
	if user == "" {
		source = "none"
	}
	x.tags = parseTags(x.r.Header.Get("X-Toll-Tags"))

	alias := x.st.aliases[x.meta.KeyID]
	switch {
	case alias != "":
		x.keyLabel = alias
	case x.meta.KeyID != "":
		x.keyLabel = x.meta.KeyID
	default:
		x.keyLabel = "none"
	}

	x.common = events.Common{
		RequestID:      x.id,
		Instance:       x.s.instance,
		User:           optional(user),
		UserSource:     source,
		KeyID:          optional(x.meta.KeyID),
		KeyHint:        optional(x.meta.KeyHint),
		KeyAlias:       optional(alias),
		Provider:       x.rt.name,
		Endpoint:       x.meta.Endpoint,
		RequestedModel: optional(x.meta.Model),
		Tags:           x.tags,
		Stream:         x.meta.Stream,
	}
}

// parseTags reads "k=v,k=v". Malformed entries are ignored.
func parseTags(h string) map[string]string {
	tags := map[string]string{}
	for _, part := range strings.Split(h, ",") {
		k, v, ok := strings.Cut(part, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if ok && k != "" {
			tags[k] = v
		}
	}
	return tags
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func stripTollHeaders(h http.Header) {
	for k := range h {
		if strings.HasPrefix(k, "X-Toll-") {
			delete(h, k)
		}
	}
}

// filterAcceptEncoding keeps only encodings toll can decode to read usage
// (gzip, deflate). The response the caller receives is still a valid
// encoding it asked for, or identity.
func filterAcceptEncoding(h http.Header) {
	values := h.Values("Accept-Encoding")
	if len(values) == 0 {
		return
	}
	var keep []string
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			coding, _, _ := strings.Cut(part, ";")
			switch strings.ToLower(strings.TrimSpace(coding)) {
			case "gzip", "deflate", "identity":
				keep = append(keep, strings.TrimSpace(part))
			}
		}
	}
	if len(keep) == 0 {
		h.Set("Accept-Encoding", "identity")
		return
	}
	h.Set("Accept-Encoding", strings.Join(keep, ", "))
}

func (x *exchange) modifyResponse(resp *http.Response) error {
	x.status, x.gotResponse = resp.StatusCode, true
	tee := &teeBody{rc: resp.Body, x: x}
	resp.Body = tee
	if !x.meta.Metered() || resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil
	}
	x.metered = true
	pr, pw := io.Pipe()
	tee.pw = pw
	x.parsed = make(chan parseOutcome, 1)
	adapter, meta, encoding := x.rt.adapter, x.meta, resp.Header.Get("Content-Encoding")
	go func() {
		var out parseOutcome
		body, err := decode(pr, encoding)
		if err != nil {
			out.err = err
		} else {
			out.result, out.err = adapter.Parse(meta, resp, body)
		}
		_, _ = io.Copy(io.Discard, pr) // keep the proxy unblocked whatever the parser did
		x.parsed <- out
	}()
	return nil
}

func decode(r io.Reader, encoding string) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return r, nil
	case "gzip":
		return gzip.NewReader(r)
	case "deflate":
		return zlib.NewReader(r)
	default:
		return nil, fmt.Errorf("unsupported content encoding %q", encoding)
	}
}

func (x *exchange) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		x.status = statusClientClosed
		return
	}
	// url.Error embeds the upstream URL, which may carry a Gemini ?key=.
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	x.s.log.Warn("upstream request failed", "request_id", x.id, "provider", x.rt.name, "err", err)
	x.status = http.StatusBadGateway
	writeTollError(w, http.StatusBadGateway, "upstream_unreachable", "toll could not reach the upstream provider")
}

// teeBody passes the upstream response through unchanged while feeding a
// copy to the usage parser.
type teeBody struct {
	rc io.ReadCloser
	pw *io.PipeWriter
	x  *exchange
}

func (t *teeBody) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		if t.x.firstByte.IsZero() {
			t.x.firstByte = time.Now()
		}
		if t.pw != nil {
			_, _ = t.pw.Write(p[:n])
		}
	}
	if errors.Is(err, io.EOF) {
		t.x.eof = true
	}
	return n, err
}

func (t *teeBody) Close() error {
	if t.pw != nil {
		t.pw.Close()
	}
	return t.rc.Close()
}

func (x *exchange) finish() {
	var outcome parseOutcome
	if x.parsed != nil {
		outcome = <-x.parsed
	}
	res := outcome.result
	if outcome.err != nil && x.eof {
		// Errors on a truncated body are expected; only complete bodies count.
		x.s.log.Warn("usage parse failed", "request_id", x.id, "provider", x.rt.name, "err", outcome.err)
	}
	end := time.Now()

	c := events.Completed{
		Common:    x.common,
		Status:    x.status,
		Partial:   x.status == statusClientClosed || (x.gotResponse && !x.eof),
		LatencyMS: end.Sub(x.start).Milliseconds(),
	}
	c.Type, c.TS = events.TypeCompleted, events.Timestamp(end)
	c.Model = optional(res.Model)
	switch {
	case !x.metered:
		c.UsageSource = events.UsageNone
	case res.HasUsage:
		c.UsageSource = events.UsageProvider
		u := res.Usage
		c.InputTokens, c.CacheReadTokens, c.CacheWriteTokens = &u.Input, &u.CacheRead, &u.CacheWrite
		c.OutputTokens, c.ReasoningTokens = &u.Output, &u.Reasoning
	default:
		c.UsageSource = events.UsageMissing
	}
	ttfb := -1.0
	if !x.firstByte.IsZero() {
		ms := x.firstByte.Sub(x.start).Milliseconds()
		c.TTFBMS = &ms
		ttfb = x.firstByte.Sub(x.start).Seconds()
	}
	_ = x.s.writeEvent(events.TypeCompleted, c)

	x.s.metrics.Observe(metrics.Request{
		Provider:        x.rt.name,
		Model:           x.modelLabel(res.Model),
		Key:             x.keyLabel,
		Status:          x.status,
		Tags:            x.tags,
		Metered:         x.metered,
		Result:          res,
		ParseErr:        outcome.err != nil && x.eof,
		Injected:        x.common.UsageInjected,
		DurationSeconds: end.Sub(x.start).Seconds(),
		TTFBSeconds:     ttfb,
	})
}

func (x *exchange) reject(status int, code, message string) {
	writeTollError(x.w, status, code, message)
	end := time.Now()
	c := events.Completed{
		Common:      x.common,
		Status:      status,
		Rejected:    true,
		Error:       code,
		UsageSource: events.UsageNone,
		LatencyMS:   end.Sub(x.start).Milliseconds(),
	}
	c.Type, c.TS = events.TypeCompleted, events.Timestamp(end)
	c.UsageInjected = false
	_ = x.s.writeEvent(events.TypeCompleted, c)
	x.s.metrics.Observe(metrics.Request{
		Provider: x.rt.name,
		Model:    x.modelLabel(""),
		Key:      x.keyLabel,
		Status:   status,
		Tags:     x.tags,
		Rejected: true,
	})
}

func (x *exchange) modelLabel(responseModel string) string {
	switch {
	case responseModel != "":
		return responseModel
	case x.meta.Model != "":
		return x.meta.Model
	default:
		return "unknown"
	}
}
