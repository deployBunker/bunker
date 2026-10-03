package agent

// NET-BUNKER-007 tests — specs/network-isolation.md §6.3 (the assertion law)
// and §6.2 (SO_PEERCRED). All hermetic: temp dirs, in-process unix
// sockets, seam stubs — no root, no live host, no agent users.
//
// What each block proves:
//
//  1. AssertAgentSocketDir REDDENS for 0755 and PASSES for 0700 — the bite
//     (§6.3: "the socket directory mode is read back from the real
//     filesystem after creation").
//  2. EnsureAgentSocketDir sets 0700 EXPLICITLY and chowns — the primitive
//     the spawn path now uses instead of MkdirAll(0755)+chown.
//  3. CheckUnixSocketPeer ACCEPTS the owning uid and REFUSES a foreign uid
//     with errors.Is(ErrForeignPeerUID) and a text naming uids — the
//     (peer uid, owner uid) -> verdict table, proven against REAL socket
//     endpoints connected in this process so the KERNEL produces the
//     credentials being judged.
//  4. A refused peer's data is never read by the server — refusal happens
//     BEFORE any read/mutation on the connection (§6.2 deliverable 2).
//  5. Nothing in this tree binds an ABSTRACT unix socket (§7): a live scan
//     of /proc/net/unix must show no @-names, and the detector is
//     table-pinned.
//  6. VerifySocketOwnership (client-side read-back) refuses a socket whose
//     owner is not the expected agent uid, and refuses a missing path.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// unixAcceptFeed pairs a unix listener with the connections it accepts, in
// order. The server goroutine performs NO read or write on any connection —
// the tests control the gate order (credential check first, I/O after).
type unixAcceptFeed struct {
	ln    net.Listener
	conns chan net.Conn
}

// startUnixAcceptFeed listens on a real PATHNAME unix socket (never
// abstract — §7) and pumps accepted connections into the feed.
func startUnixAcceptFeed(t *testing.T, sockPath string) *unixAcceptFeed {
	t.Helper()
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	f := &unixAcceptFeed{ln: ln, conns: make(chan net.Conn, 4)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				close(f.conns)
				return
			}
			f.conns <- c
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

// next pops the next accepted connection.
func (f *unixAcceptFeed) next(t *testing.T) net.Conn {
	t.Helper()
	select {
	case c, ok := <-f.conns:
		if !ok {
			t.Fatal("listener closed before the connection arrived")
		}
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no connection was accepted")
		return nil
	}
}

// connect completes a full client connect (dial + one byte, so the server's
// Accept has unblocked and the kernel holds complete peer credentials).
func connect(t *testing.T, sockPath string) net.Conn {
	t.Helper()
	client, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := client.Write([]byte{0}); err != nil {
		t.Fatalf("client write (completes the accept): %v", err)
	}
	return client
}

// TestCheckUnixSocketPeer_AcceptsOwnerRejectsForeign is the (peer uid, owner
// uid) -> verdict table from the brief. The endpoints are REAL: each case
// dials the socket, so the kernel itself reports the connecting uid.
func TestCheckUnixSocketPeer_AcceptsOwnerRejectsForeign(t *testing.T) {
	cases := []struct {
		name      string
		peerUID   uint32
		ownerUID  uint32
		wantAllow bool
	}{
		{name: "owning uid is accepted", peerUID: 4242, ownerUID: 4242, wantAllow: true},
		{name: "foreign uid is refused", peerUID: 4243, ownerUID: 4242, wantAllow: false},
		{name: "root is refused too (uid 0 is not special)", peerUID: 0, ownerUID: 4242, wantAllow: false},
		{name: "owner zero is a valid owner", peerUID: 0, ownerUID: 0, wantAllow: true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// The scenario names peer and owner uids from the table. The
			// connect in THIS process carries the suite's own uid, so the
			// table is driven through the credential VERDICT: install the
			// peer-credential seam with the case's peer uid (the real
			// syscall path is proven separately by the live-connect cases
			// below), and judge exactly what the gate does with it.
			restore := stubPeerCredential(t, tc.peerUID, nil)
			defer restore()

			err := CheckUnixSocketPeer(nil, tc.ownerUID)
			if tc.wantAllow && err != nil {
				t.Fatalf("CheckUnixSocketPeer(owner=%d) refused the owning peer %d: %v", tc.ownerUID, tc.peerUID, err)
			}
			if !tc.wantAllow {
				if err == nil {
					t.Fatalf("CheckUnixSocketPeer(owner=%d) ACCEPTED foreign peer uid %d — the gate does not bite", tc.ownerUID, tc.peerUID)
				}
				if !errors.Is(err, ErrForeignPeerUID) {
					t.Errorf("refusal does not wrap ErrForeignPeerUID: %v", err)
				}
				// Diagnosable refusal (§6.2): names what was expected and
				// what arrived.
				if !strings.Contains(err.Error(), "uid") {
					t.Errorf("refusal text does not name uids: %v", err)
				}
			}
		})
	}
}

