package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// GAP-142 command redaction.
//
// The audit chain records WHAT was executed on an agent — the forensic core
// question — but a command line is the single most likely place for a
// credential to appear in the trail (--token X, -H "Authorization: Bearer …",
// API_KEY=…, a bare 40-char hex key, a base64 blob). This file is the ONE
// redaction helper the exec/run command record goes through before any byte of
// it reaches a Record field, mirroring the SEC-08/GAP-133 posture: credentials
// are scrubbed at the point of writing, and the scrubber is fail-closed — a
// value it identifies as credential material is masked, never logged.
//
// The posture is deliberately conservative: redaction may be over-eager (an
// ordinary token that merely LOOKS like a key is masked too), because a masked
// benign value costs a forensic reader one unreadable token, while an
// un-masked credential costs the fleet its secrets.
const (
	// MinSecretLen is the shortest standalone token the blob scan treats as
	// credential material — the "long base64/hex blob (>=20 chars)" rule in
	// docs/exec-audit.md.
	MinSecretLen = 20

	// MinURLSafeBlobLen is the length at which an unseparated [A-Za-z0-9_-]
	// token (a JWT-ish or url-safe key) counts as credential material.
	MinURLSafeBlobLen = 32

	// MaxCommandSummaryLen caps the recorded command summary so a huge argv
	// cannot bloat the hash-chained trail. Truncation happens AFTER redaction,
	// so it can never expose a value the scrubber already masked.
	MaxCommandSummaryLen = 512

	// RedactedPrefix opens every placeholder. The placeholder is
	// shape-preserving — [REDACTED:len14] — so an investigator still learns the
	// value's length (a 14-char flag value is not a 512-byte key) without
	// learning the value.
	RedactedPrefix = "[REDACTED:len"
)

// redactedValue returns the shape-preserving placeholder for a secret value.
// Idempotent: an already-redacted value is returned unchanged, so a value may
// pass through the scrubber more than once (e.g. re-checked before writing,
// mirroring disclosure.go) without nesting placeholders.
func redactedValue(v string) string {
	if isRedacted(v) {
		return v
	}
	return RedactedPrefix + strconv.Itoa(len(v)) + "]"
}

// isRedacted reports whether v already carries a redaction placeholder.
func isRedacted(v string) bool {
	return strings.HasPrefix(v, "[") && strings.Contains(v, "REDACTED")
}

// secretKeyWords are the name fragments that mark a flag, env var, or HTTP
// header as carrying a credential. Matching is word-based (see
// isSecretKeyName): the fragment must be the WHOLE normalized name or one of
// its separator-delimited parts, so "token", "github-token", "API_KEY" and
// "X-Api-Key" match while "monkey" and "keynote" do not.
var secretKeyWords = []string{
	"token", "key", "secret", "password", "passwd", "pwd", "passphrase",
	"credential", "credentials", "auth", "authorization", "apikey", "session",
	"cookie", "signature", "bearer", "sas", "dsn",
}

// secretFlagNames are short flag names whose value is a credential even though
// the name alone is not a secret word (curl -u user:pass, --user bob).
var secretFlagNames = map[string]bool{
	"u": true, "user": true, "pass": true, "pwd": true, "password": true,
	"passwd": true, "token": true, "secret": true, "auth": true,
	"authorization": true, "apikey": true, "api-key": true, "credentials": true,
}

// credentialPrefixes are well-known credential-token prefixes. A token
// carrying one is credential material regardless of length (with a minimum
// length so the bare prefix alone is not masked).
var credentialPrefixes = []string{
	"sk-", "sk_live_", "sk_test_", "rk_live_", "pk_live_", "ghp_", "gho_",
	"ghu_", "ghs_", "github_pat_", "xoxb-", "xoxa-", "xoxp-", "glpat-",
	"AKIA", "ASIA", "AIza", "ya29.", "hf_", "npm_", "dop_v1_", "SG.", "eyJ",
}

// minPrefixedLen is the shortest token that may be masked on a known
// credential prefix alone.
const minPrefixedLen = 12

