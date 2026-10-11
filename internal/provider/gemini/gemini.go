// Package gemini implements the Google Gemini API wire format.
package gemini

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"

	"github.com/sokingdom/toll/internal/jsonx"
	"github.com/sokingdom/toll/internal/keyid"
	"github.com/sokingdom/toll/internal/provider"
	"github.com/sokingdom/toll/internal/sse"
)

const kindGenerate = "generate"

var generatePath = regexp.MustCompile(`/models/([^/:]+):(generateContent|streamGenerateContent)$`)

type Adapter struct{}

func (Adapter) Inspect(r *http.Request, endpoint string, _ []byte) provider.Meta {
	meta := provider.Meta{Endpoint: endpoint}
	key := r.Header.Get("X-Goog-Api-Key")
	if key == "" {
		key = r.URL.Query().Get("key")
	}
	meta.KeyID, meta.KeyHint = keyid.Fingerprint(key)
	m := generatePath.FindStringSubmatch(endpoint)
	if r.Method != http.MethodPost || m == nil {
		return meta
	}
	// The Gemini API has no end-user field; toll relies on X-Toll-User.
	meta.Kind = kindGenerate
	meta.Model = m[1]
	meta.Stream = m[2] == "streamGenerateContent"
	return meta
}

// Prepare never modifies Gemini requests: usage is always reported.
func (Adapter) Prepare(_ provider.Meta, body []byte) ([]byte, bool) { return body, false }

type usageMetadata struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
	ToolUsePromptTokenCount int64 `json:"toolUsePromptTokenCount"`
}

func (u usageMetadata) normalize() provider.Usage {
	return provider.Usage{
		Input:     provider.NonNegative(u.PromptTokenCount-u.CachedContentTokenCount) + u.ToolUsePromptTokenCount,
		CacheRead: u.CachedContentTokenCount,
		Output:    u.CandidatesTokenCount + u.ThoughtsTokenCount,
		Reasoning: u.ThoughtsTokenCount,
	}
}

type chunk struct {
	ModelVersion  string         `json:"modelVersion"`
	UsageMetadata *usageMetadata `json:"usageMetadata"`
}

func (c chunk) applyTo(res *provider.Result) {
	if c.ModelVersion != "" {
		res.Model = c.ModelVersion
	}
	if c.UsageMetadata != nil {
		res.Usage, res.HasUsage = c.UsageMetadata.normalize(), true
	}
}

func (Adapter) Parse(meta provider.Meta, resp *http.Response, body io.Reader) (provider.Result, error) {
	var res provider.Result
	switch {
	case !meta.Stream:
		var c chunk
		err := jsonx.DecodeFields(body, map[string]any{"modelVersion": &c.ModelVersion, "usageMetadata": &c.UsageMetadata})
		c.applyTo(&res)
		return res, err
	case resp.Request != nil && resp.Request.URL.Query().Get("alt") == "sse":
		// Each chunk repeats usageMetadata; the last one is final.
		err := sse.Read(body, func(data []byte) {
			var c chunk
			if json.Unmarshal(data, &c) == nil {
				c.applyTo(&res)
			}
		})
		return res, err
	default:
		// Without alt=sse the stream is a JSON array of chunks.
		dec := json.NewDecoder(body)
		if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
			return res, errors.New("gemini: expected JSON array stream")
		}
		for dec.More() {
			var c chunk
			if err := dec.Decode(&c); err != nil {
				return res, err
			}
			c.applyTo(&res)
		}
		return res, nil
	}
}
