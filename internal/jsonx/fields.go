package jsonx

import (
	"encoding/json"
	"errors"
	"io"
)

// DecodeFields streams a top-level JSON object from r, decoding only the
// fields listed in targets and skipping all others token by token, so large
// values (embedding vectors, long outputs) are never held in memory at once.
func DecodeFields(r io.Reader, targets map[string]any) error {
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok != json.Delim('{') {
		return errors.New("jsonx: expected object")
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := kt.(string)
		if target, ok := targets[key]; ok {
			if err := dec.Decode(target); err != nil {
				return err
			}
			continue
		}
		if err := skipValue(dec); err != nil {
			return err
		}
	}
	_, err = dec.Token()
	return err
}

func skipValue(dec *json.Decoder) error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
		if depth == 0 {
			return nil
		}
	}
}