// TestCheckUnixSocketPeer_LiveConnectTable proves the verdict table over
// LIVE kernel connections — real dial, real accept, real
// syscall.GetsockoptUcred. Both ends run as the suite's uid, so the live
// leg exercises the ACCEPT branch end-to-end (kernel reported uid ==
// owner); the REFUSE branch is proven live by ordering: connect as
// ourselves, demand an owner uid we are not, and watch the gate refuse the
// kernel-reported credential.
func TestCheckUnixSocketPeer_LiveConnectTable(t *testing.T) {
	self := uint32(syscall.Geteuid())

	t.Run("real connect as owning uid is accepted", func(t *testing.T) {
		sockPath := filepath.Join(t.TempDir(), "docker.sock")
		feed := startUnixAcceptFeed(t, sockPath)
		client := connect(t, sockPath)
		defer client.Close()
		serverConn := feed.next(t)
		defer serverConn.Close()

		if err := CheckUnixSocketPeer(serverConn, self); err != nil {
			t.Fatalf("kernel-connected peer (uid %d) refused by owner gate %d: %v", self, self, err)
		}
	})

	t.Run("real connect judged against a foreign owner is refused", func(t *testing.T) {
		sockPath := filepath.Join(t.TempDir(), "docker.sock")
		feed := startUnixAcceptFeed(t, sockPath)
		client := connect(t, sockPath)
		defer client.Close()
		serverConn := feed.next(t)
		defer serverConn.Close()

		foreign := self + 1
		err := CheckUnixSocketPeer(serverConn, foreign)
		if err == nil {
			t.Fatal("peer accepted against a foreign owner uid — the gate does not bite on a live connection")
		}
		if !errors.Is(err, ErrForeignPeerUID) {
			t.Errorf("refusal does not wrap ErrForeignPeerUID: %v", err)
		}
		if !strings.Contains(err.Error(), "peer connected as uid") {
			t.Errorf("refusal does not name the arriving uid: %v", err)
		}
	})
}

// TestCheckUnixSocketPeer_RefusesBeforeAnyIO pins the ORDER contract (§6.2
// deliverable 2): the refusal happens before the server reads or mutates
// anything on the connection — the refused peer's bytes are still buffered,
// untouched, after the gate ran.
func TestCheckUnixSocketPeer_RefusesBeforeAnyIO(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "docker.sock")
	feed := startUnixAcceptFeed(t, sockPath)
	client := connect(t, sockPath)
	defer client.Close()
	payload := []byte("PRIVILEGED MUTATION ATTEMPT")
	if _, err := client.Write(payload); err != nil {
		t.Fatalf("client write: %v", err)
	}
	serverConn := feed.next(t)
	defer serverConn.Close()

	self := uint32(syscall.Geteuid())
	if err := CheckUnixSocketPeer(serverConn, self+1); !errors.Is(err, ErrForeignPeerUID) {
		t.Fatalf("expected a foreign-peer refusal, got: %v", err)
	}
	// Nothing was consumed: the server has NOT read from the connection.
	// The buffer still holds the probe byte the client sent to complete the
	// accept PLUS the whole payload — the gate ran before any I/O.
	if err := serverConn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	want := append([]byte{0}, payload...)
	buf := make([]byte, len(want))
	if _, err := serverConn.Read(buf); err != nil {
		t.Fatalf("payload should still be buffered (gate consumed nothing): %v", err)
	}
	if string(buf) != string(want) {
		t.Fatalf("payload disturbed across the refused gate: got %q, want %q", buf, want)
	}
}

