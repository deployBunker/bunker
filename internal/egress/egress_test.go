package egress

import (
	"errors"
	"strings"
	"testing"
)

func TestResolveVocabulary(t *testing.T) {
	tests := []struct {
		name    string
		request string
		want    string
		wantErr bool
	}{
		{"empty defers to default", "", ModeOpen, false},
		{"whitespace-only is not unset", "   ", "", true},
		{"open accepted", "open", ModeOpen, false},
		{"allowlist accepted", "allowlist", ModeAllowlist, false},
		{"none accepted", "none", ModeNone, false},
		{"surrounding whitespace trimmed", "  none  ", ModeNone, false},
		{"unknown refuses", "block-everything", "", true},
		{"case sensitive", "Open", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(tt.request)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Resolve(%q) = %q, want error", tt.request, got)
				}
				if !strings.Contains(err.Error(), "valid:") {
					t.Errorf("error %q does not name the valid set", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tt.request, err)
			}
			if got != tt.want {
				t.Errorf("Resolve(%q) = %q, want %q", tt.request, got, tt.want)
			}
		})
	}
}

func TestChainNamePerUID(t *testing.T) {
	tests := []struct {
		uid      uint32
		nftName  string
		iptName  string
		parseOK  bool
		parseUID uint32
	}{
		{1000, "bunker-egress-1000", "BUNKER-EGRESS-1000", true, 1000},
		{0, "bunker-egress-0", "BUNKER-EGRESS-0", true, 0},
		{4294967295, "bunker-egress-4294967295", "BUNKER-EGRESS-4294967295", true, 4294967295},
	}
	for _, tt := range tests {
		if got := ChainName(tt.uid); got != tt.nftName {
			t.Errorf("ChainName(%d) = %q, want %q", tt.uid, got, tt.nftName)
		}
		if got := IPTablesChainName(tt.uid); got != tt.iptName {
			t.Errorf("IPTablesChainName(%d) = %q, want %q", tt.uid, got, tt.iptName)
		}
		uid, ok := ParseChainUID(tt.nftName)
		if !ok || uid != tt.parseUID {
			t.Errorf("ParseChainUID(%q) = %d,%v want %d,true", tt.nftName, uid, ok, tt.parseUID)
		}
		uid, ok = ParseChainUID(tt.iptName)
		if !ok || uid != tt.parseUID {
			t.Errorf("ParseChainUID(%q) = %d,%v want %d,true", tt.iptName, uid, ok, tt.parseUID)
		}
	}
}

func TestParseChainUIDRejects(t *testing.T) {
	tests := []string{
		"",
		"bunker-egress-",
		"BUNKER-EGRESS-",
		"DOCKER-USER",
		"bunker-egress-12a",
		"bunker-egress-12x",
		"bunker-egress-99999999999", // overflows uint32
		"bunker-egress--1",
		"INPUT",
		"bunker_output_hook",
		// lookalike prefixes that are not exactly ours
		"bunker-egress2-100",
		"XBUNKER-EGRESS-100",
	}
	for _, name := range tests {
		if _, ok := ParseChainUID(name); ok {
			t.Errorf("ParseChainUID(%q) accepted, want rejection", name)
		}
	}
}

