package main

import (
	"fmt"

	"github.com/deployBunker/bunker/internal/config"
)

// runPortPoolCheck (FLEET-INFRA-1) prints the resolved port-pool geometry and
// the config.Validate() verdict — the same Validate a boot runs through
// server.New — then exits. It is a CONFIG diagnostic: it loads the same
// config file a boot would (flag → BUNKERD_CONFIG → default) and reports,
// without binding a port, touching the running daemon, or reading live
// allocator state (allocator memory exists only inside the running bunkerd
// process; the in-daemon drift sweep is what heals live leaks).
//
// The relation verdict itself lives in config.Validate (agent.max_agents vs
// pool capacity), so this command cannot drift from what a boot enforces: it
// renders that check, it is not a second implementation of it.
//
// Exit 0 = the config would boot with a healthy pool relation; exit 1 = the
// boot would refuse (load or validate error — the error text carries the
// remediation).
func runPortPoolCheck(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	a := &cfg.Agent

	fmt.Printf("port-pool check (FLEET-INFRA-1)\n")
	fmt.Printf("  config:     %s\n", cfgPath)
	fmt.Printf("  pool:       %d-%d\n", a.PortRangeStart, a.PortRangeEnd)
	fmt.Printf("  per-agent:  %d ports\n", a.PortRangePerAgent)
	fmt.Printf("  max_agents: %d\n", a.MaxAgents)
	if geometryOK := a.PortRangeStart < a.PortRangeEnd && a.PortRangePerAgent > 0; geometryOK {
		totalPorts := uint64(a.PortRangeEnd) - uint64(a.PortRangeStart) + 1
		fmt.Printf("  capacity:   %d ranges (%d ports / %d per agent)\n",
			totalPorts/uint64(a.PortRangePerAgent), totalPorts, a.PortRangePerAgent)
	} else {
		fmt.Printf("  capacity:   n/a (invalid geometry — the allocator is disabled at boot)\n")
	}
	if err := cfg.Validate(); err != nil {
		fmt.Printf("  verdict:    BOOT REFUSED — %v\n", err)
		return err
	}
	fmt.Printf("  verdict:    OK (config.Validate passed: max_agents fits the pool capacity)\n")
	return nil
}
