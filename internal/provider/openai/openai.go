// Package openai implements the OpenAI wire format, also used by xAI and
// OpenAI-compatible servers such as vLLM and Ollama.
package openai

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/sokingdom/toll/internal/jsonx"
	"github.com/sokingdom/toll/internal/keyid"
	"github.com/sokingdom/toll/internal/provider"
	"github.com/sokingdom/toll/internal/sse"
)

const (
	kindChat        = "chat"
	kindCompletions = "completions"
	kindResponses   = "responses"
	kindEmbeddings  = "embeddings"
)

type Adapter struct{}

func (Adapter) Inspect(r *http.Request, endpoint string, body []byte) provider.Meta {
	meta := provider.Meta{Endpoint: endpoint}
	meta.KeyID, meta.KeyHint = keyid.Fingerprint(provider.BearerToken(r.Header))
	if r.Method != http.MethodPost {
		return meta
	}
	switch {
	case strings.HasSuffix(endpoint, "/chat/completions"):
		meta.Kind = kindChat
	case strings.HasSuffix(endpoint, "/completions"):
		meta.Kind = kindCompletions
	case strings.HasSuffix(endpoint, "/responses"):
		meta.Kind = kindResponses
	case strings.HasSuffix(endpoint, "/embeddings"):
		meta.Kind = kindEmbeddings
	default:
		return meta
	}
	var req struct {
		Model            string `json:"model"`
		Stream           bool   `json:"stream"`
		User             string `json:"user"`
		SafetyIdentifier string `json:"safety_identifier"`
	}
	_ = json.Unmarshal(body, &req)
	meta.Model, meta.Stream = req.Model, req.Stream
	meta.User = req.SafetyIdentifier
	if meta.User == "" {
		meta.User = req.User
	}
	return meta
}

// Prepare injects stream_options.include_usage into streaming Chat
// Completions and legacy Completions requests, which otherwise omit usage.
func (Adapter) Prepare(meta provider.Meta, body []byte) ([]byte, bool) {
	if !meta.Stream || (meta.Kind != kindChat && meta.Kind != kindCompletions) {
		return body, false
	}
	return jsonx.EnsureStreamUsage(body)
}

// chatUsage is the usage shape of Chat Completions, Completions and Embeddings.
type chatUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (u chatUsage) normalize() provider.Usage {
	var out provider.Usage
	if d := u.PromptTokensDetails; d != nil {
		out.CacheRead = d.CachedTokens
	}
	if d := u.CompletionTokensDetails; d != nil {
		out.Reasoning = d.ReasoningTokens
	}
	out.Input = provider.NonNegative(u.PromptTokens - out.CacheRead)
	out.Output = u.CompletionTokens
	return out
}

// responsesUsage is the usage shape of the Responses API.
type responsesUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (u responsesUsage) normalize() provider.Usage {
	var out provider.Usage
	if d := u.InputTokensDetails; d != nil {
		out.CacheRead = d.CachedTokens
	}
	if d := u.OutputTokensDetails; d != nil {
		out.Reasoning = d.ReasoningTokens
	}
	out.Input = provider.NonNegative(u.InputTokens - out.CacheRead)
	out.Output = u.OutputTokens
	return out
}

func (Adapter) Parse(meta provider.Meta, _ *http.Response, body io.Reader) (provider.Result, error) {
	if meta.Kind == kindResponses {
		return parseResponses(meta, body)
	}
	var res provider.Result
	if !meta.Stream {
		var model string
		var usage *chatUsage
		err := jsonx.DecodeFields(body, map[string]any{"model": &model, "usage": &usage})
		res.Model = model
		if usage != nil {
			res.Usage, res.HasUsage = usage.normalize(), true
		}
		return res, err
	}
	err := sse.Read(body, func(data []byte) {
		if bytes.Equal(data, []byte("[DONE]")) {
			return
		}
		var chunk struct {
			Model string     `json:"model"`
			Usage *chatUsage `json:"usage"`
		}
		if json.Unmarshal(data, &chunk) != nil {
			return
		}
		if chunk.Model != "" {
			res.Model = chunk.Model
		}
		if chunk.Usage != nil {
			res.Usage, res.HasUsage = chunk.Usage.normalize(), true
		}
	})
	return res, err
}

func parseResponses(meta provider.Meta, body io.Reader) (provider.Result, error) {
	var res provider.Result
	if !meta.Stream {
		var model string
		var usage *responsesUsage
		err := jsonx.DecodeFields(body, map[string]any{"model": &model, "usage": &usage})
		res.Model = model
		if usage != nil {
			res.Usage, res.HasUsage = usage.normalize(), true
		}
		return res, err
	}
	// Every lifecycle event carries the response object; the terminal ones
	// (completed, incomplete, failed) carry its final usage.
	err := sse.Read(body, func(data []byte) {
		if !bytes.Contains(data, []byte(`"response"`)) {
			return
		}
		var ev struct {
			Response *struct {
				Model string          `json:"model"`
				Usage *responsesUsage `json:"usage"`
			} `json:"response"`
		}
		if json.Unmarshal(data, &ev) != nil || ev.Response == nil {
			return
		}
		if ev.Response.Model != "" {
			res.Model = ev.Response.Model
		}
		if ev.Response.Usage != nil {
			res.Usage, res.HasUsage = ev.Response.Usage.normalize(), true
		}
	})
	return res, err
}
