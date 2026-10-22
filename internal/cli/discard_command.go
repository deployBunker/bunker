package cli

import (
	"io"

	"github.com/spf13/cobra"
)

// discardCommand wraps io.Writer in a *cobra.Command shell so helpers that
// print through cmd.ErrOrStderr()/cmd.OutOrStdout() can be reused from paths
// that only own an io.Writer (TOOLS-003 spawn delivery reuses the installer's
// package stages without a real cobra command).
func discardCommand(errOut, out io.Writer) *cobra.Command {
	c := &cobra.Command{}
	c.SetErr(errOut)
	c.SetOut(out)
	return c
}
