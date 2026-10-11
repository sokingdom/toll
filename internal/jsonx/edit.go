// Package jsonx holds the small amount of JSON handling toll needs beyond
// encoding/json: byte-preserving edits of request bodies and streaming
// extraction of top-level fields from large response bodies.
package jsonx

import (
	"bytes"
	"encoding/json"
)

type field struct {
	key        string
	start, end int // byte range of the value
}

// scanObject locates the top-level fields of a JSON object without
// re-encoding it. open and close are the offsets of '{' and '}'.
func scanObject(b []byte) (fields []field, open, close int, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, 0, 0, false
	}
	open = int(dec.InputOffset()) - 1
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, 0, 0, false
		}
		key, _ := kt.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, 0, 0, false
		}
		end := int(dec.InputOffset())
		fields = append(fields, field{key: key, start: end - len(raw), end: end})
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, 0, 0, false
	}
	close = int(dec.InputOffset()) - 1
	return fields, open, close, true
}

func find(fields []field, key string) *field {
	for i := range fields {
		if fields[i].key == key {
			return &fields[i]
		}
	}
	return nil
}

func splice(b []byte, start, end int, insert string) []byte {
	out := make([]byte, 0, len(b)-(end-start)+len(insert))
	out = append(out, b[:start]...)
	out = append(out, insert...)
	return append(out, b[end:]...)
}

// EnsureStreamUsage makes sure an OpenAI-format request body has
// stream_options.include_usage set to true. Only the bytes needed for that
// change are touched; every other field keeps its original bytes and order.
// It reports whether the body was modified. Bodies it cannot safely edit are
// returned unchanged.
func EnsureStreamUsage(body []byte) ([]byte, bool) {
	fields, _, closeIdx, ok := scanObject(body)
	if !ok {
		return body, false
	}
	so := find(fields, "stream_options")
	if so == nil {
		insert := `"stream_options":{"include_usage":true}`
		if len(fields) > 0 {
			insert = "," + insert
		}
		return splice(body, closeIdx, closeIdx, insert), true
	}
	val := body[so.start:so.end]
	if string(val) == "null" {
		return splice(body, so.start, so.end, `{"include_usage":true}`), true
	}
	inner, open, _, ok := scanObject(val)
	if !ok {
		return body, false
	}
	if iu := find(inner, "include_usage"); iu != nil {
		if string(val[iu.start:iu.end]) == "true" {
			return body, false
		}
		return splice(body, so.start+iu.start, so.start+iu.end, "true"), true
	}
	insert := `"include_usage":true`
	if len(inner) > 0 {
		insert += ","
	}
	pos := so.start + open + 1
	return splice(body, pos, pos, insert), true
}
