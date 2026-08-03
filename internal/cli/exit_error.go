package cli

import "fmt"

// ExitError signals that the CLI process should terminate with a specific
// exit code instead of the default 1. It is used to propagate a remote
// command's exit code (bunker exec / bunker run) to the local shell,
// ssh-style: cmd/bunker/main.go matches it with errors.As and exits
// silently with the carried code, without printing "bunker: ..." noise.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("exit code %d", e.Code)
}
