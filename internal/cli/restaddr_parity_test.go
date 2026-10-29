// restaddr_parity_test.go — QA-BUNKER-74 client-side rest_addr parity pin.
//
// The row's client half: "the client dial path derives from that same
// field [server.rest_addr] rather than an independent loopback default."
// The contract, end to end:
//
//	config.example.yaml server.rest_addr  (the daemon binds this)
//	        │  documented recipe: bunker connect http://<rest_addr>
//	        ▼
//	CLI registry entry  (~/.bunker|~/.config/bunker/config.yaml, ServerEntry.URL)
//	        │  newBunkerdClient / newBunkerdClientChecked dial entry.URL
//	        ▼
//	daemon answers on rest_addr — never a silent timeout
//
// The silent-unreachable class this pins (off-by-one corpus answer 2759):
// SEC-001 moved daemons to Tailscale IPs, client configs kept the loopback
// literal from the old example, and every dial failed DETERMINISTICALLY
// ("connection refused") — indistinguishable, to fail-closed consumers like
// scripts/root-suite.sh's classifier, from a real daemon outage.
//
// Two tests:
//
//  1. TestClientRegistryURLMatchesExampleRESTAddr — the registry round-trip:
//     an entry built from the example's rest_addr survives
//     SaveCLIConfig/LoadCLIConfig byte-for-byte (server names keep case since
//     DF-BUNKER-1; this pins the URL too), so nothing on the client storage
//     path rewrites a non-loopback dial address back to loopback.
//
//  2. TestClientDialReachesRESTAddr — the live dial. When the host can bind
//     the example's rest_addr (a tailnet host can — that is the point), the
//     production dial path reaches a real connect-go server mounted exactly
//     there: a dial to rest_addr SUCCEEDS, it does not silently time out
//     (DF-BUNKER-113's recorded failure signature). Where the placeholder
//     cannot be bound (no tailscale on the test host), the same dial
//     mechanics are proven against a loopback httptest listener so the
//     transport path stays covered on every host; the placeholder leg is the
//     fleet-host parity proof and skips with a named reason.
//
// The example-shape half of the pin (the example must SHOW a reachable
// rest_addr, not the loopback literal) lives with the example's own tests:
// internal/config/example_bind_discipline_test.go.
package cli

import (
	"context"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
	"github.com/go-chi/chi/v5"

	"github.com/deployBunker/bunker/internal/config"
)

// parityExampleConfigPath is the shipped example at the repo root — the file
// the row's acceptance criteria are written against. `go test` runs with the
// package directory as cwd, so the repo root is two levels up. The
// BUNKER_EXAMPLE_CONFIG_PATH override is the same RED-proof seam the
// internal/config example tests honour: point it at a pre-fix revision and
// these tests fail, without overwriting the working-tree file.
func parityExampleConfigPath() string {
	if p := os.Getenv("BUNKER_EXAMPLE_CONFIG_PATH"); p != "" {
		return p
	}
	return "../../config.example.yaml"
}

// parityExampleRESTAddr is what the example documents until the host's real
// tailnet address is substituted; the tests derive from the file, and this
// constant only names the expected placeholder in failure messages (kept in
// step with internal/config/example_bind_discipline_test.go).
const parityExampleRESTAddr = "100.64.0.1:8080"

// parityRESTAddr loads the shipped example and returns its rest_addr,
// requiring it to be a concrete, non-loopback host:port — the derivation
// source for client registry entries.
func parityRESTAddr(t *testing.T) string {
	t.Helper()
	cfg, err := config.Load(parityExampleConfigPath())
	if err != nil {
		t.Fatalf("load %s: %v", parityExampleConfigPath(), err)
	}
	addr := cfg.Server.RESTAddr
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		t.Fatalf("example rest_addr %q is not a dialable host:port — clients cannot derive "+
			"a registry URL from it (QA-BUNKER-74)", addr)
	}
	if ip := net.ParseIP(host); ip == nil || ip.IsLoopback() {
		t.Fatalf("example rest_addr %q is not a reachable bind (got host %q) — the old "+
			"loopback literal is the QA-BUNKER-74 silent-unreachable class; want the "+
			"tailnet placeholder %s", addr, host, parityExampleRESTAddr)
	}
	return addr
}

