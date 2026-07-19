# Package: `internal/tlsutil`

## Public API

- `SelfSignedOptions` — configuration for self-signed certificate generation: Hosts, Years.
- `GenerateSelfSignedCert(certPath, keyPath, opts)` — generates an ECDSA P-256 self-signed certificate, writes PEM-encoded cert and key files.
- `LoadOrGenerateSelfSigned(certPath, keyPath, hosts)` — loads existing cert/key or generates new ones; returns a `*tls.Config` ready for `net/http.Server`.

## Conventions

- Uses ECDSA P-256 (`elliptic.P256()`) — faster key generation than RSA, smaller keys.
- Certificate validity defaults to 1 year (configurable via `opts.Years`).
- `NotBefore` is set 1 hour in the past to account for clock skew.
- IP addresses in `Hosts` go to `IPAddresses`; hostnames go to `DNSNames`.
- Cert files are written with mode `0644`; key files with `0600`.
- Parent directories are created automatically (`os.MkdirAll`).

## Dependencies

- Standard library: `crypto/ecdsa`, `crypto/elliptic`, `crypto/rand`, `crypto/tls`, `crypto/x509`, `crypto/x509/pkix`, `encoding/pem`, `fmt`, `math/big`, `net`, `os`, `path/filepath`, `time`.

## Test Patterns

- `certgen_test.go`: verifies cert generation succeeds, cert is loadable, key matches, SANs include requested hosts, LoadOrGenerateSelfSigned idempotency (second call doesn't regenerate if files exist), missing key triggers regeneration.
- Tests write to temp directories and clean up.

## Pitfalls

1. **Self-signed certs are NOT trusted by default.** Browsers and curl will reject them unless `--insecure` / `InsecureSkipVerify` is used. Use certmagic for production Let's Encrypt certs.
2. **Key file permissions are critical.** `GenerateSelfSignedCert` writes the key with `0600`. If anything changes this, TLS loading will fail with a permissions error.
3. **`LoadOrGenerateSelfSigned` regenerates if EITHER file is missing.** A missing key with an existing cert (or vice versa) triggers full regeneration — the old cert becomes orphaned.
4. **Clock skew handling is minimal.** `NotBefore` is 1 hour in the past. For systems with larger clock drift, increase this buffer.
