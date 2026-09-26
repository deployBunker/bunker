package fsclient

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"strings"
)

// HashPrefix is the digest's namespace on the wire: `sha256:<64 lowercase
// hex>` over the RAW FILE BYTES (never a git blob hash) — the same digest the
// cache is content-addressed by, the write carries as `If-Match`, and the
// invalidation event names (BFS-005 §5.1: one hash function, one
// representation, three uses).
const HashPrefix = "sha256:"

// HashBytes returns `sha256:<hex>` for b.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return HashPrefix + hex.EncodeToString(sum[:])
}

// NewHasher returns a streaming hasher whose Sum() is in HashBytes' format, so
// a 4 MiB body never has to be materialised to learn its hash.
func NewHasher() hash.Hash { return sha256.New() }

// HashTag formats a raw digest as the tagged value used everywhere on the wire.
func HashTag(sum []byte) string { return HashPrefix + hex.EncodeToString(sum) }

// IsHash reports whether s is a well-formed content hash: the prefix, then
// exactly 64 lowercase hex digits. A malformed value is never repaired.
func IsHash(s string) bool {
	if !strings.HasPrefix(s, HashPrefix) {
		return false
	}
	hexPart := s[len(HashPrefix):]
	if len(hexPart) != 64 {
		return false
	}
	for _, c := range hexPart {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ETagFor quotes a hash as the strong entity-tag BFS-004 §3 E-1 requires: the
// value is `"sha256:<64 hex>"`, quoted, never `W/`-prefixed — `If-Match`
// compares under strong comparison, so an unquoted or weak tag is not a valid
// entity-tag at all.
func ETagFor(hash string) string {
	if hash == "" {
		return ""
	}
	return `"` + hash + `"`
}

// ParseETag normalises an ETag header value to its bare hash. It accepts the
// quoted strong form the surface sends; a weak tag is dropped rather than
// silently upgraded (a weak tag may not be used for `If-Match`).
func ParseETag(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "W/") || strings.HasPrefix(v, "w/") {
		return ""
	}
	v = strings.Trim(v, `"`)
	if !IsHash(v) {
		return ""
	}
	return v
}

// HashReader hashes everything r yields, returning the tagged hash and the
// number of bytes read. Used on both the read path (hashing a fetched body
// before it enters the cache) and the write path (hashing an arriving body).
func HashReader(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", n, err
	}
	return HashTag(h.Sum(nil)), n, nil
}