var (
	// bearerCredentialRe matches a Bearer/Basic credential inside a single
	// token — the shape a credential survives as when an Authorization header
	// is rendered as one whitespace-free token.
	bearerCredentialRe = regexp.MustCompile(`(?i)^(bearer|basic)[ 	]+(.+)$`)

	// hexBlobRe matches an unseparated hex blob (>= MinSecretLen): the shape of
	// an API key, a session id, or a 40-char hex secret.
	hexBlobRe = regexp.MustCompile(`^[0-9a-fA-F]{` + strconv.Itoa(MinSecretLen) + `,}$`)

	// b64BlobRe matches an UNPADDED standard-base64-looking blob.
	b64BlobRe = regexp.MustCompile(`^[A-Za-z0-9+/]{` + strconv.Itoa(MinSecretLen) + `,}$`)

	// paddedBlobRe matches a PADDED base64 blob. The padding is part of the
	// shape and only valid at the end, so the minimum length counts the payload
	// alone: a 20-char payload with '==' padding (22 chars) is still credential
	// material.
	paddedBlobRe = regexp.MustCompile(`^[A-Za-z0-9+/]{` + strconv.Itoa(MinSecretLen) + `,}={1,2}$`)

	// urlSafeBlobRe matches a long url-safe blob (>= MinURLSafeBlobLen): the
	// shape of a JWT segment or a random key with '-'/'_' separators.
	urlSafeBlobRe = regexp.MustCompile(`^[A-Za-z0-9_-]{` + strconv.Itoa(MinURLSafeBlobLen) + `,}$`)

	// jwtRe matches a three-segment JWT regardless of length.
	jwtRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{16,}$`)

	// scriptSummaryRe matches the ONLY script summary shape the recorder emits:
	// the byte count and the SHA-256 digest. Kept in package scope because the
	// write-path re-check (commandSummaryAllowed) uses it to recognize its own
	// output — a hex digest is credential-shaped by design, so it cannot be
	// re-scrubbed.
	scriptSummaryRe = regexp.MustCompile(`^script bytes=[0-9]+ sha256=[0-9a-f]{64}$`)
)

// normalizeValue strips the decoration a rendered command line puts around a
// value: surrounding whitespace and quotes. Lengths reported in a placeholder
// are the value's real length, not its quoted rendering — a credential masked
// as [REDACTED:len12] is 12 characters of secret.
func normalizeValue(v string) string {
	return strings.Trim(strings.TrimSpace(v), `"'`)
}

// normalizeName lowercases a flag/env/header name and strips the decoration
// surrounding it in a command line: leading dashes, surrounding quotes, a
// trailing ':' or '=', and any character outside [a-z0-9_.-].
func normalizeName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Trim(name, `"'`)
	name = strings.TrimLeft(name, "-")
	name = strings.TrimRight(name, ":=")
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isSecretKeyName reports whether a flag/env/header name denotes a credential
// value. Matching is word-based so ordinary names are not swept up.
func isSecretKeyName(name string) bool {
	n := normalizeName(name)
	if n == "" {
		return false
	}
	if secretFlagNames[n] {
		return true
	}
	for _, w := range secretKeyWords {
		if n == w ||
			strings.HasPrefix(n, w+"_") || strings.HasPrefix(n, w+"-") || strings.HasPrefix(n, w+".") ||
			strings.HasSuffix(n, "_"+w) || strings.HasSuffix(n, "-"+w) || strings.HasSuffix(n, "."+w) {
			return true
		}
	}
	return false
}

// isSecretFlag reports whether a token is a command-line flag whose VALUE is
// the next argv element (--token, --api-key, -u, …). A header flag (-H) is not
// one of these: its value is scanned by the header/bearer rules instead.
func isSecretFlag(tok string) bool {
	if !strings.HasPrefix(tok, "-") || len(tok) < 2 {
		return false
	}
	return isSecretKeyName(tok)
}

// splitAssignment splits a KEY=value token. ok is false for a token with no
// '=' or with an empty/whitespace key.
func splitAssignment(tok string) (key, value string, ok bool) {
	i := strings.IndexByte(tok, '=')
	if i <= 0 {
		return "", "", false
	}
	key = tok[:i]
	if strings.ContainsAny(key, " \t") {
		return "", "", false
	}
	return key, tok[i+1:], true
}

