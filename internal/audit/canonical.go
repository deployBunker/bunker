package audit

import (
	"encoding/json"
)

type recordLink struct {
	Hash     string `json:"hash"`
	PrevHash string `json:"prev_hash"`
}

// canonicalBytesFromLine finds the top-level hash member and blanks its value.
// This intentionally accepts reformatted JSON when its raw-byte hash matches:
// the chain is unkeyed tamper evidence, and recomputing a hash is not an
// integrity control. The old normalized fallback rejects that reformat+rehash
// class; this fast path can accept it by design.
func canonicalBytesFromLine(line []byte, declared string) ([]byte, bool) {
	if !json.Valid(line) {
		return nil, false
	}
	return canonicalBytesFromValidLine(line, declared)
}

// canonicalBytesFromValidLine derives bytes after the caller has decoded the
// minimal link struct, which also proves that the complete line is valid JSON.
func canonicalBytesFromValidLine(line []byte, declared string) ([]byte, bool) {
	if len(declared) != 64 {
		return nil, false
	}
	for _, c := range declared {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return nil, false
		}
	}
	start, end, count := -1, -1, 0
	pos := skipJSONSpace(line, 0)
	if pos >= len(line) || line[pos] != '{' {
		return nil, false
	}
	pos++
	for {
		pos = skipJSONSpace(line, pos)
		if pos >= len(line) || line[pos] == '}' {
			break
		}
		keyStart := pos
		keyEnd := jsonStringEnd(line, pos)
		if keyEnd < 0 {
			return nil, false
		}
		var key string
		if json.Unmarshal(line[keyStart:keyEnd], &key) != nil {
			return nil, false
		}
		pos = skipJSONSpace(line, keyEnd)
		if pos >= len(line) || line[pos] != ':' {
			return nil, false
		}
		pos = skipJSONSpace(line, pos+1)
		if key == "hash" {
			count++
			if pos >= len(line) || line[pos] != '"' {
				return nil, false
			}
			valueEnd := jsonStringEnd(line, pos)
			if valueEnd < 0 {
				return nil, false
			}
			var value string
			if json.Unmarshal(line[pos:valueEnd], &value) != nil || value != declared || len(value) != 64 {
				return nil, false
			}
			start, end = pos+1, valueEnd-1
			pos = valueEnd
		} else {
			var ok bool
			pos, ok = skipJSONValue(line, pos)
			if !ok {
				return nil, false
			}
		}
		pos = skipJSONSpace(line, pos)
		if pos < len(line) && line[pos] == ',' {
			pos++
			continue
		}
		if pos < len(line) && line[pos] == '}' {
			break
		}
		return nil, false
	}
	if count != 1 || start < 0 || end-start != 64 {
		return nil, false
	}
	canonical := make([]byte, 0, len(line)-64)
	canonical = append(canonical, line[:start]...)
	canonical = append(canonical, line[end:]...)
	return canonical, true
}

func skipJSONSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\n' || b[i] == '\r' || b[i] == '\t') {
		i++
	}
	return i
}

// jsonStringEnd returns the byte offset immediately after a JSON string.
func jsonStringEnd(b []byte, i int) int {
	if i >= len(b) || b[i] != '"' {
		return -1
	}
	for j := i + 1; j < len(b); j++ {
		switch b[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return -1
}

func skipJSONValue(b []byte, i int) (int, bool) {
	i = skipJSONSpace(b, i)
	if i >= len(b) {
		return i, false
	}
	if b[i] == '"' {
		end := jsonStringEnd(b, i)
		return end, end >= 0
	}
	if b[i] == '{' || b[i] == '[' {
		open, close := b[i], byte('}')
		if open == '[' {
			close = ']'
		}
		depth := 0
		for j := i; j < len(b); j++ {
			if b[j] == '"' {
				end := jsonStringEnd(b, j)
				if end < 0 {
					return i, false
				}
				j = end - 1
				continue
			}
			if b[j] == open {
				depth++
			} else if b[j] == close {
				depth--
				if depth == 0 {
					return j + 1, true
				}
			}
		}
		return i, false
	}
	for i < len(b) && b[i] != ',' && b[i] != '}' && b[i] != ']' {
		i++
	}
	return i, i > 0
}
