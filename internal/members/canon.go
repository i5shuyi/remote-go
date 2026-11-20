package members

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// Canonical JSON (RFC 8785) restricted to what records use: objects,
// arrays, strings, booleans, null and integers within ±(2^53−1).

var errDupKey = errors.New("重复的 JSON 键")

var intRE = regexp.MustCompile(`^(0|-?[1-9][0-9]*)$`)

// canonicalize parses strict JSON and returns its canonical form.
func canonicalize(b []byte) ([]byte, error) {
	if !utf8.Valid(b) {
		return nil, errors.New("JSON 不是有效的 UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	v, err := parseValue(d)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("JSON 之后有多余数据")
	}
	var out bytes.Buffer
	writeCanon(&out, v)
	return out.Bytes(), nil
}

type object struct {
	keys []string
	vals map[string]any
}

func parseValue(d *json.Decoder) (any, error) {
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t := t.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := object{vals: map[string]any{}}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return nil, err
				}
				ks := k.(string)
				if _, dup := o.vals[ks]; dup {
					return nil, fmt.Errorf("%w %q", errDupKey, ks)
				}
				v, err := parseValue(d)
				if err != nil {
					return nil, err
				}
				o.keys = append(o.keys, ks)
				o.vals[ks] = v
			}
			_, err := d.Token()
			return o, err
		case '[':
			var a []any
			for d.More() {
				v, err := parseValue(d)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			}
			_, err := d.Token()
			return a, err
		}
		return nil, errors.New("JSON 结构无效")
	case json.Number:
		if !intRE.MatchString(string(t)) {
			return nil, fmt.Errorf("只允许整数：%s", t)
		}
		n, err := strconv.ParseInt(string(t), 10, 64)
		if err != nil || n > 1<<53-1 || n < -(1<<53-1) {
			return nil, fmt.Errorf("整数越界：%s", t)
		}
		return n, nil
	default:
		return t, nil // string, bool, nil
	}
}

func utf16Less(a, b string) int {
	return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b)))
}

func writeCanon(w *bytes.Buffer, v any) {
	switch v := v.(type) {
	case object:
		keys := slices.Clone(v.keys)
		slices.SortFunc(keys, utf16Less)
		w.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				w.WriteByte(',')
			}
			writeString(w, k)
			w.WriteByte(':')
			writeCanon(w, v.vals[k])
		}
		w.WriteByte('}')
	case []any:
		w.WriteByte('[')
		for i, e := range v {
			if i > 0 {
				w.WriteByte(',')
			}
			writeCanon(w, e)
		}
		w.WriteByte(']')
	case string:
		writeString(w, v)
	case int64:
		w.WriteString(strconv.FormatInt(v, 10))
	case bool:
		w.WriteString(strconv.FormatBool(v))
	case nil:
		w.WriteString("null")
	}
}

// writeString follows ECMAScript JSON.stringify, as RFC 8785 requires.
func writeString(w *bytes.Buffer, s string) {
	const hex = "0123456789abcdef"
	w.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			w.WriteString(`\"`)
		case '\\':
			w.WriteString(`\\`)
		case '\b':
			w.WriteString(`\b`)
		case '\f':
			w.WriteString(`\f`)
		case '\n':
			w.WriteString(`\n`)
		case '\r':
			w.WriteString(`\r`)
		case '\t':
			w.WriteString(`\t`)
		default:
			if r < 0x20 {
				w.WriteString(`\u00`)
				w.WriteByte(hex[r>>4])
				w.WriteByte(hex[r&15])
			} else {
				w.WriteRune(r)
			}
		}
	}
	w.WriteByte('"')
}

// marshalWire encodes v without HTML escaping or re-indenting, so the
// canonical record bodies it carries pass through byte for byte.
func marshalWire(v any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// marshalCanon encodes v and returns its canonical form.
func marshalCanon(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return canonicalize(b)
}

// decodeStrict decodes canonical bytes into v, rejecting unknown fields
// and any body that does not re-encode to exactly the same bytes (missing
// fields, explicit defaults, non-canonical input).
func decodeStrict(b []byte, v any) error {
	c, err := canonicalize(b)
	if err != nil {
		return err
	}
	if !bytes.Equal(c, b) {
		return errors.New("记录体不是规范 JSON")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	again, err := marshalCanon(v)
	if err != nil {
		return err
	}
	if !bytes.Equal(again, b) {
		return errors.New("记录体含缺失或非法字段")
	}
	return nil
}