// splitHeader splits a Name:value token — a quoted HTTP header survives as one
// whitespace-delimited token. A URL scheme ("https://…") is not a header: the
// "//" value form is rejected.
func splitHeader(tok string) (key, value string, ok bool) {
	i := strings.IndexByte(tok, ':')
	if i <= 0 {
		return "", "", false
	}
	key = tok[:i]
	value = tok[i+1:]
	if strings.HasPrefix(value, "//") || strings.ContainsAny(key, " \t") {
		return "", "", false
	}
	return key, value, true
}

// hasLetterAndDigit reports whether s mixes letters and digits — the property
// that separates a random key from an ordinary long word.
func hasLetterAndDigit(s string) bool {
	var letter, digit bool
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			letter = true
		}
	}
	return letter && digit
}

// isCredentialShaped reports whether a standalone token is credential
// material: a known credential prefix, a JWT, a long hex blob, or a long mixed
// base64/url-safe blob. Ordinary command-line tokens (paths, names, flags,
// prose) are not.
func isCredentialShaped(tok string) bool {
	t := normalizeValue(tok)
	if len(t) < minPrefixedLen {
		return false
	}
	for _, p := range credentialPrefixes {
		if strings.HasPrefix(t, p) && len(t) >= len(p)+8 {
			return true
		}
	}
	if jwtRe.MatchString(t) {
		return true
	}
	if len(t) < MinSecretLen {
		return false
	}
	if hexBlobRe.MatchString(t) {
		return true
	}
	if b64BlobRe.MatchString(t) || paddedBlobRe.MatchString(t) || urlSafeBlobRe.MatchString(t) {
		return hasLetterAndDigit(t)
	}
	return false
}

// isBearerPhrase reports whether the joined token is a bare Bearer/Basic
// keyword, or that keyword followed by nothing but the credential itself.
// Quoting splits a header into whitespace-delimited pieces, so
// 'Authorization: Bearer abc123' arrives as [Authorization:, Bearer, abc123]
// and the only rendering kept here is the credential.
func isBearerPhrase(s string) bool {
	s = normalizeValue(s)
	if isBareCredentialKeyword(s) {
		return true
	}
	parts := strings.Fields(s)
	if len(parts) == 2 && isBareCredentialKeyword(parts[0]) {
		return true
	}
	return false
}

// isCredentialValue reports whether the value half of a NAME=value or
// NAME:value pair is credential material. Beyond the standalone token shapes,
// a value that IS a credential header ('Authorization: Bearer x', a quoted
// 'session=…' cookie) is credential material too — that is the shape an
// assignment to a non-secret name smuggles a credential in.
func isCredentialValue(value string) bool {
	switch v := normalizeValue(value); {
	case v == "":
		return false
	case isSecretKeyName(v):
		return true
	case isCredentialShaped(v):
		return true
	case isBearerPhrase(v):
		return true
	}
	parts := strings.Fields(normalizeValue(value))
	if len(parts) >= 2 {
		if k, _, ok := splitHeader(parts[0]); ok && isSecretKeyName(k) {
			return true
		}
		if k, _, ok := splitAssignment(parts[0]); ok && isSecretKeyName(k) {
			return true
		}
		return isCredentialShaped(parts[len(parts)-1])
	}
	return false
}

// isBareCredentialKeyword reports whether a token is a standalone credential
// keyword whose NEXT argv element is the credential (a quoted
// "Authorization: Bearer abc123" splits into the header token, this keyword,
// and the value).
func isBareCredentialKeyword(tok string) bool {
	switch strings.ToLower(strings.Trim(tok, `"'`)) {
	case "bearer", "basic":
		return true
	}
	return false
}

