package cli

// Shared flag grammar for the peel-style commands (`bunker exec`, `bunker
// run`, `bunker env`), introduced by DF-BUNKER-41/42 after exec's
// DF-BUNKER-31 grammar proved the shape. All three commands run with
// DisableFlagParsing (so trailing command/payload tokens pass through
// untouched) and peel their own flags manually:
//
//   - exec: the full exec flag set plus the root persistent flags, before
//     and after the agent-id, in the space and inline forms.
//   - run: the full run flag set plus the root persistent flags, before and
//     after the agent-id, in the space and inline forms (DF-BUNKER-41).
//   - env: --server/--timeout in EVERY position — before the subcommand,
//     between the subcommand and the agent-id, and after the payload — so
//     the payload validator sees exactly the documented tokens
//     (DF-BUNKER-42).
//
// With DisableFlagParsing cobra never parses even the root persistent flags
// for these commands, so a peeled --config / --daemon-config must be
// APPLIED through the cli setters (SetConfigPathOverride /
// SetDaemonConfigPathOverride), not merely accepted: the root
// PersistentPreRun transfer runs with an empty value and a peeled path
// would otherwise be silently ignored.

import (
	"fmt"
	"strings"
)

// flagGrammarSpec describes one flag a peel-style command accepts, in BOTH
// the space form (--flag value) and the inline form (--flag=value). A
// boolean flag takes no value; any other flag without one produces
// "flag needs an argument" in a strict peeler. apply stores the value (and
// may validate it): a non-nil error rejects the token locally.
type flagGrammarSpec struct {
	apply   func(value string) error
	boolean bool
}

// flagGrammar is one command's accepted-flag table plus the refusal wording
// used when a flag-like token is not in the table. The spec map is keyed by
// the full flag name INCLUDING the "--" prefix.
type flagGrammar struct {
	// name is the command name used in the default refusal wording.
	name string
	// refusePrefix overrides the default refusal wording; the offending
	// token is appended as ` (got "<arg>")`. Empty uses the exec-style
	// "<name> takes no flags before <agent-id>".
	refusePrefix string
	// specs is the single accepted-flag table for the command.
	specs map[string]flagGrammarSpec
}

func (g flagGrammar) refuseErr(arg string) error {
	if g.refusePrefix != "" {
		return fmt.Errorf("%s (got %q)", g.refusePrefix, arg)
	}
	return fmt.Errorf("%s takes no flags before <agent-id> (got %q)", g.name, arg)
}

// isFlagToken reports whether arg looks like a flag ("-"-prefixed, more than
// just "-"). A bare "-" is a stdin convention, never a flag.
func isFlagToken(arg string) bool {
	return strings.HasPrefix(arg, "-") && arg != "-"
}

type peelResult int

const (
	peelAccepted peelResult = iota
	peelUnknown             // token not in the spec table
	peelDangling            // value-taking flag with no value left
	peelInvalid             // apply rejected the value (err carries the message)
)

// peel consumes the flag token at args[i] (which must be flag-shaped): an
// accepted flag is applied via its spec in either the space form (--flag
// value) or the inline form (--flag=value). Returns the number of tokens
// consumed (2 for the space form, 1 otherwise) and the outcome.
func (g flagGrammar) peel(args []string, i int) (consumed int, result peelResult, err error) {
	arg := args[i]
	name, value, hasValue := strings.Cut(arg, "=")
	spec, ok := g.specs[name]
	if !ok {
		return 0, peelUnknown, nil
	}
	if !hasValue {
		if spec.boolean {
			value = "true"
		} else if i+1 < len(args) {
			value = args[i+1]
			consumed = 1
		} else {
			return 0, peelDangling, nil
		}
	}
	if aerr := spec.apply(value); aerr != nil {
		return 0, peelInvalid, aerr
	}
	return consumed + 1, peelAccepted, nil
}

// consume consumes one flag token in a strict peeler, converting the peel
// outcomes into the command's local errors. On success it returns the
// number of tokens consumed.
func (g flagGrammar) consume(args []string, i int) (int, error) {
	consumed, result, err := g.peel(args, i)
	switch result {
	case peelUnknown:
		return 0, g.refuseErr(args[i])
	case peelDangling:
		name, _, _ := strings.Cut(args[i], "=")
		return 0, fmt.Errorf("flag needs an argument: %s", name)
	case peelInvalid:
		return 0, err
	}
	return consumed, nil
}

// peelHead consumes flags from the head of args (STRICT): accepted flags are
// applied and consumed in both the space and inline forms; a "--" terminates
// flag parsing (the next token is never a flag); the first non-flag token
// ends the peel. An unknown flag-like token or a value-taking flag with no
// (valid) value refuses LOCALLY — before any config load or RPC — naming
// the token.
func (g flagGrammar) peelHead(args []string) ([]string, error) {
	head := 0
	for head < len(args) {
		arg := args[head]
		if arg == "--" {
			return args[head+1:], nil
		}
		if !isFlagToken(arg) || arg == "--help" || arg == "-h" {
			return args[head:], nil
		}
		consumed, err := g.consume(args, head)
		if err != nil {
			return nil, err
		}
		head += consumed
	}
	return args[head:], nil
}

// peelLeading consumes flags from the head of args (LENIENT): stops at the
// first token that is not an accepted flag and never errors — an unknown or
// dangling token belongs to what follows (the command or the payload) and is
// left untouched, so `docker run --rm` is never eaten.
func (g flagGrammar) peelLeading(args []string) []string {
	i := 0
	for i < len(args) {
		arg := args[i]
		if !isFlagToken(arg) {
			break
		}
		consumed, result, _ := g.peel(args, i)
		if result != peelAccepted {
			break // unknown/dangling token: part of the command, left untouched
		}
		i += consumed
	}
	return args[i:]
}

// peelAllFlags removes every accepted flag token from args, wherever it
// appears, keeping non-flag tokens in order (STRICT). A "--" terminates flag
// parsing: everything after it is payload, kept verbatim. An unknown
// flag-like token or a value-taking flag with no (valid) value refuses
// LOCALLY, naming the token.
func (g flagGrammar) peelAllFlags(args []string) ([]string, error) {
	out := make([]string, 0, len(args))
	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--" {
			out = append(out, args[i+1:]...)
			return out, nil
		}
		if !isFlagToken(arg) {
			out = append(out, arg)
			i++
			continue
		}
		consumed, err := g.consume(args, i)
		if err != nil {
			return nil, err
		}
		i += consumed
	}
	return out, nil
}