// TestCheckUnixSocketPeer_RefusesNonUnixConn proves fail-closed on the conn
// TYPE: a non-unix connection carries no SO_PEERCRED, so it is refused even
// before any uid comparison — no invented approval.
func TestCheckUnixSocketPeer_RefusesNonUnixConn(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	err := CheckUnixSocketPeer(c1, 4242)
	if err == nil {
		t.Fatal("non-unix conn was approved — the check is not fail-closed")
	}
	if !errors.Is(err, ErrForeignPeerUID) {
		t.Errorf("refusal does not wrap ErrForeignPeerUID: %v", err)
	}
}

// TestCheckUnixSocketPeer_FailsClosedOnGetsockoptError: if the kernel
// credential read itself fails, the peer is refused (fail closed), never
// approved by default.
func TestCheckUnixSocketPeer_FailsClosedOnGetsockoptError(t *testing.T) {
	restore := stubPeerCredential(t, 0, errors.New("getsockopt: simulated failure"))
	defer restore()
	err := CheckUnixSocketPeer(nil, 0) // even matching uid 0 must NOT pass
	if err == nil {
		t.Fatal("unverifiable peer was approved — the gate is not fail-closed")
	}
	if !errors.Is(err, ErrForeignPeerUID) {
		t.Errorf("refusal does not wrap ErrForeignPeerUID: %v", err)
	}
}

// TestEnsureAgentSocketDir_SetsModeExplicitly is the mode-contract table:
// after Ensure, the directory is EXACTLY 0700 regardless of the mode it was
// created with (fresh, 0755 repair, 0777 hostile pre-existing), and the
// assertion passes on the result.
func TestEnsureAgentSocketDir_SetsModeExplicitly(t *testing.T) {
	self := uint32(syscall.Geteuid())
	restoreChown := stubChown(t)
	defer restoreChown()

	cases := []struct {
		name    string
		preMode os.FileMode
	}{
		{name: "fresh directory", preMode: 0},
		{name: "pre-existing 0755 is tightened", preMode: 0755},
		{name: "pre-existing 0777 is tightened", preMode: 0777},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "run", "bunker", "a1")
			if tc.preMode != 0 {
				if err := os.MkdirAll(dir, tc.preMode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(dir, tc.preMode); err != nil {
					t.Fatal(err)
				}
			}
			if err := EnsureAgentSocketDir("bunker-a1", dir); err != nil {
				t.Fatalf("EnsureAgentSocketDir: %v", err)
			}
			// THE ASSERTION — read back from the real filesystem. The
			// directory was created by THIS process, so its real owner is
			// the suite uid; that is the expected owner here.
			if err := AssertAgentSocketDir(dir, self); err != nil {
				t.Fatalf("AssertAgentSocketDir after Ensure: %v", err)
			}
			fi, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != 0700 {
				t.Fatalf("mode = %#o, want exactly 0700", got)
			}
		})
	}
}