// isNameToken reports whether a candidate key looks like a NAME rather than an
// arbitrary blob: plausible name characters and a bounded length. It stops a
// padded base64 value ('AAAA…==') from being read as an assignment to a key
// named 'AAAA…'.
func isNameToken(key string) bool {
	if key == "" || len(key) > 64 {
		return false
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// maskNextCredential masks the credential at tokens[idx] in place, stepping past
// bare credential keywords first (the keyword is not the secret; the value after
// it is). It returns the index of the last token it touched so the caller
// resumes the scan AFTER it — every branch resumes rather than continues, so a
// masked element can never shift a still-visible element out of the loop.
func maskNextCredential(tokens []string, idx int) int {
	for idx < len(tokens) && isBareCredentialKeyword(tokens[idx]) {
		idx++
	}
	if idx >= len(tokens) {
		return idx - 1
	}
	tokens[idx] = redactedValue(normalizeValue(tokens[idx]))
	return idx
}

// redactValueRun redacts one VALUE half (the text after NAME= or NAME:) and
// reports how many tokens the run spans BEYOND the current one. Rendering is
// explicit rather than a re-join of the raw text, because the pieces differ in
// kind:
//
//   - the LAST element is the credential when it is an actual value (a JWT, a
//     blob, 'session=abc'), and is masked with its real length;
//   - when the last element IS a credential keyword ('Authorization: Bearer') or
//     an empty rendering (a lone quote), the credential did not fit inside the
//     value — it is the NEXT token: atEnd is false and the caller masks the
//     token after the run. Masking the value here instead would write a
//     placeholder in place of nothing while the real secret stayed visible in
//     the argv.
//
// extraTokens is len(parts)-1: the run's own text lives inside the current
// token, so only additional whitespace-delimited pieces occupy later tokens.
// With the pair at index i the run ends at i+extraTokens and the scan resumes at
// i+extraTokens (atEnd) or masks the token at i+extraTokens+1 (not atEnd).
func redactValueRun(value string) (redacted string, extraTokens int, atEnd bool) {
	parts := strings.Fields(value)
	if len(parts) == 0 {
		return "", 0, false // empty value: the credential is the next token
	}
	rendered := make([]string, 0, len(parts))
	for _, p := range parts[:len(parts)-1] {
		rendered = append(rendered, redactToken(p))
	}
	last := parts[len(parts)-1]
	if normalizeValue(last) == "" {
		// A bare quote: the value carries nothing, so the credential is next.
		return strings.Join(rendered, " "), len(parts) - 1, false
	}
	if isBareCredentialKeyword(last) || isSecretKeyName(last) {
		rendered = append(rendered, last)
		return strings.Join(rendered, " "), len(parts) - 1, false
	}
	rendered = append(rendered, redactedValue(normalizeValue(last)))
	return strings.Join(rendered, " "), len(parts) - 1, true
}

// redactCommandLine scrubs a rendered command line, returning it unchanged
// (byte-for-byte, including original spacing) when nothing matched so an
// ordinary command's summary is exactly what the caller wrote. Rules, applied
// per whitespace-delimited token:
//
//  1. NAME=value whose name is a credential -> mask the value (any shape).
//  2. NAME=value whose VALUE is credential material -> redact the value run
//     (this is how an assignment to a non-secret name smuggles a header in).
//  3. NAME:value whose name is a credential (a quoted HTTP header) -> redact the
//     value run, or the following argv element when the value is empty or ends
//     at a bare keyword.
//  4. a credential flag (--token, --api-key, -u, …) -> mask the NEXT element.
//  5. a bare Bearer/Basic keyword -> mask the NEXT element.
//  6. a Bearer/Basic credential inside one token -> mask it in place.
//  7. a standalone credential-shaped token -> mask it wholesale.
//
// A token that only LOOKS like an assignment (a padded base64 blob contains '=')
// falls through to the remaining rules instead of being skipped.
func redactCommandLine(line string) string {
	tokens := strings.Fields(line)
	if len(tokens) == 0 {
		return ""
	}
	changed := false
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]

		if key, value, ok := splitAssignment(tok); ok && isNameToken(key) {
			switch {
			case isSecretKeyName(key):
				tokens[i] = key + "=" + redactedValue(value)
				changed = true
				continue
			case isCredentialValue(value):
				run, extra, atEnd := redactValueRun(value)
				tokens[i] = key + "=" + run
				changed = true
				if atEnd {
					i += extra
				} else {
					i = maskNextCredential(tokens, i+extra+1)
				}
				continue
			}
			// Not credential material after all: fall through.
		}

		if key, value, ok := splitHeader(tok); ok && isSecretKeyName(key) {
			// Preserve the caller's own rendering ('Authorization: Bearer …' vs
			// 'Authorization:Bearer…') and replace only the credential itself.
			// The surrounding quotes go: a redacted header is no longer the
			// caller's byte string, and leaving an unbalanced opening quote
			// behind reads as corruption. An ordinary header is untouched (it
			// never reaches this branch).
			key = strings.Trim(key, `"'`)
			run, extra, atEnd := redactValueRun(value)
			if strings.HasPrefix(value, " ") {
				tokens[i] = key + ":" + " " + run
			} else {
				tokens[i] = key + ":" + run
			}
			changed = true
			if atEnd {
				i += extra
			} else {
				i = maskNextCredential(tokens, i+extra+1)
			}
			continue
		}

		if isSecretFlag(tok) {
			i = maskNextCredential(tokens, i+1)
			changed = true
			continue
		}

		if isBareCredentialKeyword(tok) {
			i = maskNextCredential(tokens, i)
			changed = true
			continue
		}

		if r := redactToken(tok); r != tok {
			tokens[i] = r
			changed = true
		}
	}
	if !changed {
		// Nothing matched: return the caller's text verbatim so an ordinary
		// command is recorded exactly as issued (spacing included).
		return strings.TrimSpace(line)
	}
	return strings.Join(tokens, " ")
}