func TestInstallOpenModeZeroFirewallCalls(t *testing.T) {
	// Requirement 4 + 5: open mode performs ZERO firewall helper invocations.
	// The executor records every invocation; the manager's PATH probes are
	// also pinned to "would fail" so even a backend resolution cannot fire.
	exec := &RecordingExecutor{}
	prevNFT, prevIPT := nftBinaryPresent, iptablesBinaryPresent
	nftBinaryPresent, iptablesBinaryPresent = func() bool { return true }, func() bool { return true }
	defer func() { nftBinaryPresent, iptablesBinaryPresent = prevNFT, prevIPT }()

	m := NewManagerWith(exec, StaticResolver{})
	// The empty REQUESTED mode resolves to open at the config layer; the
	// manager-level contract under test is that Install(open) touches
	// nothing. Resolve("") first, then install what it produced.
	for _, requested := range []string{ModeOpen, ""} {
		resolved, rerr := Resolve(requested)
		if rerr != nil {
			t.Fatalf("Resolve(%q): %v", requested, rerr)
		}
		if resolved != ModeOpen {
			t.Fatalf("Resolve(%q) = %q, want open", requested, resolved)
		}
		if err := m.Install(ModePolicy{Mode: resolved}, 1000); err != nil {
			t.Fatalf("Install(open): %v", err)
		}
	}
	if exec.Count() != 0 {
		t.Fatalf("open mode invoked the firewall executor %d times, want 0: %+v", exec.Count(), exec.Calls)
	}
	// The MANAGER-level Sweep is mechanical by contract (the agent layer
	// owns the open-mode gate); here we pin only that a nil-live-set sweep
	// against an empty listing removes nothing.
	emptyExec := &listingExecutor{inner: &RecordingExecutor{}, listing: []byte("table ip " + EgressTableName + " {\n	chain " + HookChainName + " {\n	}\n}\n")}
	m2 := NewManagerWith(emptyExec, StaticResolver{})
	removed, err := m2.Sweep(map[uint32]bool{})
	if err != nil {
		t.Fatalf("Sweep(open): %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("Sweep removed %v from a chainless listing", removed)
	}
}

func TestInstallAllowlistNFTRuleSet(t *testing.T) {
	exec := &RecordingExecutor{}

	m := NewManagerWith(exec, StaticResolver{
		"example.internal": {"93.184.216.34"},
		"gb.ubuntu.test":   {"2001:db8::1", "91.189.88.24"},
	})
	pol := ModePolicy{
		Mode:      ModeAllowlist,
		Allowlist: []string{"10.0.0.0/8", "example.internal", "gb.ubuntu.test"},
	}
	if err := m.Install(pol, 4242); err != nil {
		t.Fatalf("Install(allowlist): %v", err)
	}
	// The per-agent chain must carry the resolved accepts and the guards.
	var chainRules []string
	for _, c := range exec.Calls {
		// argv shape: nft add rule ip <table> <chain> <statement...>
		if len(c.Argv) >= 7 && c.Argv[1] == "add" && c.Argv[2] == "rule" && c.Argv[5] == ChainName(4242) {
			chainRules = append(chainRules, strings.Join(c.Argv[6:], " "))
		}
	}
	want := []string{
		"ip daddr 127.0.0.0/8 counter accept",
		"ct state invalid counter drop",
		"ct state established,related counter accept",
		"ip daddr 10.0.0.0/8 counter accept",
		"ip daddr 91.189.88.24/32 counter accept",
		"ip daddr 93.184.216.34/32 counter accept",
		"counter drop",
	}
	if strings.Join(chainRules, "|") != strings.Join(want, "|") {
		t.Errorf("chain rules:\n got %v\nwant %v", chainRules, want)
	}
	// The jump rule must carry the uid source match.
	var foundJump bool
	for _, c := range exec.Calls {
		if len(c.Argv) >= 7 && c.Argv[1] == "add" && c.Argv[2] == "rule" && c.Argv[5] == HookChainName {
			if strings.Join(c.Argv[6:], " ") == nftJumpStatement(4242) {
				foundJump = true
			}
		}
	}
	if !foundJump {
		t.Errorf("no jump rule for uid 4242 in %v", exec.Calls)
	}
}

func TestInstallAllowlistDomainsResolvedNotEmbedded(t *testing.T) {
	// Requirement 1: domains resolve at rule-install time — a hostname must
	// never appear verbatim inside a rule.
	exec := &RecordingExecutor{}
	m := NewManagerWith(exec, StaticResolver{"corp.example.com": {"198.51.100.7"}})
	if err := m.Install(ModePolicy{Mode: ModeAllowlist, Allowlist: []string{"corp.example.com"}}, 77); err != nil {
		t.Fatalf("Install: %v", err)
	}
	for _, c := range exec.Calls {
		if strings.Contains(strings.Join(c.Argv, " "), "corp.example.com") {
			t.Errorf("hostname leaked into rule argv: %v", c.Argv)
		}
	}
}

func TestInstallFailuresRefuseLoud(t *testing.T) {
	// Requirement 4: a failed install in allowlist/none mode is a returned
	// error — the spawn caller rolls the agent back.
	tests := []struct {
		name string
		pol  ModePolicy
		fail func(call int, argv []string) error
	}{
		{
			name: "allowlist nft add-rule fails",
			pol:  ModePolicy{Mode: ModeAllowlist, Allowlist: []string{"10.0.0.0/8"}},
			fail: func(call int, argv []string) error {
				if len(argv) > 2 && argv[1] == "add" && argv[2] == "rule" && call == 7 {
					return errors.New("boom")
				}
				return nil
			},
		},
		{
			name: "none chain add fails",
			pol:  ModePolicy{Mode: ModeNone},
			fail: func(call int, argv []string) error {
				if len(argv) > 2 && argv[1] == "add" && argv[2] == "chain" && strings.Contains(strings.Join(argv, " "), ChainName(5)) {
					return errors.New("boom")
				}
				return nil
			},
		},
		{
			name: "allowlist unresolvable domain",
			pol:  ModePolicy{Mode: ModeAllowlist, Allowlist: []string{"nope.invalid"}},
			fail: func(call int, argv []string) error { return nil },
		},
		{
			name: "no firewall binary at all",
			pol:  ModePolicy{Mode: ModeNone},
			fail: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := &RecordingExecutor{FailOn: tt.fail}
			prevNFT, prevIPT := nftBinaryPresent, iptablesBinaryPresent
			if tt.name == "no firewall binary at all" {
				nftBinaryPresent, iptablesBinaryPresent = func() bool { return false }, func() bool { return false }
			} else {
				nftBinaryPresent, iptablesBinaryPresent = func() bool { return true }, func() bool { return false }
			}
			defer func() { nftBinaryPresent, iptablesBinaryPresent = prevNFT, prevIPT }()
			m := NewManagerWith(exec, StaticResolver{})
			if err := m.Install(tt.pol, 5); err == nil {
				t.Fatalf("Install(%s) succeeded, want loud failure", tt.pol.Mode)
			}
		})
	}
}