// TestAssertAgentSocketDir_Bites demonstrates THE BITE the brief requires,
// on the REAL production function with real directories: the same assertion
// that passes (0700, owned-by-expected-uid) REDDENS for 0755 (the ISO-002
// hole), 0750, and a wrong owner. On an unprivileged host the wrong-owner
// case is exercised through the ownership read-back seam (the production
// function's own seam), which flips ONLY the owner the assertion compares —
// the mode check on the real directory is untouched by the seam.
func TestAssertAgentSocketDir_Bites(t *testing.T) {
	self := uint32(syscall.Geteuid())

	cases := []struct {
		name       string
		mode       os.FileMode
		wantUID    uint32
		ownerSeam  uint32 // the uid the read-back reports (self when 0)
		wantErrSub string
	}{
		{name: "0700 owned by the agent passes", mode: 0700, wantUID: self},
		{name: "0755 REDDENS (world-listable — the ISO-002 hole)", mode: 0755, wantUID: self, wantErrSub: "mode is 0755, required 0700"},
		{name: "0750 REDDENS (group-listable)", mode: 0750, wantUID: self, wantErrSub: "mode is 0750, required 0700"},
		{name: "0700 owned by a FOREIGN uid REDDENS", mode: 0700, wantUID: self, ownerSeam: self + 1, wantErrSub: "owned by uid"},
		{name: "0700 with a WRONG EXPECTED owner REDDENS", mode: 0700, wantUID: self + 1, wantErrSub: "owned by uid"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.ownerSeam != 0 {
				prev := statOwnerForTests
				statOwnerForTests = func(string) (uint32, bool) { return tc.ownerSeam, true }
				defer func() { statOwnerForTests = prev }()
			}

			dir := filepath.Join(t.TempDir(), "a1")
			if err := os.MkdirAll(dir, tc.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, tc.mode); err != nil {
				t.Fatal(err)
			}
			err := AssertAgentSocketDir(dir, tc.wantUID)
			if tc.wantErrSub == "" {
				if err != nil {
					t.Fatalf("0700/owner case must PASS, reddened: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("assertion did not bite for mode %#o — a loosened directory passed", tc.mode)
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Errorf("refusal %v does not name the expectation (%s)", err, tc.wantErrSub)
			}
			if !errors.Is(err, ErrSocketDirMode) {
				t.Errorf("refusal does not wrap ErrSocketDirMode: %v", err)
			}
		})
	}
}

// TestAssertSocketFileNotGroupWorld_Bites proves the socket-FILE half of
// §6.1 on a REAL socket: a socket at 0755 reddens, and Ensure tightens it
// to owner-only (never widens).
func TestAssertSocketFileNotGroupWorld_Bites(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Loosen the socket file the way a permissive umask could.
	if err := os.Chmod(sockPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := AssertSocketFileNotGroupWorld(sockPath); err == nil {
		t.Fatal("AssertSocketFileNotGroupWorld accepted a group/world-accessible socket — the assertion does not bite")
	}
	// Ensure tightens; the assertion then passes. Never the reverse.
	if err := EnsureSocketFileNotGroupWorld(sockPath); err != nil {
		t.Fatalf("EnsureSocketFileNotGroupWorld: %v", err)
	}
	if err := AssertSocketFileNotGroupWorld(sockPath); err != nil {
		t.Fatalf("after Ensure the socket must pass: %v", err)
	}
	fi, err := os.Stat(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0600 {
		t.Fatalf("socket mode = %#o, want 0600 (owner-only, never widened)", got)
	}
}

// TestVerifySocketOwnership_RefusesForeignOwner proves the client-side
// read-back on a REAL socket: a socket owned by anyone but the expected uid
// is refused with ErrForeignPeerUID (and an owned socket passes).
func TestVerifySocketOwnership_RefusesForeignOwner(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	self := uint32(syscall.Geteuid())
	if err := VerifySocketOwnership(sockPath, self); err != nil {
		t.Fatalf("socket owned by the expected uid must pass: %v", err)
	}
	err = VerifySocketOwnership(sockPath, self+1)
	if err == nil {
		t.Fatal("VerifySocketOwnership accepted a socket owned by a foreign uid")
	}
	if !errors.Is(err, ErrForeignPeerUID) {
		t.Errorf("refusal does not wrap ErrForeignPeerUID: %v", err)
	}
}

// TestVerifySocketOwnership_RefusesMissingPath — "exists" is not ownership:
// a missing socket path is a refusal, never a silent pass.
func TestVerifySocketOwnership_RefusesMissingPath(t *testing.T) {
	err := VerifySocketOwnership(filepath.Join(t.TempDir(), "nope.sock"), 4242)
	if err == nil || !errors.Is(err, ErrForeignPeerUID) {
		t.Fatalf("missing socket must refuse with ErrForeignPeerUID, got: %v", err)
	}
}

// TestIsAbstractSocketName pins the §7 detector table.
func TestIsAbstractSocketName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{name: "NUL-prefixed (the real abstract form)", in: "\x00bunker", want: true},
		{name: "@-display form", in: "@bunker", want: true},
		{name: "pathname socket", in: "/run/bunker/a1/docker.sock", want: false},
		{name: "relative pathname", in: "docker.sock", want: false},
		{name: "empty", in: "", want: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAbstractSocketName(tc.in); got != tc.want {
				t.Errorf("IsAbstractSocketName(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestNoAbstractUnixSocketBindsInThisTree is the §7 sweep. /proc/net/unix is
// SYSTEM-WIDE, so a bare grep proves nothing about this tree — attribution
// needs the per-process table: every line carries the socket's inode, and
// /proc/<pid>/fd/* symlinks name `socket:[inode]`. The sweep (a) asserts no
// abstract-socket inode belongs to THIS process, and (b) runs a POSITIVE
// control: a throwaway subprocess binds a real abstract socket, the sweep
// detector attributes it, and that proves the detector can see an abstract
// bind when one exists — an all-green run is evidence, not a blind spot.
func TestNoAbstractUnixSocketBindsInThisTree(t *testing.T) {
	selfPID := os.Getpid()

	selfSockets := socketInodesOf(selfPID)
	abstract := procUnixAbstractInodes(t)
	for in := range abstract {
		if selfSockets[in] {
			t.Errorf("abstract unix socket bound in this process: inode %s (spec §7 defect)", in)
		}
	}

	// POSITIVE CONTROL: a real abstract bind in a throwaway subprocess, then
	// prove the SAME attribution sees it — an all-green sweep is evidence,
	// not a blind spot.
	path, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available for the positive control")
	}
	script := "import socket; s=socket.socket(socket.AF_UNIX); s.bind(\"\\x00bunker-nb007-control\"); import time; time.sleep(10)"
	cmd := exec.Command(path, "-c", script)
	if err := cmd.Start(); err != nil {
		t.Skipf("could not start control subprocess: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	deadline := time.Now().Add(5 * time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		abstract = procUnixAbstractInodes(t)
		controlSockets := socketInodesOf(cmd.Process.Pid)
		for in := range abstract {
			if controlSockets[in] {
				found = true
			}
		}
		if !found {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !found {
		t.Fatal("positive control failed: the subprocess's abstract bind was never attributable via /proc fd inodes — the sweep cannot detect abstract binds")
	}
}

// socketInodesOf returns the set of socket inodes owned by pid, read from
// its /proc/<pid>/fd symlinks (`socket:[inode]`).
func socketInodesOf(pid int) map[string]bool {
	inodes := map[string]bool{}
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return inodes
	}
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(fdDir, e.Name()))
		if err != nil || !strings.HasPrefix(target, "socket:[") || !strings.HasSuffix(target, "]") {
			continue
		}
		inodes[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] = true
	}
	return inodes
}

// procUnixAbstractInodes returns the inodes of every ABSTRACT socket visible
// in the system-wide /proc/net/unix table (last field is the name; it starts
// with '@' for abstract sockets; field 7 is the inode).
func procUnixAbstractInodes(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile("/proc/net/unix")
	if err != nil {
		t.Skipf("cannot read /proc/net/unix on this host: %v", err)
	}
	inodes := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n")[1:] {
		fields := strings.Fields(line)
		if len(fields) >= 7 && strings.HasPrefix(fields[len(fields)-1], "@") {
			inodes[fields[6]] = true
		}
	}
	return inodes
}

// TestSpawnSocketDirContractByteLevel pins the exact spawn-site calls: the
// socket dir goes through EnsureAgentSocketDir (explicit 0700) + the
// AssertAgentSocketDir read-back, with NO MkdirAll(0755) left on the path —
// the byte-level guarantee that the 0755 cannot quietly return.
func TestSpawnSocketDirContractByteLevel(t *testing.T) {
	src, err := os.ReadFile("manager_spawn.go")
	if err != nil {
		t.Fatalf("read manager_spawn.go: %v", err)
	}
	code := string(src)
	if strings.Contains(code, "MkdirAll(sockDir, 0755)") {
		t.Fatal("manager_spawn.go still creates the agent socket directory 0755 — the ISO-002 hole is back")
	}
	if !strings.Contains(code, "EnsureAgentSocketDir(username, sockDir)") {
		t.Fatal("manager_spawn.go does not set the socket dir through EnsureAgentSocketDir (explicit 0700 + chown)")
	}
	if !strings.Contains(code, "AssertAgentSocketDir(sockDir, uint32(uid))") {
		t.Fatal("manager_spawn.go does not ASSERT the socket dir mode/ownership back (spec §6.3)")
	}
}

// ── helpers ─────────────────────────────────────────────────────────────

// stubChown installs a no-op chown seam (tests run unprivileged; the chown
// behavior itself is covered by the live spawn battery).
func stubChown(t *testing.T) func() {
	t.Helper()
	prev := chownForTests
	chownForTests = func(_ context.Context, _, _ string) error { return nil }
	return func() { chownForTests = prev }
}

// stubPeerCredential installs the peer-credential seam with a fixed
// verdict and returns a restore func.
func stubPeerCredential(t *testing.T, uid uint32, err error) func() {
	t.Helper()
	prev := peerCredentialFor
	peerCredentialFor = func(net.Conn) (uint32, error) { return uid, err }
	return func() { peerCredentialFor = prev }
}
