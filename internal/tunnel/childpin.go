package tunnel

import (
	"os/exec"
	"runtime"
	"sync"
)

// childPin keeps the OS thread that created a long-lived child alive for as
// long as that child lives, so the kernel parent-death backstop stays armed.
//
// WHY (measured, GAP-084): Pdeathsig is delivered when the THREAD that created
// the child exits — prctl(2) PR_SET_PDEATHSIG is explicit, "the signal will be
// sent when that thread terminates ... rather than after all of the threads in
// the parent process terminate". Go multiplexes goroutines over OS threads
// whose lifetime is NOT tied to the process, so a cloudflared started from an
// RPC handler goroutine and armed with Pdeathsig but no pin would be SIGKILLed
// at a random moment — whenever that M is torn down — instead of when bunkerd
// dies. Same reasoning and same shape as the CLI's startLongLivedChild
// (internal/cli/proc.go, GAP-079).
type childPin struct {
	done chan struct{}
	once sync.Once
}

// newChildPin returns a pin for one child.
func newChildPin() *childPin {
	return &childPin{done: make(chan struct{})}
}

// start starts cmd from a pinned OS thread that then parks until release is
// called. Start's error is propagated the same way cmd.Start propagates it;
// Wait stays with the caller (stopTunnelCommand).
func (p *childPin) start(cmd *exec.Cmd) error {
	started := make(chan error, 1)

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		started <- nil

		<-p.done
	}()

	return <-started
}

// release unparks the pinned thread. It is idempotent and nil-safe, so a stop
// path can call it unconditionally — including on the failure paths where no
// child was ever started, which would otherwise leak the pin's goroutine and
// its OS thread.
func (p *childPin) release() {
	if p == nil {
		return
	}
	p.once.Do(func() { close(p.done) })
}
