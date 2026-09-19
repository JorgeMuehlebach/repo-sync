package gitops

import "fmt"

// mirrorPointerBackend implements the one mutable object in a mirror: the
// stable path used by readers. Generations themselves are immutable once they
// have been exposed.
type mirrorPointerBackend interface {
	Resolve(exposed string) (string, error)
	Create(exposed, target string) error
	Replace(exposed, expected, next string) error
}

// mirrorAtomicPointerError distinguishes lack of a proven atomic replacement
// primitive from ordinary transaction I/O failures. Callers must fail closed;
// replacing a directory pointer with a remove/create sequence is never safe.
type mirrorAtomicPointerError struct {
	err error
}

func (e *mirrorAtomicPointerError) Error() string {
	return fmt.Sprintf("atomic mirror pointer replacement: %v", e.err)
}

func (e *mirrorAtomicPointerError) Unwrap() error { return e.err }
