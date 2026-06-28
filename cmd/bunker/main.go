// bunker — CLI for managing Bunker agent hosts
//
// Three-tier CLI:
//
//	bunker infra ...    — manage servers, deploy bunkerd instances
//	bunker host ...     — manage agents on a connected server
//	bunker agent ...    — scoped to single agent (customer-facing)
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "bunker: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	fmt.Println("bunker v0.1.0 — CLI")
	// TODO: WI-011 through WI-016 — full CLI surface
	return nil
}