// TestClientRegistryURLMatchesExampleRESTAddr pins the storage leg: the
// documented `bunker connect http://<rest_addr>` registry entry survives a
// save/load round trip verbatim — no path rewrites the dial address, so a
// non-loopback rest_addr never decays back to a loopback default on disk.
func TestClientRegistryURLMatchesExampleRESTAddr(t *testing.T) {
	restAddr := parityRESTAddr(t)

	t.Setenv("BUNKER_HOME", t.TempDir())
	want := ServerEntry{
		Name:  "parity-host",
		URL:   "http://" + restAddr,
		Token: "parity-token",
	}
	if err := SaveCLIConfig(&CLIConfig{
		Servers:      map[string]ServerEntry{want.Name: want},
		ActiveServer: want.Name,
	}); err != nil {
		t.Fatalf("save CLI config: %v", err)
	}

	loaded, err := LoadCLIConfig()
	if err != nil {
		t.Fatalf("load CLI config: %v", err)
	}
	got, ok := loaded.Servers["parity-host"]
	if !ok {
		t.Fatalf("registry lost the parity entry (servers: %d)", len(loaded.Servers))
	}
	if got.URL != want.URL {
		t.Errorf("registry URL = %q, want %q — the client storage path must keep the "+
			"rest_addr-derived dial address verbatim, not rewrite it toward a loopback "+
			"default (QA-BUNKER-74)", got.URL, want.URL)
	}
}

// TestClientDialReachesRESTAddr is the live-dial leg: a server bound exactly
// at the example's rest_addr answers a dial through the production client
// path (newBunkerdClientChecked → ServerInfo). This is the row's
// "a dial to a spawned agent's rest_addr must succeed, not silently time
// out" cross-check, expressed against the local harness; on a fleet host
// with tailscale up the placeholder is directly bindable and the leg runs
// against it verbatim.
func TestClientDialReachesRESTAddr(t *testing.T) {
	restAddr := parityRESTAddr(t)

	mock := &mockBunkerdServer{info: &v1.ServerInfoResponse{
		Hostname:  "restaddr-parity",
		Version:   "test",
		MaxAgents: 1,
	}}
	router := chi.NewRouter()
	path, handler := bunkerv1connect.NewBunkerdHandler(mock)
	router.Mount(path, handler)

	// Leg 1 — the parity proof: bind exactly rest_addr. Only a tailnet host
	// (or one with the placeholder routed) can do this.
	if ln, err := net.Listen("tcp", restAddr); err == nil {
		srv := &http.Server{Handler: router, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Close() })

		entry := ServerEntry{Name: "parity-live", URL: "http://" + restAddr, Token: "parity-token"}
		assertDialAnswers(t, entry, mock.info.Hostname)
		return
	}

	// Leg 2 — the mechanics proof on hosts without the placeholder: same
	// production dial path, real HTTP server, loopback listener. The
	// entry-URL-following property is what matters: the client dials the
	// ENTRY, and if a hidden loopback default ever reappeared on the dial
	// path, this leg's random port would miss it and the RPC would fail.
	srv := &http.Server{Handler: router, ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen loopback: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	entry := ServerEntry{Name: "parity-mechanics", URL: "http://" + ln.Addr().String(), Token: "parity-token"}
	assertDialAnswers(t, entry, mock.info.Hostname)
	t.Logf("placeholder %s not bindable on this host — dial mechanics proven on %s; the "+
		"rest_addr-verbatim leg runs on tailnet hosts (QA-BUNKER-74)", restAddr, entry.URL)
}

// assertDialAnswers dials entry.URL through the production client constructor
// and requires the connect RPC to reach the mounted mock.
func assertDialAnswers(t *testing.T, entry ServerEntry, wantHostname string) {
	t.Helper()
	client, err := newBunkerdClientChecked(entry, 5*time.Second)
	if err != nil {
		t.Fatalf("build client for %s: %v", entry.URL, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := connect.NewRequest(&v1.ServerInfoRequest{})
	if entry.Token != "" {
		req.Header().Set("Authorization", "Bearer "+entry.Token)
	}
	resp, err := client.ServerInfo(ctx, req)
	if err != nil {
		t.Fatalf("dial %s: %v — a registry entry derived from the example's rest_addr must "+
			"reach the daemon, not fail with a transport error or silent timeout (QA-BUNKER-74)",
			entry.URL, err)
	}
	if resp.Msg.GetHostname() != wantHostname {
		t.Errorf("ServerInfo hostname = %q, want %q — the dial answered from an unexpected "+
			"server", resp.Msg.GetHostname(), wantHostname)
	}
}
