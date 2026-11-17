// Package miniyaml parses the small YAML subset used by Glasshouse policy files:
// nested maps (2+ space indentation), lists of scalars ("- x" or "[a, b]"),
// quoted/unquoted scalars, booleans, integers and '#' comments.
// Anything outside that subset is a hard error, never silently ignored.
package miniyaml

import (
	"fmt"
	"strconv"
	"strings"
)

type line struct {
	no     int
	indent int
	text   string
}

// Parse returns map[string]any, []any, string, int64 or bool values.
func Parse(src string) (map[string]any, error) {
	var lines []line
	for i, raw := range strings.Split(src, "\n") {
		raw = strings.TrimRight(raw, "\r")
		if strings.Contains(raw, "\t") && strings.TrimLeft(raw, " \t") != strings.TrimLeft(raw, " ") {
			return nil, fmt.Errorf("line %d: tabs are not allowed for indentation", i+1)
		}
		text := stripComment(raw)
		if strings.TrimSpace(text) == "" {
			continue
		}
		indent := len(text) - len(strings.TrimLeft(text, " "))
		lines = append(lines, line{no: i + 1, indent: indent, text: strings.TrimSpace(text)})
	}
	if len(lines) == 0 {
		return map[string]any{}, nil
	}
	if lines[0].indent != 0 {
		return nil, fmt.Errorf("line %d: unexpected indentation", lines[0].no)
	}
	m, next, err := parseMap(lines, 0, 0)
	if err != nil {
		return nil, err
	}
	if next != len(lines) {
		return nil, fmt.Errorf("line %d: unexpected content", lines[next].no)
	}
	return m, nil
}

func stripComment(s string) string {
	var q rune
	for i, r := range s {
		switch {
		case q != 0:
			if r == q {
				q = 0
			}
		case r == '"' || r == '\'':
			q = r
		case r == '#' && (i == 0 || s[i-1] == ' '):
			return s[:i]
		}
	}
	return s
}

func parseMap(ls []line, i, indent int) (map[string]any, int, error) {
	m := map[string]any{}
	for i < len(ls) {
		l := ls[i]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, 0, fmt.Errorf("line %d: unexpected indentation", l.no)
		}
		if strings.HasPrefix(l.text, "- ") || l.text == "-" {
			return nil, 0, fmt.Errorf("line %d: list item where a key was expected", l.no)
		}
		key, val, ok := splitKey(l.text)
		if !ok {
			return nil, 0, fmt.Errorf("line %d: expected 'key: value'", l.no)
		}
		if _, dup := m[key]; dup {
			return nil, 0, fmt.Errorf("line %d: duplicate key %q", l.no, key)
		}
		i++
		if val != "" {
			v, err := scalarOrInline(val, l.no)
			if err != nil {
				return nil, 0, err
			}
			m[key] = v
			continue
		}
		// block value: nested map, list, or empty
		if i < len(ls) && ls[i].indent > indent {
			child := ls[i].indent
			if strings.HasPrefix(ls[i].text, "- ") || ls[i].text == "-" {
				lst, next, err := parseList(ls, i, child)
				if err != nil {
					return nil, 0, err
				}
				m[key], i = lst, next
			} else {
				sub, next, err := parseMap(ls, i, child)
				if err != nil {
					return nil, 0, err
				}
				m[key], i = sub, next
			}
		} else if i < len(ls) && ls[i].indent == indent && (strings.HasPrefix(ls[i].text, "- ")) {
			lst, next, err := parseList(ls, i, indent)
			if err != nil {
				return nil, 0, err
			}
			m[key], i = lst, next
		} else {
			m[key] = ""
		}
	}
	return m, i, nil
}

func parseList(ls []line, i, indent int) ([]any, int, error) {
	out := []any{}
	for i < len(ls) {
		l := ls[i]
		if l.indent != indent || !(strings.HasPrefix(l.text, "- ") || l.text == "-") {
			break
		}
		item := strings.TrimSpace(strings.TrimPrefix(l.text, "-"))
		if item == "" {
			return nil, 0, fmt.Errorf("line %d: empty list item", l.no)
		}
		v, err := scalarOrInline(item, l.no)
		if err != nil {
			return nil, 0, err
		}
		if _, isMap := v.(map[string]any); isMap {
			return nil, 0, fmt.Errorf("line %d: lists of maps are not supported", l.no)
		}
		out = append(out, v)
		i++
	}
	return out, i, nil
}

// splitKey splits "key: value" at the first ": " (or a trailing ':'),
// ignoring colons inside quotes.
func splitKey(s string) (key, val string, ok bool) {
	var q rune
	for i, r := range s {
		switch {
		case q != 0:
			if r == q {
				q = 0
			}
		case r == '"' || r == '\'':
			q = r
		case r == ':' && (i+1 == len(s) || s[i+1] == ' '):
			k := strings.TrimSpace(s[:i])
			if k == "" {
				return "", "", false
			}
			return unquote(k), strings.TrimSpace(s[i+1:]), true
		}
	}
	return "", "", false
}

func scalarOrInline(s string, no int) (any, error) {
	if strings.HasPrefix(s, "[") {
		if !strings.HasSuffix(s, "]") {
			return nil, fmt.Errorf("line %d: unterminated inline list", no)
		}
		inner := strings.TrimSpace(s[1 : len(s)-1])
		out := []any{}
		if inner == "" {
			return out, nil
		}
		for _, part := range splitInline(inner) {
			v, err := scalar(strings.TrimSpace(part), no)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	if strings.HasPrefix(s, "{") {
		return nil, fmt.Errorf("line %d: inline maps are not supported", no)
	}
	return scalar(s, no)
}

func splitInline(s string) []string {
	var parts []string
	var q rune
	start := 0
	for i, r := range s {
		switch {
		case q != 0:
			if r == q {
				q = 0
			}
		case r == '"' || r == '\'':
			q = r
		case r == ',':
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

func scalar(s string, no int) (any, error) {
	if s == "" {
		return "", nil
	}
	if s[0] == '"' || s[0] == '\'' {
		if len(s) < 2 || s[len(s)-1] != s[0] {
			return nil, fmt.Errorf("line %d: unterminated quoted string", no)
		}
		return unquote(s), nil
	}
	switch s {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	return s, nil
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}
