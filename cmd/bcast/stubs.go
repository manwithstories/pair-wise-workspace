package main

import "bcast/internal/fault"

func runVerify(args []string) error {
	return fault.E(fault.Internal, "cli", "verify", errNotImplemented)
}

var errNotImplemented = cliStringError("not implemented")

type cliStringError string

func (e cliStringError) Error() string { return string(e) }
