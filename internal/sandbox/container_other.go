//go:build !darwin && !linux

package sandbox

// newContainer returns the no-op backend on hosts with no supported sandbox
// runtime: CanContain is always false, so every call escalates to a human.
func newContainer() Container { return noopContainer{} }
