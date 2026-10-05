package typesafe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Fields is a JSON object that keeps insertion order. Choice criteria and
// questions are sent in the order the caller wrote them, because that order is
// what the model reads.
type Fields struct {
	keys   []string
	values map[string]any
}

// NewFields returns an empty ordered object.
func NewFields() *Fields {
	return &Fields{values: map[string]any{}}
}

// Set adds or replaces a key; a replaced key keeps its original position.
func (f *Fields) Set(key string, value any) *Fields {
	if _, ok := f.values[key]; !ok {
		f.keys = append(f.keys, key)
	}
	f.values[key] = value
	return f
}

// Get returns the value stored under key.
func (f *Fields) Get(key string) (any, bool) {
	v, ok := f.values[key]
	return v, ok
}

// Keys returns the keys in insertion order.
func (f *Fields) Keys() []string {
	return append([]string(nil), f.keys...)
}

// Len returns the number of keys.
func (f *Fields) Len() int {
	return len(f.keys)
}

// MarshalJSON writes the object with its keys in insertion order.
func (f *Fields) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range f.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := marshal(k)
		if err != nil {
			return nil, err
		}
		vb, err := marshal(f.values[k])
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", k, err)
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// marshal encodes without HTML escaping, so text reaches the model as written.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// DecodeOrdered parses JSON like encoding/json, except that objects become
// *Fields (keeping key order) and numbers stay json.Number.
func DecodeOrdered(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, &json.SyntaxError{Offset: dec.InputOffset()}
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		if err == io.EOF {
			return nil, &json.SyntaxError{Offset: dec.InputOffset()}
		}
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := NewFields()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, _ := kt.(string)
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				obj.Set(key, v)
			}
			_, err := dec.Token()
			return obj, err
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			_, err := dec.Token()
			return arr, err
		}
		return nil, &json.SyntaxError{Offset: dec.InputOffset()}
	default:
		return t, nil
	}
}
