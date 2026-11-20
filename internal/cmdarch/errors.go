package cmdarch

import "errors"

// UsageError maps to exit code 2: bad flags or unparseable parameters.
type UsageError struct{ Message string }

func (e *UsageError) Error() string { return e.Message }

// VerifyError maps to exit code 3: processing finished but independent
// verification rejected the output.
type VerifyError struct{ Message string }

func (e *VerifyError) Error() string { return e.Message }

// IOError maps to exit code 4: unrecoverable read or write failure.
type IOError struct{ Message string }

func (e *IOError) Error() string { return e.Message }

// ExitCodeFor maps an orchestration error onto a process exit code.
func ExitCodeFor(err error) ExitCode {
	var u *UsageError
	if errors.As(err, &u) {
		return ExitUsage
	}
	var v *VerifyError
	if errors.As(err, &v) {
		return ExitVerify
	}
	var i *IOError
	if errors.As(err, &i) {
		return ExitIO
	}
	return ExitOK
}
