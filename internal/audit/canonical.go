package audit

import (
	"bytes"
	"crypto/sha256"
)

var hashToken = []byte(`"hash":"`)
var prevHashToken = []byte(`"prev_hash":"`)

// canonicalBytesFromLine searches for hash-shaped tokens without parsing JSON.
// It intentionally accepts a reformat+rehash of the raw line; the normalized
// fallback rejects that class by design.
func canonicalBytesFromLine(line, scratch []byte) ([]byte, []byte, []byte, bool, []byte) {
	for offset := 0; ; {
		i := bytes.Index(line[offset:], hashToken)
		if i < 0 {
			break
		}
		start := offset + i + len(hashToken)
		end := start + 64
		if end < len(line) && line[end] == '"' && lowerHex(line[start:end]) {
			if cap(scratch) < len(line) {
				scratch = make([]byte, len(line))
			} else {
				scratch = scratch[:len(line)]
			}
			copy(scratch, line[:start])
			copy(scratch[start:], line[end:])
			sum := sha256.Sum256(scratch[:len(line)-64])
			if equalHex(sum[:], line[start:end]) {
				prev, ok := prevHashBytesFromLine(line)
				if !ok {
					return nil, nil, nil, false, scratch
				}
				return line[start:end], prev, scratch[:len(line)-64], true, scratch
			}
		}
		offset = start
	}
	return nil, nil, nil, false, scratch
}

func prevHashBytesFromLine(line []byte) ([]byte, bool) {
	var candidate []byte
	found := false
	for offset := 0; ; {
		i := bytes.Index(line[offset:], prevHashToken)
		if i < 0 {
			break
		}
		start := offset + i + len(prevHashToken)
		var value []byte
		valid := false
		if start < len(line) && line[start] == '"' {
			valid = true
		} else if start+64 < len(line) && line[start+64] == '"' && lowerHex(line[start:start+64]) {
			value, valid = line[start:start+64], true
		}
		if valid {
			if found && !bytes.Equal(candidate, value) {
				return nil, false
			}
			candidate, found = value, true
		}
		offset = start
	}
	return candidate, found
}

func lowerHex(value []byte) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func equalHex(value, encoded []byte) bool {
	if len(encoded) != len(value)*2 {
		return false
	}
	const digits = "0123456789abcdef"
	for i, b := range value {
		if encoded[i*2] != digits[b>>4] || encoded[i*2+1] != digits[b&15] {
			return false
		}
	}
	return true
}
