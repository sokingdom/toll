// Package provider defines how toll understands each LLM provider's wire
// format: where the API key and user ID live in a request, which endpoints
// carry token usage, and how to read usage back out of a response.
package provider

import (
	"io"
	"net/http"
	"strings"
)

// Meta is what toll learns from a request before forwarding it.
type Meta struct {
	// Endpoint is the upstream path, without toll's route prefix.
	Endpoint string
	// Kind names a metered endpoint, e.g. "chat" or "messages". Empty means
	// the endpoint does not report token usage and is passed through unmetered.
	Kind  string
	Model string
	// Stream reports whether the response will be streamed.
	Stream bool
	// User is the end-user ID found in the request body, if any.
	User    string
	KeyID   string
	KeyHint string
}

// Metered reports whether toll expects token usage in a successful response.
func (m Meta) Metered() bool { return m.Kind != "" }

// Usage is token usage normalized across providers. Input excludes cached
// tokens so that the fields never overlap.
type Usage struct {
	Input      int64
	CacheRead  int64
	CacheWrite int64
	Output     int64
	// Reasoning is informational: it is already included in Output.
	Reasoning int64
}

// Result is what toll learns from a response.
type Result struct {
	Model    string
	Usage    Usage
	HasUsage bool
}

// Adapter implements one provider wire format.
type Adapter interface {
	// Inspect extracts metadata from the request. body is the full request
	// body and may be empty. Inspect never fails: unknown shapes yield a
	// zero-value Kind and are passed through unmetered.
	Inspect(r *http.Request, endpoint string, body []byte) Meta
	// Prepare returns the body to send upstream and whether it differs from
	// the original. Adapters only modify bodies when usage would otherwise be
	// missing from the response.
	Prepare(meta Meta, body []byte) ([]byte, bool)
	// Parse reads the decoded response body of a successful metered request.
	// It may stop reading early; the caller drains the rest.
	Parse(meta Meta, resp *http.Response, body io.Reader) (Result, error)
}

// BearerToken returns the token from an "Authorization: Bearer" header.
func BearerToken(h http.Header) string {
	v := h.Get("Authorization")
	if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return ""
}

// NonNegative clamps v at zero. Providers occasionally report cached
// tokens that exceed the prompt count they are subtracted from.
func NonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}
