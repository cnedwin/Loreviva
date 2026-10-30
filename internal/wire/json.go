// Package wire owns the one original-wire parser and exact payload canonicalizer.
package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const MaxBytes = 1 << 20
const MaxDepth = 64

var ErrJSON = errors.New("invalid or bounded JSON")

func stringsValid(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	in := false
	for i := 0; i < len(b); i++ {
		if !in {
			if b[i] == '"' {
				in = true
			}
			continue
		}
		if b[i] == '"' {
			in = false
			continue
		}
		if b[i] != '\\' {
			continue
		}
		i++
		if i >= len(b) {
			return false
		}
		if b[i] != 'u' {
			continue
		}
		if i+4 >= len(b) {
			return false
		}
		n, e := strconv.ParseUint(string(b[i+1:i+5]), 16, 16)
		if e != nil {
			return false
		}
		i += 4
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(b) || b[i+1] != '\\' || b[i+2] != 'u' {
				return false
			}
			m, e := strconv.ParseUint(string(b[i+3:i+7]), 16, 16)
			if e != nil || m < 0xdc00 || m > 0xdfff {
				return false
			}
			i += 6
		} else if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
	}
	return !in
}
func Parse(data []byte) (any, error) {
	return ParseBounded(data, MaxBytes)
}

// ParseBounded uses a trusted caller-specific ceiling without changing ordinary
// wire limits. All callers retain the same strict syntax and exact numbers.
func ParseBounded(data []byte, maxBytes int64) (any, error) {
	if maxBytes <= 0 || int64(len(data)) > maxBytes || !stringsValid(data) {
		return nil, ErrJSON
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	v, e := value(d, 0)
	if e != nil {
		return nil, ErrJSON
	}
	if _, e = d.Token(); e != io.EOF {
		return nil, ErrJSON
	}
	return v, nil
}
func value(d *json.Decoder, depth int) (any, error) {
	if depth > MaxDepth {
		return nil, ErrJSON
	}
	t, e := d.Token()
	if e != nil {
		return nil, e
	}
	switch x := t.(type) {
	case json.Delim:
		switch x {
		case '{':
			m := map[string]any{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return nil, e
				}
				s, ok := k.(string)
				if !ok {
					return nil, ErrJSON
				}
				if _, exists := m[s]; exists {
					return nil, ErrJSON
				}
				v, e := value(d, depth+1)
				if e != nil {
					return nil, e
				}
				m[s] = v
			}
			end, e := d.Token()
			if e != nil || end != json.Delim('}') {
				return nil, ErrJSON
			}
			return m, nil
		case '[':
			a := []any{}
			for d.More() {
				v, e := value(d, depth+1)
				if e != nil {
					return nil, e
				}
				a = append(a, v)
			}
			end, e := d.Token()
			if e != nil || end != json.Delim(']') {
				return nil, ErrJSON
			}
			return a, nil
		default:
			return nil, ErrJSON
		}
	case json.Number:
		if _, e := number(string(x)); e != nil {
			return nil, e
		}
		return x, nil
	default:
		return t, nil
	}
}

// Decimal coefficient plus exponent is exact and never allocates exponent-sized
// zero strings. Strings and identity bytes are never normalized.
func number(s string) (string, error) {
	if len(s) > 1024 {
		return "", ErrJSON
	}
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign = "-"
		s = s[1:]
	}
	exp := int64(0)
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		v, e := strconv.ParseInt(s[i+1:], 10, 32)
		if e != nil || v > 1000 || v < -1000 {
			return "", ErrJSON
		}
		exp = v
		s = s[:i]
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		exp -= int64(len(s) - i - 1)
		s = s[:i] + s[i+1:]
	}
	s = strings.TrimLeft(s, "0")
	if s == "" {
		return "0", nil
	}
	n := len(s)
	s = strings.TrimRight(s, "0")
	exp += int64(n - len(s))
	if exp == 0 {
		return sign + s, nil
	}
	return sign + s + "e" + strconv.FormatInt(exp, 10), nil
}
func Canonical(data []byte) ([]byte, error) {
	return CanonicalBounded(data, MaxBytes)
}

// CanonicalBounded retains the canonical algorithm with an explicit byte bound.
func CanonicalBounded(data []byte, maxBytes int64) ([]byte, error) {
	v, e := ParseBounded(data, maxBytes)
	if e != nil {
		return nil, e
	}
	var b bytes.Buffer
	if e = appendValue(&b, v); e != nil {
		return nil, e
	}
	return b.Bytes(), nil
}
func appendValue(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			s, _ := json.Marshal(k)
			b.Write(s)
			b.WriteByte(':')
			if e := appendValue(b, x[k]); e != nil {
				return e
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, v := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if e := appendValue(b, v); e != nil {
				return e
			}
		}
		b.WriteByte(']')
	case json.Number:
		n, e := number(string(x))
		if e != nil {
			return e
		}
		b.WriteString(n)
	default:
		s, e := json.Marshal(v)
		if e != nil {
			return e
		}
		b.Write(s)
	}
	return nil
}
