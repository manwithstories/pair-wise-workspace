package fixture

import "fmt"

type caseError struct{ name string }

func (e *caseError) Error() string {
	return "fixture: unknown case " + e.name
}

func errUnknownCase(name string) error { return &caseError{name: fmt.Sprint(name)} }
