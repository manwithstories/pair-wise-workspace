package media

import (
	"errors"
	"fmt"
	"strings"
)

// Faults is an ordered error collection. It implements error; the worst
// recovery grade determines the process exit code.
type Faults struct {
	List []Fault
}

// Add appends a fault.
func (fs *Faults) Add(f Fault) { fs.List = append(fs.List, f) }

// Addf appends a formatted fault.
func (fs *Faults) Addf(l Level, c Code, track uint64, off int64, format string, args ...any) {
	fs.Add(Fault{Level: l, Code: c, Track: track, Offset: off, Detail: fmt.Sprintf(format, args...)})
}

// Len reports the number of recorded faults.
func (fs *Faults) Len() int { return len(fs.List) }

// Empty reports whether no fault was recorded.
func (fs *Faults) Empty() bool { return len(fs.List) == 0 }

// MaxLevel returns the worst recovery grade seen, or 0 when empty.
func (fs *Faults) MaxLevel() Level {
	var max Level
	for _, f := range fs.List {
		if f.Level > max {
			max = f.Level
		}
	}
	return max
}

// Has reports whether any fault uses the given code.
func (fs *Faults) Has(c Code) bool {
	_, ok := fs.First(c)
	return ok
}

// First returns the first fault with the given code.
func (fs *Faults) First(c Code) (Fault, bool) {
	for _, f := range fs.List {
		if f.Code == c {
			return f, true
		}
	}
	return Fault{}, false
}

// Unwrap exposes each fault as an error so errors.Is can inspect codes.
func (fs *Faults) Unwrap() []error {
	out := make([]error, len(fs.List))
	for i, f := range fs.List {
		out[i] = f
	}
	return out
}

// Error renders every fault on one line.
func (fs *Faults) Error() string {
	if fs.Empty() {
		return "no faults"
	}
	parts := make([]string, len(fs.List))
	for i, f := range fs.List {
		parts[i] = f.String()
	}
	return strings.Join(parts, "; ")
}

// codeError is a comparable error wrapper around a Code so callers can write
// errors.Is(faults, media.CodeTruncatedVint.Err()).
type codeError Code

func (c codeError) Error() string { return string(c) }

// Err returns a sentinel error usable with errors.Is.
func (c Code) Err() error { return codeError(c) }

// Error renders the fault as a single diagnostic line.
func (f Fault) Error() string { return f.String() }

// levelError is a comparable error wrapper around a Level.
type levelError Level

func (l levelError) Error() string { return Level(l).String() }

// Err returns a sentinel error for errors.Is(fault, media.LevelFatal.Err()).
func (l Level) Err() error { return levelError(l) }

// Is supports errors.Is(fault, code.Err()) and level.Err().
func (f Fault) Is(target error) bool {
	switch t := target.(type) {
	case codeError:
		return f.Code == Code(t)
	case levelError:
		return f.Level == Level(t)
	}
	return false
}

// As lets errors.As extract a *Fault from a fault value.
func (f Fault) As(target any) bool {
	p, ok := target.(**Fault)
	if !ok {
		return false
	}
	c := f
	*p = &c
	return true
}

// Exit status codes of the command contract. ExitOK means a manifest exists;
// ExitCorrupt means the input is damaged and only diagnostics were produced;
// ExitInternal covers any other failure.
const (
	ExitOK       = 0
	ExitInternal = 1
	ExitCorrupt  = 2
)

// ExitCode maps an error to the command exit status.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var fs *Faults
	if errors.As(err, &fs) {
		if fs.MaxLevel() == LevelAbort {
			return ExitCorrupt
		}
		return ExitOK
	}
	return ExitInternal
}
