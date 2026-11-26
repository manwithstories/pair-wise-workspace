package main

import (
	"strconv"

	"mediamix/internal/clock"
)

// inputError is the locatable error type used for argument validation.
type inputError struct{ msg string }

func (e *inputError) Error() string { return e.msg }

func usageError(msg string) error {
	return &inputError{msg: msg}
}

func parseInt(s string) (int, error) { return strconv.Atoi(s) }

func parseFloat(s string) (float64, error) { return strconv.ParseFloat(s, 64) }

// asMediaError wraps an input error in the shared clock error model so it
// carries the invalid_param code and is easy to locate in logs.
func asMediaError(err error) *clock.MediaError {
	return clock.NewError(clock.StageCommit, clock.ErrInvalidParam, 0, false, err.Error())
}
