// Package fault implements fault atoms (故障执行层) with a uniform
// lifecycle so the experiment scheduler can treat them identically:
//
//	Check()   -> environment awareness: platform / tool / permission gates
//	Inject()  -> execute the fault
//	Recover() -> idempotent rollback; must never fail, even on platforms
//	            where the fault could never have been injected
package fault

import "fmt"

// FaultError wraps a rejected injection or recovery attempt.
type FaultError struct{ msg string }

func (e *FaultError) Error() string { return e.msg }

// Errf builds a *FaultError from a format string.
func Errf(format string, args ...any) error {
	return &FaultError{msg: fmt.Sprintf(format, args...)}
}

// Fault is the common contract for all fault atoms.
type Fault interface {
	Name() string
	Description() string
	// Check validates that the fault can run in this environment.
	Check() error
	// Inject executes the fault. Safe to call once per experiment.
	Inject() error
	// Recover rolls the fault back. MUST be idempotent and never raise.
	Recover() error
	// Describe renders the fault and its parameters.
	Describe() string
}

// Factory builds a fault atom with default parameters; callers then set
// concrete fields before injection.
type Factory func() Fault

// Registry maps fault atom names to their factories.
var Registry = map[string]Factory{}

// Register adds a fault atom factory to the registry.
func Register(name string, f Factory) {
	Registry[name] = f
}
