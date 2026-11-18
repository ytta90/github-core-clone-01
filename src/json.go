package main

import (
	"strconv"
	"strings"
)

// Minimal JSON value model: nil, bool, float64, string, []any, map[string]any.
// Numbers that arrive as integers are kept as float64; jnum converts.

// parseJSON parses a complete JSON document. It returns nil on any error.
func parseJSON(b []byte) any {
	p := &jparser{b: b}
	p.ws()
	v := p.value()
	if v == nil && !p.ok {
		return nil
	}
	p.ws()
	if p.i != len(p.b) {
		return nil
	}
	return v
}

type jparser struct {
	b  []byte
	i  int
	ok bool
}

func (p *jparser) ws() {
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *jparser) value() any {
	p.ok = true
	if p.i >= len(p.b) {
		p.ok = false
		return nil
	}
	switch c := p.b[p.i]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		s, ok := p.str()
		if !ok {
			p.ok = false
			return nil
		}
		return s
	case c == 't':
		if p.lit("true") {
			return true
		}
	case c == 'f':
		if p.lit("false") {
			return false
		}
	case c == 'n':
		if p.lit("null") {
			return nil
		}
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	}
	p.ok = false
	return nil
}

func (p *jparser) lit(s string) bool {
	if len(p.b)-p.i < len(s) || string(p.b[p.i:p.i+len(s)]) != s {
		return false
	}
	p.i += len(s)
	return true
}

func (p *jparser) number() any {
	start := p.i
	if p.i < len(p.b) && p.b[p.i] == '-' {
		p.i++
	}
	for p.i < len(p.b) {
		c := p.b[p.i]
		if (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
			p.i++
		} else {
			break
		}
	}
	f, err := strconv.ParseFloat(string(p.b[start:p.i]), 64)
	if err != nil {
		p.ok = false
		return nil
	}
	return f
}

func (p *jparser) str() (string, bool) {
	// p.b[p.i] == '"'
	p.i++
	var sb strings.Builder
	for p.i < len(p.b) {
		c := p.b[p.i]
		if c == '"' {
			p.i++
			return sb.String(), true
		}
		if c == '\\' {
			p.i++
			if p.i >= len(p.b) {
				return "", false
			}
			e := p.b[p.i]
			p.i++
			switch e {
			case '"':
				sb.WriteByte('"')
			case '\\':
				sb.WriteByte('\\')
			case '/':
				sb.WriteByte('/')
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			case 'u':
				if p.i+4 > len(p.b) {
					return "", false
				}
				n, err := strconv.ParseUint(string(p.b[p.i:p.i+4]), 16, 32)
				if err != nil {
					return "", false
				}
				p.i += 4
				if n >= 0xD800 && n <= 0xDBFF {
					// surrogate pair
					if p.i+6 <= len(p.b) && p.b[p.i] == '\\' && p.b[p.i+1] == 'u' {
						n2, err := strconv.ParseUint(string(p.b[p.i+2:p.i+6]), 16, 32)
						if err == nil && n2 >= 0xDC00 && n2 <= 0xDFFF {
							p.i += 6
							sb.WriteRune(rune(0x10000 + ((n - 0xD800) << 10) + (n2 - 0xDC00)))
							continue
						}
					}
					sb.WriteRune(0xFFFD)
					continue
				}
				sb.WriteRune(rune(n))
			default:
				return "", false
			}
			continue
		}
		if c < 0x20 {
			return "", false
		}
		sb.WriteByte(c)
		p.i++
	}
	return "", false
}

func (p *jparser) object() any {
	p.i++ // {
	m := make(map[string]any)
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == '}' {
		p.i++
		return m
	}
	for {
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != '"' {
			p.ok = false
			return nil
		}
		k, ok := p.str()
		if !ok {
			p.ok = false
			return nil
		}
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != ':' {
			p.ok = false
			return nil
		}
		p.i++
		p.ws()
		v := p.value()
		if !p.ok {
			return nil
		}
		m[k] = v
		p.ws()
		if p.i < len(p.b) && p.b[p.i] == ',' {
			p.i++
			continue
		}
		if p.i < len(p.b) && p.b[p.i] == '}' {
			p.i++
			return m
		}
		p.ok = false
		return nil
	}
}

func (p *jparser) array() any {
	p.i++ // [
	a := []any{}
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == ']' {
		p.i++
		return a
	}
	for {
		p.ws()
		v := p.value()
		if !p.ok {
			return nil
		}
		a = append(a, v)
		p.ws()
		if p.i < len(p.b) && p.b[p.i] == ',' {
			p.i++
			continue
		}
		if p.i < len(p.b) && p.b[p.i] == ']' {
			p.i++
			return a
		}
		p.ok = false
		return nil
	}
}

// ---- readers ----

func jobj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func jarr(v any) []any {
	a, _ := v.([]any)
	return a
}

func jstr(v any) string {
	s, _ := v.(string)
	return s
}

func jbool(v any, def bool) bool {
	b, ok := v.(bool)
	if !ok {
		return def
	}
	return b
}

func jnum(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

// ---- builder ----

// jb builds JSON by hand. Zero-allocation for numbers via strconv.
type jb struct {
	b []byte
}

func (j *jb) reset() { j.b = j.b[:0] }

func (j *jb) raw(s string) { j.b = append(j.b, s...) }

func (j *jb) bytes(p []byte) { j.b = append(j.b, p...) }

func (j *jb) str(s string) {
	j.b = append(j.b, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			j.b = append(j.b, '\\', '"')
		case '\\':
			j.b = append(j.b, '\\', '\\')
		case '\n':
			j.b = append(j.b, '\\', 'n')
		case '\r':
			j.b = append(j.b, '\\', 'r')
		case '\t':
			j.b = append(j.b, '\\', 't')
		case '\b':
			j.b = append(j.b, '\\', 'b')
		case '\f':
			j.b = append(j.b, '\\', 'f')
		default:
			if c < 0x20 {
				j.b = append(j.b, '\\', 'u', '0', '0')
				const hex = "0123456789abcdef"
				j.b = append(j.b, hex[c>>4], hex[c&0xf])
			} else {
				j.b = append(j.b, c)
			}
		}
	}
	j.b = append(j.b, '"')
}

func (j *jb) num(n int) {
	j.b = strconv.AppendInt(j.b, int64(n), 10)
}

func (j *jb) boolean(v bool) {
	if v {
		j.b = append(j.b, "true"...)
	} else {
		j.b = append(j.b, "false"...)
	}
}

// quoteJSON escapes s for embedding inside a JSON string literal (without quotes).
func quoteJSON(s string) string {
	var j jb
	j.str(s)
	return string(j.b[1 : len(j.b)-1])
}
