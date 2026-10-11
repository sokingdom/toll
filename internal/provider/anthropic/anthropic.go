// Package anthropic implements the Anthropic Messages wire format.
package anthropic

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

const kindMessages = "messages"

type Adapter struct{}

func (Adapter) Inspect(r *http.Request, endpoint string, body []byte) provider.Meta {
	meta := provider.Meta{Endpoint: endpoint}
	key := r.Header.Get("X-Api-Key")
	if key == "" {
		key = provider.BearerToken(r.Header)
	}
	meta.KeyID, meta.KeyHint = keyid.Fingerprint(key)
	if r.Method != http.MethodPost || !strings.HasSuffix(endpoint, "/messages") {
		return meta
	}
	meta.Kind = kindMessages
	var req struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(body, &req)
	meta.Model, meta.Stream, meta.User = req.Model, req.Stream, req.Metadata.UserID
	return meta
}

// Prepare never modifies Anthropic requests: usage is always reported.
func (Adapter) Prepare(_ provider.Meta, body []byte) ([]byte, bool) { return body, false }

// usage fields are pointers because message_delta events only carry the
// fields that changed.
type usage struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
}

func (u usage) applyTo(out *provider.Usage) {
	set := func(dst *int64, src *int64) {
		if src != nil {
			*dst = *src
		}
	}
	set(&out.Input, u.InputTokens)
	set(&out.Output, u.OutputTokens)
	set(&out.CacheWrite, u.CacheCreationInputTokens)
	set(&out.CacheRead, u.CacheReadInputTokens)
}

func (Adapter) Parse(meta provider.Meta, _ *http.Response, body io.Reader) (provider.Result, error) {
	var res provider.Result
	if !meta.Stream {
		var model string
		var u *usage
		err := jsonx.DecodeFields(body, map[string]any{"model": &model, "usage": &u})
		res.Model = model
		if u != nil {
			u.applyTo(&res.Usage)
			res.HasUsage = true
		}
		return res, err
	}
	// message_start carries the model and input usage; each message_delta
	// carries cumulative output usage.
	err := sse.Read(body, func(data []byte) {
		if !bytes.Contains(data, []byte(`"message_start"`)) && !bytes.Contains(data, []byte(`"message_delta"`)) {
			return
		}
		var ev struct {
			Type    string `json:"type"`
			Message *struct {
				Model string `json:"model"`
				Usage *usage `json:"usage"`
			} `json:"message"`
			Usage *usage `json:"usage"`
		}
		if json.Unmarshal(data, &ev) != nil {
			return
		}
		switch ev.Type {
		case "message_start":
			if ev.Message == nil {
				return
			}
			res.Model = ev.Message.Model
			if ev.Message.Usage != nil {
				ev.Message.Usage.applyTo(&res.Usage)
				res.HasUsage = true
			}
		case "message_delta":
			if ev.Usage != nil {
				ev.Usage.applyTo(&res.Usage)
				res.HasUsage = true
			}
		}
	})
	return res, err
}
