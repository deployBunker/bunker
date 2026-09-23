// Package mountdriver is the mount driver seam (MOUNT-006).
//
// Before this seam the mount command was one opaque, sshfs-shaped string
// produced at spawn and executed after strings.Fields on the client, with
// every retry/classification constant sshfs-specific and unattributed. The
// seam keeps that sshfs path BYTE-IDENTICAL for default requests and adds:
//
//   - a named driver identity (proto MountSpec.driver, default "sshfs");
//   - server-side driver selection on spawn, refusing unknown names;
//   - per-driver failure classification on the client, with an EXPLICIT,
//     test-checked declaration when a driver has no classifier.
//
// Only the sshfs driver is registered here today. rclone and FUSE/io_uring
// drivers arrive in later board rows; the seam is the deliverable.
package mountdriver

import (
	"fmt"
	"sort"
)

// DefaultDriver is the mount driver every spawn/mount uses when the request
// does not name one. The legacy opaque sshfs_mount string keeps its proto
// field and semantics, so an older client mounts via this driver unchanged.
const DefaultDriver = "sshfs"

// ClassifierClass is the failure class a driver's classifier reports.
// "permanent" means retrying cannot help; "transient" means a retry is
// worthwhile; "unknown" means the output carried no recognised signal. The
// values and precedence match the sshfs classifier that predates the seam.
type ClassifierClass string

const (
	ClassPermanent ClassifierClass = "permanent"
	ClassTransient ClassifierClass = "transient"
	ClassUnknown   ClassifierClass = "unknown"
)

// FailureClassifier turns one failed mount attempt's combined output and
// process error into a class plus a short human-readable reason. The class
// decides retry: only "transient" is retried by the mount loop.
type FailureClassifier func(output string, err error) (class ClassifierClass, reason string)

// NoClassifierReason explains WHY a registered driver has no failure
// classifier. The registry invariant (asserted by the seam's registry test)
// is that every driver either carries a classifier or declares its absence
// with a named reason — a driver must never silently inherit sshfs-shaped
// durability logic, nor silently skip it.
type NoClassifierReason struct {
	// Why is the operator-readable explanation (shown in `bunker mount`
	// diagnostics and asserted non-empty by the registry test).
	Why string
}

// Driver is one registered mount-driver implementation.
type Driver struct {
	// Name is the driver identity carried in proto MountSpec.driver and
	// selected on spawn/mount. Unique across the registry.
	Name string

	// Classify, when non-nil, classifies a failed attempt for the bounded
	// retry loop. Nil is allowed ONLY with a non-empty NoClassifier.
	Classify FailureClassifier

	// NoClassifier declares the deliberate absence of a failure classifier.
	// The mount path must then fail fast on the first attempt error (no
	// retries, no sshfs-shaped classification) and can explain why.
	NoClassifier *NoClassifierReason
}

// Validate enforces the registry invariant: a driver either provides a
// failure classifier or explicitly declares its absence with a reason.
func (d Driver) Validate() error {
	if d.Name == "" {
		return fmt.Errorf("mountdriver: driver must have a name")
	}
	switch {
	case d.Classify != nil && d.NoClassifier != nil:
		return fmt.Errorf("mountdriver: driver %q provides both a failure classifier and a no-classifier declaration; pick one", d.Name)
	case d.Classify == nil && d.NoClassifier == nil:
		return fmt.Errorf("mountdriver: driver %q provides neither a failure classifier nor a no-classifier declaration — declare one explicitly", d.Name)
	}
	return nil
}

// registry is the process-wide driver set. Populated by Register at package
// init; read-only after init, so no mutex is needed.
var registry = map[string]Driver{}

// ErrUnknownDriver is wrapped by every unknown-driver refusal so callers can
// match on the cause, and every refusal text names the offending driver.
type unknownDriverError struct{ name string }

func (e *unknownDriverError) Error() string {
	return fmt.Sprintf("unknown mount driver %q (registered drivers: %s)", e.name, RegisteredNames())
}

// Is makes every unknown-driver refusal match the ErrUnknownDriver sentinel
// regardless of which driver name it carries.
func (e *unknownDriverError) Is(target error) bool {
	_, ok := target.(*unknownDriverError)
	return ok
}

// ErrUnknownDriver is the sentinel callers can errors.Is against; it wraps
// the named refusal produced by Resolve.
var ErrUnknownDriver = &unknownDriverError{}

// Register adds a driver to the registry. It panics on a duplicate name or a
// driver violating the classifier invariant: registration happens at package
// init, so a violation is a programmer error, not a runtime condition.
func Register(d Driver) {
	if err := d.Validate(); err != nil {
		panic(err)
	}
	if _, dup := registry[d.Name]; dup {
		panic(fmt.Sprintf("mountdriver: driver %q registered twice", d.Name))
	}
	registry[d.Name] = d
}

// Resolve returns the registered driver with the given name. An unknown name
// is a named error (errors.Is(err, ErrUnknownDriver)) — the refusal the
// server maps to CodeInvalidArgument and the CLI surfaces loudly; it is
// NEVER a silent fallback to the default driver.
func Resolve(name string) (Driver, error) {
	if name == "" {
		name = DefaultDriver
	}
	d, ok := registry[name]
	if !ok {
		return Driver{}, &unknownDriverError{name: name}
	}
	return d, nil
}

// Known reports whether name is a registered driver (or empty = default).
func Known(name string) bool {
	_, err := Resolve(name)
	return err == nil
}

// RegisteredNames returns every registered driver name, sorted for stable
// error text and test output.
func RegisteredNames() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// RegistryInvariantError describes a registered driver that violates the
// classifier invariant — impossible via Register, but the test walks the
// registry through this check so the invariant is executable, not prose.
func RegistryInvariantError() error {
	for _, d := range registry {
		if err := d.Validate(); err != nil {
			return err
		}
	}
	return nil
}