func TestBackendFallbackToIPTables(t *testing.T) {
	// Requirement 1: iptables fallback when nft is absent. A FRESH manager
	// (unresolved backend choice) must pick iptables and emit iptables argv.
	prevNFT, prevIPT := nftBinaryPresent, iptablesBinaryPresent
	nftBinaryPresent, iptablesBinaryPresent = func() bool { return false }, func() bool { return true }
	defer func() { nftBinaryPresent, iptablesBinaryPresent = prevNFT, prevIPT }()

	exec := &RecordingExecutor{}
	m := NewManagerWith(exec, StaticResolver{})
	if err := m.Install(ModePolicy{Mode: ModeNone}, 99); err != nil {
		t.Fatalf("Install: %v", err)
	}
	sawIptables, sawNFT := false, false
	for _, c := range exec.Calls {
		switch c.Argv[0] {
		case "iptables":
			sawIptables = true
		case "nft":
			sawNFT = true
		}
	}
	if !sawIptables || sawNFT {
		t.Errorf("fallback argv wrong: iptables=%v nft=%v", sawIptables, sawNFT)
	}
	// The jump must be the uid source-match into the per-agent chain.
	var foundJump bool
	for _, c := range exec.Calls {
		if strings.Join(c.Argv, " ") == "iptables -I OUTPUT 1 "+iptablesJumpSpec(99) {
			foundJump = true
		}
	}
	if !foundJump {
		t.Errorf("no iptables OUTPUT jump for uid 99 in %v", exec.Calls)
	}
}