// redactToken scrubs one token that has no key/flag context: a Bearer/Basic
// credential is masked in place, and a standalone credential-shaped token is
// replaced wholesale.
func redactToken(tok string) string {
	if m := bearerCredentialRe.FindStringSubmatch(tok); m != nil {
		return m[1] + " " + redactedValue(normalizeValue(m[2]))
	}
	if isCredentialShaped(tok) {
		return redactedValue(normalizeValue(tok))
	}
	return tok
}

// commandLine renders a command plus its argv as one line, preserving argument
// order. It is the exact text handed to redactCommandLine.
func commandLine(command string, args []string) string {
	if len(args) == 0 {
		return strings.TrimSpace(command)
	}
	return strings.TrimSpace(command + " " + strings.Join(args, " "))
}

// RedactCommandSummary renders a command + argv into the redacted summary text
// recorded in the audit chain. Exported so the daemon (and tests) can assert
// exactly what a given command line will look like in the trail without
// writing a record.
func RedactCommandSummary(command string, args []string) string {
	return capCommandSummary(redactCommandLine(commandLine(command, args)))
}

// RedactScriptSummary summarizes an uploaded script body. A script is
// arbitrary multi-line content and is NOT token-scanned like a command line:
// recording it is both unsafe (it is the most likely carrier of an embedded
// credential) and unreadable (multi-line content in a one-line record). What
// is recorded instead is the script's size plus its SHA-256 digest — enough to
// correlate the audit record with the script a caller supplied and to prove
// two records describe the same script, with nothing secret in the trail.
// Stricter than the command rule on purpose; see docs/exec-audit.md.
func RedactScriptSummary(script string) string {
	return capCommandSummary(fmt.Sprintf("script bytes=%d sha256=%s", len(script), scriptDigest(script)))
}

// scriptDigest is the SHA-256 hex digest of a script body.
func scriptDigest(script string) string {
	sum := sha256.Sum256([]byte(script))
	return hex.EncodeToString(sum[:])
}

// capCommandSummary bounds a summary line to MaxCommandSummaryLen bytes,
// cutting on a rune boundary and marking the truncation.
func capCommandSummary(s string) string {
	if len(s) <= MaxCommandSummaryLen {
		return s
	}
	cut, total := 0, 0
	for i, r := range s {
		total += len(string(r))
		if total > MaxCommandSummaryLen {
			break
		}
		cut = i + 1
	}
	return s[:cut] + " …(truncated)"
}