func TestDestroyRemovesChain(t *testing.T) {
	// Requirement 5: spawn adds + destroy removes (here: the Remove argv
	// sequence and its contract — jump handles first, then flush+delete).
	exec := &RecordingExecutor{}
	m := NewManagerWith(exec, StaticResolver{})
	// Pre-resolve the backend as nft by installing once.
	if err := m.Install(ModePolicy{Mode: ModeNone}, 55); err != nil {
		t.Fatalf("Install: %v", err)
	}
	exec.Calls = nil
	// The hook listing reports the jump handle 42.
	listing := "table ip " + EgressTableName + " {\n	chain " + HookChainName + " {\n		" +
		nftJumpStatement(55) + " # handle 42\n	}\n}\n"
	multiExec := &RecordingExecutor{}
	multi := NewManagerWith(&listingExecutor{inner: multiExec, listing: []byte(listing)}, StaticResolver{})
	if err := multi.Remove(55); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	var order []string
	for _, c := range multiExec.Calls {
		switch {
		case len(c.Argv) > 3 && (c.Argv[1] == "list" || c.Argv[1] == "-a") && c.Argv[2] == "list":
			order = append(order, "list")
		case len(c.Argv) > 2 && c.Argv[1] == "list":
			order = append(order, "list")
		case len(c.Argv) > 6 && c.Argv[1] == "delete" && c.Argv[2] == "rule":
			order = append(order, "delete-rule:"+c.Argv[7])
		case len(c.Argv) > 2 && c.Argv[1] == "flush":
			order = append(order, "flush")
		case len(c.Argv) > 2 && c.Argv[1] == "delete" && c.Argv[2] == "chain":
			order = append(order, "delete-chain")
		}
	}
	// Remove must list the hook FIRST (jump-handle discovery), then delete
	// the jump rule by handle, then flush + delete the chain.
	if len(order) != 4 || order[0] != "list" || order[1] != "delete-rule:42" || order[2] != "flush" || order[3] != "delete-chain" {
		t.Errorf("remove sequence wrong: %v", order)
	}
	// The shared hook chain and table must never be deleted by Remove.
	for _, c := range multiExec.Calls {
		joined := strings.Join(c.Argv, " ")
		if strings.Contains(joined, "delete table") {
			t.Errorf("Remove deletes the shared table: %v", c.Argv)
		}
	}
}

// listingExecutor serves a fixed listing for list-chain calls and delegates
// everything else.
type listingExecutor struct {
	inner   *RecordingExecutor
	listing []byte
}

// Run serves the listing for list-chain AND list-table calls (with or
// without the -a handle flag) — recording the served call in the inner
// executor so assertions see the FULL call sequence — else delegates.
func (l *listingExecutor) Run(argv []string) ([]byte, error) {
	if len(argv) > 3 && (argv[1] == "list" || argv[1] == "-a") && argv[2] == "list" && (argv[3] == "chain" || argv[3] == "table") {
		l.inner.Calls = append(l.inner.Calls, RecordedCall{Argv: append([]string(nil), argv...)})
		return l.listing, nil
	}
	if len(argv) > 2 && argv[1] == "list" && (argv[2] == "chain" || argv[2] == "table") {
		l.inner.Calls = append(l.inner.Calls, RecordedCall{Argv: append([]string(nil), argv...)})
		return l.listing, nil
	}
	return l.inner.Run(argv)
}

func TestSweepRemovesStaleAndKeepsLive(t *testing.T) {
	// Requirement 5: reconcile removes orphans and leaves live agents' chains.
	listing := "table ip " + EgressTableName + " {\n" +
		"\tchain " + HookChainName + " {\n\t}\n" +
		"\tchain " + ChainName(100) + " {\n\t}\n" +
		"\tchain " + ChainName(200) + " {\n\t}\n" +
		"\tchain DOCKER-USER {\n\t}\n" +
		"}\n"
	exec := &RecordingExecutor{}
	m := NewManagerWith(&listingExecutor{inner: exec, listing: []byte(listing)}, StaticResolver{})
	prevNFT, prevIPT := nftBinaryPresent, iptablesBinaryPresent
	nftBinaryPresent, iptablesBinaryPresent = func() bool { return true }, func() bool { return false }
	defer func() { nftBinaryPresent, iptablesBinaryPresent = prevNFT, prevIPT }()

	removed, err := m.Sweep(map[uint32]bool{200: true})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(removed) != 1 || removed[0].UID != 100 {
		t.Fatalf("removed %v, want exactly uid 100", removed)
	}
	// The stale chain was flushed + deleted; the live chain was never named.
	var deletedChain, flushedLive bool
	for _, c := range exec.Calls {
		joined := strings.Join(c.Argv, " ")
		if strings.Contains(joined, ChainName(100)) && strings.Contains(joined, "delete chain") {
			deletedChain = true
		}
		if strings.Contains(joined, ChainName(200)) {
			flushedLive = true
		}
	}
	if !deletedChain {
		t.Errorf("stale chain 100 not deleted in %v", exec.Calls)
	}
	if flushedLive {
		t.Errorf("live agent chain 200 was touched")
	}
}

func TestParseNFTJumpHandles(t *testing.T) {
	listing := "		meta skuid 55 jump bunker-egress-55 counter packets 5 bytes 400 # handle 7\n" +
		"		meta skuid 55 jump bunker-egress-55 counter packets 0 bytes 0 # handle 9\n" +
		"		meta skuid 42 jump bunker-egress-42 counter packets 0 bytes 0 # handle 11\n"
	got := ParseNFTJumpHandles([]byte(listing), 55)
	if len(got) != 2 || got[0] != "7" || got[1] != "9" {
		t.Errorf("ParseNFTJumpHandles = %v, want [7 9]", got)
	}
	if got := ParseNFTJumpHandles([]byte(listing), 56); len(got) != 0 {
		t.Errorf("unexpected handles for uid 56: %v", got)
	}
}

func TestConfigValidateRejectsBadAllowlistAndMode(t *testing.T) {
	// The egress grammar: CIDs/IPs/hostnames accepted; junk refused.
	bad := []string{"", "not a host", "exa_mple.com", "-lead.example.com", "10.0.0.0/64", "10.0.0.999", "http://example.com"}
	for _, entry := range bad {
		if err := ValidAllowlistEntry(entry); err == nil {
			t.Errorf("ValidAllowlistEntry(%q) accepted, want refusal", entry)
		}
	}
	good := []string{"10.0.0.0/8", "192.168.1.1", "example.com", "api.internal.example.co.uk", "2001:db8::/32", "::1"}
	for _, entry := range good {
		if err := ValidAllowlistEntry(entry); err != nil {
			t.Errorf("ValidAllowlistEntry(%q): %v", entry, err)
		}
	}
}

func TestBoundaryForNeverEmptyForKnownModes(t *testing.T) {
	for _, mode := range ValidModes() {
		if BoundaryFor(mode) == "" {
			t.Errorf("BoundaryFor(%q) is empty", mode)
		}
	}
	if BoundaryFor("unknown") != "" || BoundaryFor("") != "" {
		t.Errorf("unknown/empty modes must have no boundary string (the reporting law)")
	}
}

func TestNoneModeChainDeniesAllExceptControl(t *testing.T) {
	spec, err := buildChainSpec(ModeNone, 3, nil, StaticResolver{})
	if err != nil {
		t.Fatalf("buildChainSpec: %v", err)
	}
	stmts := spec.nftChainStatements()
	last := stmts[len(stmts)-1]
	if last != "counter drop" {
		t.Errorf("last statement = %q, want the default drop", last)
	}
	for _, s := range stmts {
		if strings.Contains(s, "accept") && !strings.Contains(s, "127.0.0.0/8") && !strings.Contains(s, "established,related") {
			t.Errorf("none mode accepts non-control traffic: %q", s)
		}
	}
}

func TestNFTHookDeclarationPinned(t *testing.T) {
	// The hook declaration is load-bearing (priority ahead of the standard
	// filter chain; policy accept) — pin it byte-for-byte.
	want := "table ip " + EgressTableName + " {\n" +
		"\tchain " + HookChainName + " {\n" +
		"\t\ttype filter hook output priority filter - 10; policy accept;\n" +
		"\t}\n" +
		"}\n"
	if NFTHookChainDecl != want {
		t.Errorf("NFTHookChainDecl drifted:\n got %q\nwant %q", NFTHookChainDecl, want)
	}
}
